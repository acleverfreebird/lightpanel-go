import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const app = await readFile(new URL('../static/app.js', import.meta.url), 'utf8');
const sites = await readFile(new URL('../static/sites.js', import.meta.url), 'utf8');
const template = await readFile(new URL('../templates/index.html', import.meta.url), 'utf8');

test('site management page is wired into navigation, routes and template', () => {
  assert.match(app, /sites: \['站点管理'/, 'app.js must register the sites page');
  assert.match(app, /setupSites\(go\)/, 'app.js must install sites.js handlers');
  assert.match(template, /data-tab="sites"/, 'sidebar must contain a sites tab');
  assert.match(template, /<section id="sites" hidden/, 'template must contain the sites section');
  assert.match(template, /id="site-list"/, 'sites section must contain the site table');
  assert.match(template, /id="site-form"/, 'sites section must contain the create form');
  assert.match(sites, /\/api\/sites'/, 'sites.js must call the sites API');
});

test('client-side site name pattern matches the server validation', () => {
  const pattern = template.match(/name="name" required pattern="([^"]+)"/)?.[1];
  assert.ok(pattern, 'site name input must declare a pattern');
  const matcher = new RegExp(`^(?:${pattern})$`);
  for (const name of ['blog', 'a1', 'my-site-2']) assert.match(name, matcher);
  for (const name of ['', '-a', 'UPPER', 'a b', 'site.name', `${'a'.repeat(40)}`]) assert.doesNotMatch(name, matcher);
});

test('unmanaged docker sites expose no actions and managed ones do', () => {
  // Mirror of the backend guardrail: only lightpanel-created containers may
  // be started, stopped or removed from the UI.
  assert.match(sites, /if \(site\.managed\)/, 'docker actions must be gated on the managed flag');
  assert.match(sites, /removeSite/, 'delete action must exist');
});
