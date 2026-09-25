import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const app = await readFile(new URL('../static/app.js', import.meta.url), 'utf8');
const databases = await readFile(new URL('../static/databases.js', import.meta.url), 'utf8');
const apps = await readFile(new URL('../static/apps.js', import.meta.url), 'utf8');
const template = await readFile(new URL('../templates/index.html', import.meta.url), 'utf8');

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
