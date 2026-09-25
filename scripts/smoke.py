#!/usr/bin/env python3
"""Optional Linux development smoke test. No production data or host mutations.
Run: python3 scripts/smoke.py dist/lightpanel-linux-amd64
Uses a disposable loopback server with a test-only password/hash fixture.
"""
import http.cookiejar
import json
import os
from pathlib import Path
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

PASSWORD = 'local-test-only-password'
TEST_HASH = '$2a$10$rsjl2q3/rV/bLuwfx4vf0OYE7r/n.0mbGv9vNwlRiTS5OD.akczYG'


def main():
    binary = Path(sys.argv[1]).resolve()
    with tempfile.TemporaryDirectory(prefix='lightpanel-smoke-') as temp:
        root = Path(temp)
        # Execute a fresh inode on Linux; WSL can cache stale executable pages
        # when Windows rebuilds the same path on a mounted NTFS workspace.
        executable = root / 'lightpanel'
        shutil.copyfile(binary, executable)
        executable.chmod(0o700)
        workspace = root / 'workspace'
        workspace.mkdir()
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            port = listener.getsockname()[1]
        origin = f'http://127.0.0.1:{port}'
        cfg = root / 'config.toml'
        cfg.write_text(f'host="127.0.0.1"\nport={port}\nadmin_user="admin"\n'
                       f'password_hash="{TEST_HASH}"\n'
                       f'public_origin="{origin}"\n')
        cfg.chmod(0o600)
        env = {k: v for k, v in os.environ.items() if not k.startswith('LP_')}
        with (root / 'audit.jsonl').open('w+') as audit:
            start = time.perf_counter()
            process = subprocess.Popen([str(executable), '-c', str(cfg)], env=env, stdout=audit, stderr=audit)
            try:
                opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
                def req(path, data=None, token=None):
                    headers = {'Origin': origin}
                    if token:
                        headers['X-CSRF-Token'] = token
                    if isinstance(data, dict):
                        data = urllib.parse.urlencode(data).encode()
                        headers['Content-Type'] = 'application/x-www-form-urlencoded'
                    return opener.open(urllib.request.Request(origin + path, data=data, headers=headers), timeout=10)
                deadline = time.monotonic() + 10
                while True:
                    try:
                        with req('/login') as response:
                            assert response.status == 200
                        break
                    except (urllib.error.URLError, ConnectionError):
                        if process.poll() is not None or time.monotonic() > deadline:
                            audit.flush()
                            audit.seek(0)
                            raise RuntimeError('server did not start: ' + audit.read())
                        time.sleep(0.02)
                startup_ms = (time.perf_counter() - start) * 1000
                with req('/login', {'username': 'admin', 'password': PASSWORD}) as response:
                    page = response.read().decode()
                token = re.search(r'name="csrf-token" content="([a-f0-9]+)"', page).group(1)
                def expect_status(path, code, data=None, csrf=None):
                    try:
                        req(path, data, csrf)
                    except urllib.error.HTTPError as error:
                        assert error.code == code, (path, error.code)
                    else:
                        raise AssertionError(f'{path} unexpectedly succeeded')
                expect_status('/api/file/delete', 403, {'path': 'absent'})
                sample = str(workspace / 'sample.txt')
                quoted = urllib.parse.quote(sample)
                payload = b'lightpanel-smoke\n' * 65536
                with req(f'/api/file/upload?path={quoted}', payload, token) as response:
                    assert response.status == 200
                with req(f'/api/file/download?path={quoted}') as response:
                    assert response.read() == payload
                    assert response.headers['Content-Disposition'].startswith('attachment;')
                with req('/api/file/chmod', {'path': sample, 'mode': '640'}, token) as response:
                    assert response.status == 200
                with req('/api/file/delete', {'path': sample}, token) as response:
                    assert response.status == 200
                expect_status('/api/file/download?path=../config.toml', 400)
                def cpu_ticks():
                    fields = Path(f'/proc/{process.pid}/stat').read_text().split(') ', 1)[1].split()
                    return int(fields[11]) + int(fields[12])
                before = cpu_ticks()
                started = time.perf_counter()
                for _ in range(100):
                    with req('/api/metrics') as response:
                        metrics = json.load(response)
                        assert metrics['memory_total'] > 0
                elapsed = time.perf_counter() - started
                ticks = cpu_ticks() - before
                status = Path(f'/proc/{process.pid}/status').read_text()
                rss = int(re.search(r'^VmRSS:\s+(\d+)', status, re.M).group(1))
                peak = int(re.search(r'^VmHWM:\s+(\d+)', status, re.M).group(1))
                with req('/logout', {'csrf': token}, token) as response:
                    assert response.status == 200  # Redirected to login.
                expect_status('/api/metrics', 401)
                result = {'startup_ready_ms': round(startup_ms, 2), 'rss_kib': rss,
                          'peak_rss_kib': peak, 'metrics_100_requests_ms': round(elapsed * 1000, 2),
                          'cpu_seconds_for_100_metrics': ticks / os.sysconf('SC_CLK_TCK'),
                          'binary_bytes': binary.stat().st_size, 'file_roundtrip_bytes': len(payload)}
                process.terminate()
                process.wait(timeout=12)
                assert process.returncode == 0, process.returncode
                audit.seek(0)
                records = [json.loads(line) for line in audit if line.strip()]
                assert any(r.get('msg') == 'audit_end' and r.get('path') == sample for r in records)
                assert all(PASSWORD not in json.dumps(r) and token not in json.dumps(r) for r in records)
                result['graceful_shutdown'] = True
                result['audit_verified'] = True
                print(json.dumps(result, ensure_ascii=False, indent=2))
            finally:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=12)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait()

if __name__ == '__main__':
    main()
