import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const app = await readFile(new URL('../static/app.js', import.meta.url), 'utf8');
const sites = await readFile(new URL('../static/sites.js', import.meta.url), 'utf8');
const template = await readFile(new URL('../templates/index.html', import.meta.url), 'utf8');

test('site management page is wired into navigation, routes and template', () => {
  assert.match(app, /sites: \['网站管理'/, 'app.js must register the sites page');
  assert.match(app, /setupSites\(go\)/, 'app.js must install sites.js handlers');
  assert.match(template, /data-tab="sites"/, 'sidebar must contain a sites tab');
  assert.match(template, /<section id="sites" hidden/, 'template must contain the sites section');
  assert.match(template, /id="site-list"/, 'sites section must contain the site table');
  assert.match(template, /id="site-search"/, 'sites section must contain the search field');
  assert.match(template, /id="site-create-dialog"/, 'template must contain the create-site dialog');
  assert.match(template, /id="site-form"/, 'create dialog must contain the create form');
  assert.match(template, /id="site-dialog"/, 'template must contain the site settings dialog');
  assert.match(template, /id="ssl-form"/, 'settings dialog must contain the SSL form');
  assert.match(sites, /\/api\/sites'/, 'sites.js must call the sites API');
  assert.match(sites, /\/api\/sites\/certs'/, 'sites.js must call the certificate listing API');
  assert.match(sites, /\/api\/sites\/ssl'/, 'sites.js must call the one-click SSL API');
  assert.match(sites, /name=mode/, 'create form must expose the static/proxy mode');
});

test('client-side site name pattern matches the server validation', () => {
  const pattern = template.match(/站点名称<input name="name" required pattern="([^"]+)"/)?.[1];
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

test('SSL issuance uses the built-in ACME client without certbot', () => {
  assert.match(sites, /op: 'issue'/, 'issue action must be posted to /api/sites/ssl');
  assert.match(sites, /op: 'off'/, 'ssl-off action must be posted to /api/sites/ssl');
  assert.match(sites, /force_https/, 'the SSL form must offer force-HTTPS');
  assert.match(sites, /staging/, 'the SSL form must offer staging certificates');
  assert.match(sites, /days_left/, 'cert expiry must be surfaced to the operator');
  assert.doesNotMatch(sites + template, /certbot/i, 'no certbot references may remain in the UI');
  assert.match(template, /自动续期/, 'the UI must document automatic renewal');
});

test('site settings dialog exposes tabbed panes', () => {
  assert.match(template, /data-pane="site-info-pane"/, 'settings tabs must switch panes');
  assert.match(template, /data-pane="site-ssl-pane"/, 'SSL must be a settings pane');
  assert.match(template, /data-pane="site-domains-pane"/, 'domains must be a settings pane');
  assert.match(sites, /switchPane/, 'sites.js must wire the tab switching');
});
