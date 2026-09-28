import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../static/tasks.js', import.meta.url), 'utf8');
const task = (id, state = 'running', output = '') => ({ id, title: `安装 ${id}`, state, output,
  created_at: '2026-09-28T10:00:00Z', updated_at: '2026-09-28T10:01:00Z' });
function harness(api) {
  const nodes = new Map();
  const el = (tag, text = '', className = '') => ({ tag, textContent: text, className, dataset: {},
    hidden: false, children: [], checked: true, scrollHeight: 400, scrollTop: 0, clientHeight: 200,
    classList: { toggle() {} }, setAttribute() {}, removeAttribute() {}, addEventListener() {},
    append(...children) { this.children.push(...children); },
    replaceChildren(...children) { this.children = children; },
    showModal() { this.open = true; }, focus() {},
  });
  const $ = selector => {
    if (!nodes.has(selector)) nodes.set(selector, el('div'));
    return nodes.get(selector);
  };
  const context = vm.createContext({ $, el, badge: (text, cls) => el('span', text, cls), api,
    readOnly: false, message() {}, guard: fn => fn, setInterval() {},
    document: { hidden: false, addEventListener() {}, activeElement: null },
    window: { dispatchEvent() {} }, CustomEvent: class {},
  });
  vm.runInContext(source.replace(/^import .*;\r?\n/gm, '').replace(/^export /gm, ''), context);
  return { context, $ };
}

test('selected task receives final output when it finishes', async () => {
  let current = task('a', 'running', '开始安装');
  const { context, $ } = harness(async path => path === '/api/tasks' ? { tasks: [current] } : current);
  $('#task-dialog').open = true;
  await context.tick();
  await context.selectTask('a');
  current = task('a', 'done', '开始安装\n安装完成');
  await context.tick();
  assert.match($('#task-detail').textContent, /安装完成/);
});

test('late detail response cannot overwrite the newly selected task', async () => {
  let resolveFirst;
  const { context, $ } = harness(path => path.endsWith('/a')
    ? new Promise(resolve => { resolveFirst = resolve; }) : Promise.resolve(task('b', 'done', 'B 的日志')));
  $('#task-dialog').open = true;
  const first = context.selectTask('a');
  await context.selectTask('b');
  resolveFirst(task('a', 'done', 'A 的日志'));
  await first;
  assert.match($('#task-detail').textContent, /B 的日志/);
  assert.doesNotMatch($('#task-detail').textContent, /A 的日志/);
});

test('list requested before task creation cannot replace the task opened by its caller', async () => {
  let resolveList;
  const { context, $ } = harness(path => path === '/api/tasks'
    ? new Promise(resolve => { resolveList = resolve; })
    : Promise.resolve(task(path.split('/').at(-1), 'running', path.endsWith('/new') ? '新任务日志' : '旧任务日志')));
  const poll = context.tick(true);
  context.openTaskCenter('new');
  resolveList({ tasks: [task('old')] });
  await poll;
  assert.match($('#task-detail').textContent, /新任务日志/);
});
