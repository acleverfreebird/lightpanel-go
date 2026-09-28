import { $, api, mutate, guard, el, badge, message } from './ui.js';

// 任务中心：全局轮询后台任务，右下角悬浮球 + 弹窗展示状态与日志。
// 任务结束（running → done/error）时派发 task-finished 事件，其他模块自行刷新。

const stateLabels = { running: '进行中', done: '已完成', error: '失败' };
const stateClasses = { done: 'good', error: 'bad' };
let tasks = [], loaded = false, selectedId = null, ticking = false;

function runningCount() {
  return tasks.filter(task => task.state === 'running').length;
}

function renderFab() {
  const fab = $('#task-fab');
  fab.hidden = !loaded;
  const count = runningCount();
  const countBadge = $('#task-fab-count');
  countBadge.hidden = !count;
  countBadge.textContent = count;
  fab.classList.toggle('busy', count > 0);
}

function renderList() {
  $('#task-empty').hidden = tasks.length > 0;
  $('#task-list').replaceChildren(...tasks.map(task => {
    const row = el('li', undefined, `task-item${task.id === selectedId ? ' selected' : ''}`);
    const head = el('div', undefined, 'task-head');
    const title = el('button', undefined, 'task-title');
    title.type = 'button';
    title.append(el('span', task.title));
    title.append(badge(stateLabels[task.state] || task.state, stateClasses[task.state] || ''));
    title.addEventListener('click', () => selectTask(task.id));
    head.append(title);
    head.append(el('span', new Date(task.created_at).toLocaleTimeString(), 'muted'));
    row.append(head);
    if (task.state === 'error' && task.error) row.append(el('p', `错误：${task.error}`, 'task-error'));
    return row;
  }));
}

function renderDetail(task) {
  const pre = $('#task-detail');
  if (!task) {
    pre.hidden = true;
    pre.textContent = '';
    delete pre.dataset.taskId;
    return;
  }
  pre.hidden = false;
  const label = stateLabels[task.state] || task.state;
  const text = `${task.title} · ${label}${task.error ? `（${task.error}）` : ''}\n${task.output || ''}`;
  const nearBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 48;
  if (pre.dataset.taskId !== task.id) {
    pre.dataset.taskId = task.id;
    pre.textContent = text;
    pre.scrollTop = 0;
  } else if (pre.textContent !== text) {
    pre.textContent = text;
    if (task.state === 'running' && nearBottom) pre.scrollTop = pre.scrollHeight;
  }
}

async function selectTask(id) {
  selectedId = id;
  renderList();
  try {
    renderDetail(await api(`/api/tasks/${id}`));
  } catch (error) {
    message(error.message, true);
  }
}

export function openTaskCenter(taskId = null) {
  selectedId = taskId;
  const dialog = $('#task-dialog');
  if (!dialog.open) dialog.showModal();
  if (!taskId) {
    const active = tasks.find(task => task.state === 'running');
    if (active) selectedId = active.id;
  }
  if (selectedId) {
    selectTask(selectedId);
  } else {
    renderList();
    renderDetail(null);
  }
  tick();
}

async function tick() {
  if (ticking) return;
  const dialog = $('#task-dialog');
  const idle = loaded && !dialog.open && runningCount() === 0;
  if (idle || document.hidden) return;
  ticking = true;
  try {
    const data = await api('/api/tasks');
    const previous = new Map(tasks.map(task => [task.id, task.state]));
    tasks = data.tasks || [];
    loaded = true;
    renderFab();
    for (const task of tasks) {
      if (previous.get(task.id) === 'running' && task.state !== 'running') {
        // 先写提示再派发事件，让具体模块的提示可以覆盖通用文案。
        message(task.state === 'done' ? `任务完成：${task.title}` : `任务失败：${task.title}：${task.error || ''}`, task.state !== 'done');
        window.dispatchEvent(new CustomEvent('task-finished', { detail: task }));
      }
    }
    if (dialog.open) {
      renderList();
      const selected = tasks.find(task => task.id === selectedId);
      if (selected && selected.state === 'running') {
        renderDetail(await api(`/api/tasks/${selected.id}`));
      }
    }
  } catch (error) {
    if (!loaded) return; // 首次加载失败保持悬浮球隐藏，等待下一轮。
    message(error.message, true);
  } finally {
    ticking = false;
  }
}

export function setupTasks() {
  $('#task-fab').addEventListener('click', () => openTaskCenter());
  $('#task-dialog-close').addEventListener('click', () => $('#task-dialog').close());
  $('#task-dialog-clear').addEventListener('click', guard(async () => {
    await mutate('/api/tasks/clear', {});
    if (selectedId && !tasks.some(task => task.id === selectedId && task.state === 'running')) selectedId = null;
    renderList();
    if (selectedId) await selectTask(selectedId);
    else renderDetail(null);
    await tick();
  }));
  $('#task-dialog').addEventListener('close', () => {
    selectedId = null;
    renderList();
    renderDetail(null);
  });
  document.addEventListener('visibilitychange', () => { if (!document.hidden) tick(); });
  setInterval(tick, 2000);
  tick();
}
