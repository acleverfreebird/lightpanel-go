export const $ = selector => document.querySelector(selector);
export const readOnly = document.body.dataset.readonly === 'true';
const csrf = $('meta[name="csrf-token"]').content;
let mutationPending = false;

export function el(tag, text, className) {
  const node = document.createElement(tag);
  if (text !== undefined) node.textContent = text;
  if (className) node.className = className;
  return node;
}
export function message(text, error = false) {
  $('#message-text').textContent = text;
  $('#message').className = error ? 'error' : '';
  $('#message').hidden = !text;
}
export function guard(fn) {
  return async event => {
    event?.preventDefault();
    const control = event?.submitter || (event?.currentTarget?.tagName === 'BUTTON' ? event.currentTarget : null);
    if (control?.dataset.busy === 'true') return;
    if (control) control.dataset.busy = 'true';
    try { await fn(event); }
    catch (error) { message(error.message, true); }
    finally { if (control) delete control.dataset.busy; }
  };
}
export async function api(path, options = {}) {
  let response;
  try { response = await fetch(path, { credentials: 'same-origin', ...options, signal: AbortSignal.timeout(65000) }); }
  catch { throw new Error('暂时无法连接服务器，请检查连接后重试。'); }
  if (response.status === 401) { location.assign('/login'); throw new Error('登录已过期，请重新登录。'); }
  if (!response.ok) {
    const detail = (await response.text()).slice(0, 2500).trim();
    const names = { 403: '操作被拒绝：请检查权限或重新登录', 409: '目标已变化或文件已存在，请刷新后重试', 413: '文件超过 32 MiB 上限', 429: '请求过于频繁，请稍后重试', 501: '服务器不支持这项功能', 502: '系统命令执行失败', 503: '服务器繁忙，请稍后重试', 504: '操作超时，请刷新确认实际状态' };
    throw new Error(`${names[response.status] || '请求未完成'}（${response.status}）\n${detail}`);
  }
  return response.json();
}
export async function mutate(path, data, raw = false) {
  if (readOnly) throw new Error('当前为只读模式，不能修改服务器。');
  if (mutationPending) throw new Error('上一项操作仍在进行，请稍候。');
  mutationPending = true;
  const buttons = [...document.querySelectorAll('[data-mutation]')];
  const prior = buttons.map(button => button.disabled);
  buttons.forEach(button => button.disabled = true);
  try {
    const result = await api(path, { method: 'POST', headers: { 'X-CSRF-Token': csrf }, body: raw ? data : new URLSearchParams(data) });
    message('操作已完成。');
    return result;
  } finally {
    mutationPending = false;
    buttons.forEach((button, i) => button.disabled = prior[i]);
  }
}
export function button(text, action, mutation = false, className = 'text-button') {
  const node = el('button', text, className);
  node.type = 'button';
  if (mutation) { node.dataset.mutation = ''; node.disabled = readOnly; node.title = readOnly ? '当前账号只有查看权限' : text; }
  node.addEventListener('click', guard(action));
  return node;
}
export function badge(text, state = '') { return el('span', text, `badge ${state}`); }
export function table(target, headers, rows, emptyText = '暂无条目', emptyHint = '尝试调整筛选条件或刷新数据。') {
  const host = $(target);
  if (!rows.length) {
    const empty = el('div', undefined, 'empty-state');
    empty.append(el('strong', emptyText), el('p', emptyHint));
    host.replaceChildren(empty);
    return;
  }
  const grid = el('table'), head = el('thead'), row = el('tr'), body = el('tbody');
  headers.forEach(text => { const th = el('th', text); th.scope = 'col'; row.append(th); });
  head.append(row);
  rows.forEach(values => {
    const row = el('tr');
    values.forEach(value => { const cell = el('td'); cell.append(value instanceof Node ? value : document.createTextNode(String(value))); row.append(cell); });
    body.append(row);
  });
  grid.append(head, body);
  host.replaceChildren(grid);
}
export function confirmAction({ title, description, target, confirm = '确认操作', danger = true, input = null }) {
  const dialog = $('#action-dialog');
  if (dialog.open) return Promise.resolve(null);
  $('#dialog-title').textContent = title;
  $('#dialog-description').textContent = description;
  $('#dialog-target').textContent = target;
  $('#dialog-confirm').textContent = confirm;
  $('#dialog-confirm').className = danger ? 'danger' : '';
  $('#dialog-input-label').hidden = input === null;
  $('#dialog-input').disabled = input === null;
  $('#dialog-input').required = input !== null;
  $('#dialog-input').value = input ?? '';
  return new Promise(resolve => {
    let result = null;
    const submit = event => { event.preventDefault(); result = input === null ? true : $('#dialog-input').value; dialog.close(); };
    const cancel = () => dialog.close();
    const closed = () => { $('#dialog-form').removeEventListener('submit', submit); $('#dialog-cancel').removeEventListener('click', cancel); resolve(result); };
    $('#dialog-form').addEventListener('submit', submit);
    $('#dialog-cancel').addEventListener('click', cancel);
    dialog.addEventListener('close', closed, { once: true });
    dialog.showModal();
    (input === null ? $('#dialog-cancel') : $('#dialog-input')).focus();
  });
}
