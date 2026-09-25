import { $, api, mutate, guard, el, button, table, badge, confirmAction } from './ui.js';

const engineNames = { mysql: 'MySQL', mariadb: 'MariaDB', postgresql: 'PostgreSQL', redis: 'Redis' };
const sqlEngines = ['mysql', 'mariadb', 'postgresql'];

let version = 0, data = { engines: [], databases: {}, users: {}, units: {}, errors: {} };

function unitOf(engine) {
  return (data.units[engine] || [])[0] || '';
}

async function serviceAction(engine, action) {
  const unit = unitOf(engine);
  if (!unit) throw new Error(`未找到 ${engineNames[engine]} 的 systemd 服务单元`);
  await mutate('/api/service/action', { name: unit, action });
  await refresh();
}

function engineCard(info) {
  const card = el('article', undefined, 'metric-card');
  card.append(el('h3', engineNames[info.engine] || info.engine));
  const state = info.installed
    ? (info.running ? ['运行中', 'good'] : ['未运行', 'warn'])
    : ['未安装', 'warn'];
  card.append(badge(state[0], state[1]));
  card.append(el('p', info.version || info.detail || '—'));
  const error = data.errors?.[info.engine];
  if (error) {
    const note = el('p', undefined, 'muted');
    note.textContent = `读取列表失败：${error}`;
    card.append(note);
  }
  if (info.installed) {
    const actions = el('div', undefined, 'actions');
    if (unitOf(info.engine)) {
      actions.append(button(info.running ? '重启' : '启动', () => serviceAction(info.engine, info.running ? 'restart' : 'start'), true));
      if (info.running) actions.append(button('停止', () => serviceAction(info.engine, 'stop'), true));
    }
    card.append(actions);
  }
  return card;
}

function renderEngines() {
  const installed = data.engines.filter(info => info.installed);
  $('#db-engine-count').textContent = data.engines.length ? `${installed.length} / ${data.engines.length} 已安装` : '—';
  if (!data.engines.length) {
    $('#db-engines').replaceChildren(el('article', '未能检测数据库引擎。', 'metric-card'));
    return;
  }
  $('#db-engines').replaceChildren(...data.engines.map(engineCard));
  // 按检测状态同步表单里可选的引擎。
  const byEngine = Object.fromEntries(data.engines.map(info => [info.engine, info.installed]));
  document.querySelectorAll('#databases select[name=engine]').forEach(select => {
    select.querySelectorAll('option').forEach(option => {
      option.disabled = !byEngine[option.value];
    });
    if (select.selectedOptions[0]?.disabled) select.value = sqlEngines.find(engine => byEngine[engine]) || select.value;
  });
}

function renderDatabases() {
  const rows = [];
  for (const engine of sqlEngines) {
    for (const name of data.databases[engine] || []) {
      const cell = el('div', undefined, 'file-name');
      cell.append(el('span', '▦', 'file-icon'), name);
      rows.push([cell, engineNames[engine], button('删除', () => dropDatabase(engine, name), true, 'danger-text')]);
    }
  }
  $('#db-count').textContent = String(rows.length);
  table('#db-list', ['数据库', '引擎', '操作'], rows,
    '没有可管理的数据库', '数据库在引擎首次安装后出现，也可以用下方表单创建。');
}

function renderUsers() {
  const rows = [];
  for (const engine of sqlEngines) {
    for (const name of data.users[engine] || []) {
      const cell = el('div', undefined, 'file-name');
      cell.append(el('span', '◎', 'file-icon'), name);
      rows.push([cell, engineNames[engine], button('修改密码', () => prefillPassword(engine, name), true)]);
    }
  }
  $('#db-user-count').textContent = String(rows.length);
  table('#db-user-list', ['用户', '引擎', '操作'], rows,
    '没有可管理的用户', '用户在引擎首次安装后出现，也可以用下方表单创建。');
}

async function dropDatabase(engine, name) {
  const confirmed = await confirmAction({
    title: '删除这个数据库？',
    description: '将永久删除该数据库及其中的全部数据，此操作无法撤销。',
    target: `${name} · ${engineNames[engine]}`,
    confirm: '确认删除',
  });
  if (!confirmed) return;
  await mutate('/api/databases/delete', { engine, name });
  await refresh();
}

function prefillPassword(engine, name) {
  const panel = $('#db-password-panel');
  panel.open = true;
  const form = $('#db-password-form');
  form.elements.engine.value = engine;
  form.elements.name.value = name;
  form.elements.password.focus();
  panel.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
}

async function refresh() {
  const request = ++version;
  data = await api('/api/databases');
  if (request !== version) return;
  renderEngines();
  renderDatabases();
  renderUsers();
}

export async function databases() {
  await refresh();
}

export function setupDatabases() {
  $('#db-detect').addEventListener('click', guard(refresh));
  $('#db-create-form').addEventListener('submit', guard(async event => {
    const form = event.target;
    await mutate('/api/databases/create', { engine: form.engine.value, name: form.name.value.trim() });
    form.reset();
    await refresh();
  }));
  $('#db-user-form').addEventListener('submit', guard(async event => {
    const form = event.target;
    await mutate('/api/databases/user', { engine: form.engine.value, name: form.name.value.trim(), password: form.password.value });
    form.reset();
    await refresh();
  }));
  $('#db-password-form').addEventListener('submit', guard(async event => {
    const form = event.target;
    await mutate('/api/databases/user-password', { engine: form.engine.value, name: form.name.value.trim(), password: form.password.value });
    form.reset();
    await refresh();
  }));
}
