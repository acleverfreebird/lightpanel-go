#!/usr/bin/env python3
"""Disposable Linux UI validation server; test credentials only, loopback only.

Usage: python3 scripts/preview.py /tmp/lightpanel-ui-check [--readonly]
Login: admin / local-test-only-password. Ctrl+C cleans up the server and files.
Do not use this configuration for deployment.
"""
from pathlib import Path
import subprocess
import sys
import tempfile
import os

with tempfile.TemporaryDirectory(prefix='lightpanel-ui-') as directory:
    root = Path(directory)
    (root / 'files' / 'backups').mkdir(parents=True)
    (root / 'files' / 'readme.txt').write_text('LightPanel UI validation fixture\n')
    config = root / 'config.toml'
    readonly = '--readonly' in sys.argv
    port = 8893 if readonly else 8892
    config.write_text(f'''host = "127.0.0.1"
port = {port}
admin_user = "admin"
password_hash = "$2a$10$rsjl2q3/rV/bLuwfx4vf0OYE7r/n.0mbGv9vNwlRiTS5OD.akczYG"
sandbox_root = "{root}/files"
public_origin = "http://127.0.0.1:{port}"
read_only = {str(readonly).lower()}
''')
    env = {key: value for key, value in os.environ.items() if not key.startswith('LP_')}
    process = subprocess.Popen([sys.argv[1], '-c', str(config)], env=env)
    try:
        process.wait()
    except KeyboardInterrupt:
        pass
    finally:
        process.terminate()
        process.wait(timeout=12)
