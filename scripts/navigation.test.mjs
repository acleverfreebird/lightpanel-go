import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../static/navigation.js', import.meta.url), 'utf8');
const context = vm.createContext({});
vm.runInContext(source.replace(/^export /gm, ''), context);
const entries = [
  { id: 'sites', title: '网站管理', description: '域名与证书', keywords: 'nginx ssl' },
  { id: 'databases', title: '数据库管理', description: '数据库与用户', keywords: 'mysql redis sql' },
];
const matches = query => Array.from(context.matchNavigation(entries, query), entry => entry.id);
test('blank search returns all destinations in navigation order', () => assert.deepEqual(matches('  '), ['sites', 'databases']));
test('Chinese title and description can find a destination', () => {
  assert.deepEqual(matches('网站'), ['sites']);
  assert.deepEqual(matches('证书'), ['sites']);
});
test('English keywords ignore case and whitespace', () => assert.deepEqual(matches('  MySQL '), ['databases']));
test('all search terms must match the same destination', () => {
  assert.deepEqual(matches('网站 SSL'), ['sites']);
  assert.deepEqual(matches('网站 MySQL'), []);
});
test('unmatched and markup-like input are treated as plain search text', () => {
  assert.deepEqual(matches('不存在'), []);
  assert.deepEqual(matches('<script>'), []);
});
