#!/usr/bin/env python3
"""Loopback-only visual fixture server; does not execute any server operations.

python scripts/ui-preview.py
Open http://127.0.0.1:8894/ (or /login, /?readonly=1).
All displayed data is synthetic. POST requests are rejected. This is not the
real Linux server or an authentication/integration test.
"""
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse, parse_qs
import json
import math
import re
import time

ROOT = Path(__file__).resolve().parents[1]
GIB = 1024 ** 3
ENGINES = [dict(engine=e, installed=e in ('nginx', 'docker', 'mysql', 'redis'),
                running=e in ('nginx', 'docker', 'mysql', 'redis'), version=v)
           for e, v in [('nginx', 'nginx/1.26.2'), ('apache', '尚未安装'), ('docker', '27.3.1'),
                        ('mysql', '8.0.39'), ('mariadb', '尚未安装'), ('postgresql', '尚未安装'), ('redis', '7.4.0')]]
TASKS = [dict(id='preview-deploy', title='部署 product.example.com', kind='site-create', state='done',
              created_at='2026-10-01T02:20:00Z', updated_at='2026-10-01T02:20:08Z',
              output='[预览数据]\n检查运行环境…\n生成站点配置…\n配置检查通过\n部署完成。')]


def fixture(path, query):
    if path == '/api/metrics':
        return dict(hostname='workspace-preview', os='Ubuntu 24.04 LTS · 预览数据', uptime=1284300,
                    sample_ready=True, cpu_percent=18 + math.sin(time.time() / 9) * 7,
                    memory_used=3.8 * GIB, memory_total=16 * GIB, disk_used=28.6 * GIB,
                    disk_total=100 * GIB, rx_bytes_per_sec=240000, tx_bytes_per_sec=94000,
                    self_rss=12 * 1024 ** 2, load=[0.42, 0.38, 0.31], errors=[])
    if path == '/api/health':
        return dict(mode='helper', uid=1001, systemd=True, tools={'systemctl': True, 'journalctl': True},
                    helper=dict(configured=True, reachable=True, service_rules=8, wildcard_services=False,
                                allow_firewall=True, allow_kill=True, allow_update=True), warnings=[],
                    notes=['这是本地界面预览，所有数据均为测试数据，不会对服务器执行操作。'])
    if path == '/api/sites':
        return dict(environment=ENGINES[:3], items=[dict(id=f'preview-{i}', engine='nginx', kind='static',
                    server_names=[domain], ports=['80', '443'], state='active', ssl=True,
                    root=f'/var/www/{name}', managed=True, detail=f'/etc/nginx/conf.d/{name}.conf')
                    for i, (name, domain) in enumerate([('product', 'product.example.com'),
                    ('docs', 'docs.example.com'), ('api', 'api.example.com')])])
    if path == '/api/sites/certs':
        return {'certs': []}
    if path == '/api/databases':
        return dict(engines=ENGINES[3:], databases={'mysql': [dict(name='product', user='product_user',
                    charset='utf8mb4', size=128 * 1024 ** 2)]}, users={'mysql': ['product_user\tlocalhost']},
                    units={'mysql': ['mysql.service'], 'redis': ['redis.service']}, errors={})
    if path == '/api/apps':
        return dict(package_manager='apt-get', items=[dict(name=e['engine'], title=e['engine'],
                    installed=e['installed'], running=e['running'], version=e['version'], package=e['engine'],
                    description=description) for e, description in zip(ENGINES,
                    ['高性能 Web 服务器与反向代理。', '成熟可靠的 HTTP 服务。', '容器运行环境，让应用部署更简单。',
                     '广泛使用的关系型数据库。', '开源关系型数据库引擎。', '功能丰富的对象关系型数据库。', '高性能内存数据存储。'])])
    if path == '/api/services':
        if 'name' in query:
            return {'output': f"{query['name'][0]} - Preview service\nActive: active (running)\n[fixture data]"}
        return {'items': [dict(name=f'{name}.service', state=state, unit_file_state='enabled', description=desc)
                          for name, state, desc in [('nginx', 'active', 'A high performance web server'),
                          ('docker', 'active', 'Docker Application Container Engine'), ('mysql', 'active', 'MySQL Community Server'),
                          ('redis', 'active', 'Advanced key-value store'), ('ssh', 'active', 'OpenBSD Secure Shell server'),
                          ('cron', 'inactive', 'Regular background program processing daemon')]]}
    if path == '/api/processes':
        items = [dict(pid=pid, name=name, uid=0, state='S', rss_bytes=rss * 1024 ** 2, start_time=1)
                 for pid, name, rss in [(1258, 'mysqld', 280), (982, 'dockerd', 145), (1730, 'nginx', 32), (621, 'redis-server', 18)]]
        term = query.get('q', [''])[0]
        items = [p for p in items if term in p['name'] or term in str(p['pid'])]
        return dict(items=items, total=len(items))
    if path == '/api/files':
        directory = query.get('path', ['/'])[0]
        return dict(path=directory, more=False, items=[dict(name=name, path=directory.rstrip('/') + '/' + name,
                    is_dir=directory == '/', regular=directory != '/', symlink=False, size=2048,
                    mode='755' if directory == '/' else '644', modified=1790820000)
                    for name in (['etc', 'home', 'opt', 'usr', 'var'] if directory == '/' else ['README.md', 'config.toml'])])
    if path == '/api/file/read':
        return dict(path=query.get('path', [''])[0], content='# Local visual fixture\nNo server files are modified.\n')
    if path == '/api/logs':
        return {'output': 'Oct 01 10:20:00 workspace systemd[1]: Started nginx.service.\nOct 01 10:20:02 workspace lightpanel[931]: Preview fixture ready.\n'}
    if path == '/api/firewall':
        return {'engine': 'ufw', 'output': 'Status: active\n\nTo                         Action      From\n22/tcp                     ALLOW       Anywhere\n80/tcp                     ALLOW       Anywhere\n443/tcp                    ALLOW       Anywhere\n'}
    if path == '/api/tasks':
        return {'tasks': TASKS}
    if path.startswith('/api/tasks/'):
        return TASKS[0]
    if path == '/api/update/check':
        return dict(current='preview', latest='', update_available=False)
    if path == '/api/databases/backups':
        return {'items': []}
    return {}


class Handler(SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=str(ROOT), **kwargs)

    def do_POST(self):
        self.send_error(405, 'Visual preview only: server operations are disabled')

    def do_GET(self):
        url = urlparse(self.path)
        query = parse_qs(url.query)
        if url.path.startswith('/static/'):
            self.send_response(200)
            body = (ROOT / url.path.lstrip('/')).read_bytes()
            self.send_header('Content-Type', 'text/javascript' if url.path.endswith('.js')
                             else 'text/css' if url.path.endswith('.css') else 'application/octet-stream')
            self.send_header('Content-Length', str(len(body)))
            self.send_header('Cache-Control', 'no-store')
            self.end_headers()
            self.wfile.write(body)
            return
        if url.path.startswith('/api/'):
            return self.respond(json.dumps(fixture(url.path, query), ensure_ascii=False), 'application/json')
        if url.path == '/viewport':
            # Sized iframe allows actual CSS media-query inspection in browsers
            # whose automation adapter does not expose viewport resizing.
            page = '/login' if query.get('page') == ['login'] else '/'
            return self.respond(f'<title>390px UI preview</title><body style="margin:0;background:#dde4df"><iframe title="390px preview" src="{page}" style="width:390px;height:844px;border:0;display:block;margin:24px auto;background:white"></iframe>', 'text/html')
        if url.path not in ('/', '/login'):
            return self.send_error(404)
        name = 'login.html' if url.path == '/login' else 'index.html'
        body = (ROOT / 'templates' / name).read_text(encoding='utf-8-sig')
        readonly = query.get('readonly') == ['1']
        body = re.sub(r'{{if .ReadOnly}}(.*?){{end}}', lambda m: m[1] if readonly else '', body, flags=re.S)
        for key, value in {'.CSRF': 'preview', '.ReadOnly': str(readonly).lower(), '.UploadMB': '32',
                           '.Version': 'preview', '.TerminalEnabled': 'false', '.User': 'admin',
                           'slice .User 0 1': 'A'}.items():
            body = body.replace('{{' + key + '}}', value)
        return self.respond(body, 'text/html')

    def respond(self, body, mime):
        encoded = body.encode('utf-8')
        self.send_response(200)
        self.send_header('Content-Type', mime + '; charset=utf-8')
        self.send_header('Content-Length', str(len(encoded)))
        self.send_header('Cache-Control', 'no-store')
        self.send_header('Content-Security-Policy', "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self'; frame-src 'self'; form-action 'self'; base-uri 'none'")
        self.end_headers()
        self.wfile.write(encoded)


if __name__ == '__main__':
    print('Visual fixtures only: http://127.0.0.1:8894/ — POST disabled', flush=True)
    ThreadingHTTPServer(('127.0.0.1', 8894), Handler).serve_forever()
