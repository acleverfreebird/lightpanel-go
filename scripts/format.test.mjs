import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
// Load browser ES module without imposing package.json on the embedded frontend.
const source = await readFile(new URL('../static/format.js', import.meta.url), 'utf8');
const { resolveInputPath } = await import('data:text/javascript;base64,' + Buffer.from(source).toString('base64'));
test('file dialogs resolve relative names in the current directory', () => {
  assert.equal(resolveInputPath('new.txt', '/etc/nginx'), '/etc/nginx/new.txt');
  assert.equal(resolveInputPath('a/b', '/var/lib'), '/var/lib/a/b');
  assert.equal(resolveInputPath('/tmp/new', '/etc'), '/tmp/new');
  assert.equal(resolveInputPath('new', '/'), '/new');
  assert.throws(() => resolveInputPath('../escape', '/etc'));
  assert.throws(() => resolveInputPath('', '/etc'));
});
