import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { webcrypto as crypto } from 'node:crypto';
import vm from 'node:vm';

const app = await readFile(new URL('../static/app.js', import.meta.url), 'utf8');
const databases = await readFile(new URL('../static/databases.js', import.meta.url), 'utf8');
const apps = await readFile(new URL('../static/apps.js', import.meta.url), 'utf8');
const template = await readFile(new URL('../templates/index.html', import.meta.url), 'utf8');

// databases.js 以 ES module 书写：剥掉 import/export 后在 vm 里执行，
// 顶层函数声明即成为 context 上的全局，供各测试直接调用。
const loadDatabaseModule = context => {
  vm.runInContext(databases.replace(/^import .*;\r?\n/gm, '').replace(/^export /gm, ''), context);
  return context;
};

test('database page is wired into navigation, routes and template', () => {
  assert.match(app, /databases: \['数据库管理'/, 'app.js must register the databases page');
  assert.match(app, /setupDatabases\(\)/, 'app.js must install databases.js handlers');
  assert.match(template, /data-tab="databases"/, 'sidebar must contain a databases tab');
  assert.match(template, /<section id="databases" hidden/, 'template must contain the databases section');
  assert.match(template, /id="db-engines"/, 'databases section must contain the engine cards');
  assert.match(template, /id="db-list"/, 'databases section must contain the database table');
  assert.match(template, /id="db-create-form"/, 'databases section must contain the create form');
  assert.match(template, /id="db-user-form"/, 'databases section must contain the user form');
  assert.match(template, /id="db-password-form"/, 'databases section must contain the password form');
  assert.match(databases, /\/api\/databases'/, 'databases.js must call the databases API');
  assert.match(databases, /\/api\/databases\/create/, 'databases.js must call the create API');
  assert.match(databases, /\/api\/databases\/delete/, 'databases.js must call the delete API');
  assert.match(databases, /\/api\/databases\/user'/, 'databases.js must call the user API');
  assert.match(databases, /\/api\/databases\/user-password/, 'databases.js must call the password API');
  assert.match(databases, /\/api\/service\/action/, 'engine start/stop must reuse the service action API');
});

test('app store catalogs the database engines', () => {
  const labels = { mysql: 'MySQL', mariadb: 'MariaDB', postgresql: 'PostgreSQL', redis: 'Redis' };
  for (const [name, label] of Object.entries(labels)) {
    assert.match(apps, new RegExp(`${name}: '${label}'`), `apps.js must label ${name}`);
  }
  // Redis is key-value only: the SQL engine selects must not offer it.
  for (const name of ['mysql', 'mariadb', 'postgresql']) {
    assert.match(template, new RegExp(`<option value="${name}">`), `engine selects must offer ${name}`);
  }
  assert.doesNotMatch(template, /<option value="redis">/, 'engine selects must not offer redis');
});

test('database overview renders an error-free API response with omitted errors', async () => {
  // The Go API marks errors as omitempty; a healthy/fresh server omits the map.
  const nodes = new Map();
  const node = (tag, text) => ({ tag, textContent: text, children: [],
    append(...children) { this.children.push(...children); },
    replaceChildren(...children) { this.children = children; },
  });
  const $ = selector => {
    if (!nodes.has(selector)) nodes.set(selector, node('div'));
    return nodes.get(selector);
  };
  // renderEngines() syncs the create form's charset select after every refresh.
  const createForm = node('form');
  createForm.elements = { engine: node('select'), charset: node('select') };
  nodes.set('#db-create-form', createForm);
  const charsetLabel = node('label');
  charsetLabel.querySelector = () => node('small');
  nodes.set('#db-charset-label', charsetLabel);
  const tables = [];
  const context = vm.createContext({
    $, el: node, badge: text => node('span', text),
    document: { querySelectorAll: () => [], createTextNode: text => ({ text }) },
    api: async () => ({
      engines: ['mysql', 'mariadb', 'postgresql', 'redis'].map(engine => ({ engine, installed: false })),
      databases: {}, users: {}, units: {},
    }),
    table: (target, headers, rows, emptyText) => tables.push({ target, rows, emptyText }),
  });
  loadDatabaseModule(context);
  await context.databases();
  assert.equal($('#db-engines').children.length, 4);
  assert.equal($('#db-engine-count').textContent, '0 / 4 已安装');
  assert.deepEqual(tables.map(t => t.target), ['#db-list', '#db-user-list']);
  assert.ok(tables.every(t => t.rows.length === 0 && t.emptyText));
});

test('same-name account suggestion stays within the 32-character user limit', () => {
  // 库名可到 63 字符，用户名只到 32：默认账号照抄超长库名会变成非法值。
  const context = loadDatabaseModule(vm.createContext({}));
  assert.equal(context.sameNameUser('app_production'), 'app_production');
  assert.equal(context.sameNameUser('a'.repeat(40)), 'a'.repeat(32));
  assert.equal(context.sameNameUser('  padded  '), 'padded');
  assert.equal(context.sameNameUser(''), '');
});

test('randomPassword draws every alphabet character without modulo bias', () => {
  const context = loadDatabaseModule(vm.createContext({ crypto }));
  const alphabet = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789!@#$%^&*-_=+';
  const seen = new Set();
  for (let i = 0; i < 200; i++) {
    const password = context.randomPassword();
    assert.equal(password.length, 16);
    for (const ch of password) {
      assert.ok(alphabet.includes(ch), `unexpected character ${ch}`);
      seen.add(ch);
    }
  }
  // 200 次 × 16 位远超期望次数（每字符约 47 次），68 个字符应全部出现。
  assert.equal(seen.size, alphabet.length);
  const samples = new Set(Array.from({ length: 50 }, () => context.randomPassword()));
  assert.equal(samples.size, 50);
});
