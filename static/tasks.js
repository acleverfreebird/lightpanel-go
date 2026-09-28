import { $, api, mutate, guard, el, badge, message, readOnly } from './ui.js';

// Shared background jobs. Opening the dialog does not change the current page.
const stateLabels = { running: '执行中', done: '已完成', error: '失败' };
const stateClasses = { running: 'warn', done: 'good', error: 'bad' };
let tasks = [], loaded = false, selectedId = null, polling = null;
let selectionVersion = 0, detailPending = null, listSignature = '', currentOutput = '';

const runningCount = () => tasks.filter(task => task.state === 'running').length;
const formatTime = value => value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '—';

function feedback(selector, text) {
  const node = $(selector);
  node.textContent = text;
  node.hidden = !text;
}

function renderNavigation() {
  const count = runningCount();
  $('#task-count').hidden = !count;
  $('#task-count').textContent = String(count);
  $('#task-center-open').setAttribute('aria-label', count ? `任务中心，${count} 个任务执行中` : '任务中心');
  $('#task-center-open').classList.toggle('busy', count > 0);
  $('#task-summary').textContent = count ? `${count} 个任务执行中 · 共 ${tasks.length} 个任务` : `共 ${tasks.length} 个任务 · 暂无执行中的任务`;
  $('#task-total').textContent = String(tasks.length);
  $('#task-dialog-clear').disabled = readOnly || !tasks.some(task => task.state !== 'running');
}

function renderList() {
  $('#task-empty').hidden = !loaded || tasks.length > 0;
  // Keep focused buttons in place when only log timestamps change during polling.
  const signature = JSON.stringify([selectedId, tasks.map(({ id, title, state, created_at }) => [id, title, state, created_at])]);
  if (signature === listSignature) return;
  listSignature = signature;
  const focusedId = document.activeElement?.dataset.taskId;
  let focusTarget;
  $('#task-list').replaceChildren(...tasks.map(task => {
    const row = el('li', undefined, 'task-item');
    const control = el('button', undefined, `task-select${task.id === selectedId ? ' selected' : ''}`);
    control.type = 'button';
    control.dataset.taskId = task.id;
    control.setAttribute('aria-pressed', String(task.id === selectedId));
    const head = el('span', undefined, 'task-row-heading');
    head.append(el('span', task.title, 'task-title'), badge(stateLabels[task.state] || task.state, stateClasses[task.state] || ''));
    control.append(head, el('span', formatTime(task.created_at), 'task-time'));
    control.addEventListener('click', () => selectTask(task.id));
    row.append(control);
    if (focusedId === task.id) focusTarget = control;
    return row;
  }));
  focusTarget?.focus({ preventScroll: true });
}

function renderMetadata(task) {
  $('#task-detail-title').textContent = task?.title || (selectedId ? '正在读取任务…' : '选择任务查看详情');
  const state = $('#task-detail-state');
  state.hidden = !task;
  state.textContent = task ? stateLabels[task.state] || task.state : '';
  state.className = `badge ${stateClasses[task?.state] || ''}`;
  $('#task-meta').hidden = !task;
  $('#task-started').textContent = formatTime(task?.created_at);
  $('#task-updated').textContent = formatTime(task?.updated_at);
  feedback('#task-execution-error', task?.error ? `执行失败：${task.error}` : '');
}

function renderDetail(task) {
  const pre = $('#task-detail');
  renderMetadata(task);
  currentOutput = task?.output || '';
  $('#task-copy').disabled = !currentOutput;
  pre.hidden = !currentOutput;
  if (pre.textContent !== currentOutput) {
    pre.textContent = currentOutput;
    if ($('#task-follow').checked) pre.scrollTop = pre.scrollHeight;
  }
  feedback('#task-detail-feedback', !task ? '选择任务查看执行日志' : currentOutput ? '' : task.state === 'running'
    ? '任务执行中，等待日志输出。部分操作会在步骤结束后返回日志。' : '该任务没有日志输出。');
}

async function loadDetail() {
  const id = selectedId, version = selectionVersion;
  if (!id) return;
  if (detailPending?.version === version) return detailPending.promise;
  const promise = (async () => {
    try {
      const task = await api(`/api/tasks/${encodeURIComponent(id)}`);
      if (version === selectionVersion && $('#task-dialog').open) renderDetail(task);
    } catch (error) {
      if (version === selectionVersion && $('#task-dialog').open) {
        feedback('#task-detail-feedback', `日志读取失败，正在重试。${error.message}`);
      }
    } finally {
      if (detailPending?.version === version) detailPending = null;
    }
  })();
  detailPending = { version, promise };
  return promise;
}

async function selectTask(id) {
  selectedId = id;
  selectionVersion++;
  $('#task-follow').checked = true;
  renderList();
  renderDetail(null);
  if (!id) return;
  renderMetadata(tasks.find(task => task.id === id));
  feedback('#task-detail-feedback', '正在读取执行日志…');
  return loadDetail();
}

export function openTaskCenter(taskId = null) {
  const dialog = $('#task-dialog');
  if (!dialog.open) dialog.showModal();
  $('#task-center-open').classList.toggle('task-open', true);
  const id = taskId || tasks.find(task => task.state === 'running')?.id || tasks[0]?.id || null;
  selectTask(id);
  tick(true);
}

async function refreshTasks() {
  const selectionAtRequest = selectionVersion;
  const data = await api('/api/tasks');
  const previous = new Map(tasks.map(task => [task.id, task.state]));
  tasks = data.tasks || [];
  loaded = true;
  renderNavigation();
  feedback('#task-list-error', '');
  for (const task of tasks) {
    if (previous.get(task.id) === 'running' && task.state !== 'running') {
      message(task.state === 'done' ? `任务完成：${task.title}` : `任务失败：${task.title}：${task.error || ''}`, task.state !== 'done');
      window.dispatchEvent(new CustomEvent('task-finished', { detail: task }));
    }
  }
  if (!$('#task-dialog').open) return;
  // A task opened while this request was in flight may not be in its snapshot.
  if (selectionAtRequest === selectionVersion && !tasks.some(task => task.id === selectedId)) {
    await selectTask(tasks.find(task => task.state === 'running')?.id || tasks[0]?.id || null);
  } else {
    renderList();
    // Fetch even after completion: the list endpoint intentionally omits output.
    await loadDetail();
  }
}

function tick(force = false) {
  if (polling) return polling;
  if (!force && (document.hidden || (loaded && !$('#task-dialog').open && runningCount() === 0))) return Promise.resolve();
  polling = refreshTasks().catch(error => {
    feedback('#task-list-error', `任务列表读取失败，正在重试。${error.message}`);
  }).finally(() => { polling = null; });
  return polling;
}

export function setupTasks() {
  $('#task-center-open').addEventListener('click', () => openTaskCenter());
  $('#task-dialog-close').addEventListener('click', () => $('#task-dialog').close());
  $('#task-dialog-clear').addEventListener('click', guard(async () => {
    try {
      await mutate('/api/tasks/clear', {});
      // Wait out a list request started before the clear, then read fresh state.
      if (polling) await polling;
      await tick(true);
    } catch (error) {
      feedback('#task-list-error', `清除失败：${error.message}`);
    }
  }));
  $('#task-copy').addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(currentOutput);
      feedback('#task-detail-feedback', '日志已复制。');
    } catch {
      feedback('#task-detail-feedback', '无法自动复制，请选中日志文本手动复制。');
    }
  });
  $('#task-follow').addEventListener('change', () => {
    if ($('#task-follow').checked) $('#task-detail').scrollTop = $('#task-detail').scrollHeight;
  });
  $('#task-detail').addEventListener('scroll', () => {
    const pre = $('#task-detail');
    if (pre.scrollHeight - pre.scrollTop - pre.clientHeight > 48) $('#task-follow').checked = false;
  });
  $('#task-dialog').addEventListener('close', () => {
    selectedId = null;
    selectionVersion++;
    $('#task-center-open').classList.toggle('task-open', false);
    renderDetail(null);
  });
  document.addEventListener('visibilitychange', () => { if (!document.hidden) tick(); });
  setInterval(tick, 2000);
  tick();
}
