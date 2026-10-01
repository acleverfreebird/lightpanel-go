import { $, api, mutate, guard, el, button, table, badge, confirmAction, message, readOnly } from './ui.js';
import { size, time } from './format.js';
import { openTaskCenter } from './tasks.js';

const engineNames = { mysql: 'MySQL', mariadb: 'MariaDB', postgresql: 'PostgreSQL', redis: 'Redis' };
const sqlEngines = ['mysql', 'mariadb', 'postgresql'];

let version = 0, data = { engines: [], databases: {}, users: {}, units: {}, errors: {} };
let search = '';
let currentEngine = '', currentDb = '';
let backupTimer = null;
const revealed = new Set();

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
  syncCharsetOptions();
}

function installedEngines() {
  return sqlEngines.filter(engine => data.engines.find(info => info.engine === engine)?.installed);
}

function nameCell(name) {
  const cell = el('div', undefined, 'file-name');
  cell.append(el('span', '▦', 'file-icon'), name);
  return cell;
}

function passwordCell(engine, db) {
  const key = `${engine}/${db.name}`;
  if (!db.password) return el('span', '—', 'muted');
  const wrap = el('div', undefined, 'password-cell');
  const text = el('code', revealed.has(key) ? db.password : '••••••••', 'password-text');
  text.title = revealed.has(key) ? db.password : '点击「查看」显示密码';
  wrap.append(text);
  const toggle = button(revealed.has(key) ? '隐藏' : '查看', () => {
    if (revealed.has(key)) revealed.delete(key); else revealed.add(key);
    renderDatabases();
  });
  toggle.classList.add('password-toggle');
  wrap.append(toggle);
  wrap.append(button('复制', () => copyText(db.password)));
  return wrap;
}

function databaseActions(engine, db) {
  const cell = el('div', undefined, 'actions');
  cell.append(button('备份', () => openBackupDialog(engine, db.name)));
  cell.append(button('SQL', () => openQueryDialog(engine, db.name)));
  cell.append(button('改密', () => prefillPassword(engine, db.user || db.name)));
  cell.append(button('删除', () => dropDatabase(engine, db.name), true, 'danger-text'));
  return cell;
}

function renderDatabases() {
  const total = sqlEngines.reduce((n, engine) => n + (data.databases[engine]?.length || 0), 0);
  $('#db-count').textContent = String(total);
  const rows = [];
  for (const engine of sqlEngines) {
    for (const db of data.databases[engine] || []) {
      if (search && !db.name.toLowerCase().includes(search)) continue;
      rows.push([
        nameCell(db.name),
        engineNames[engine],
        db.user || '—',
        passwordCell(engine, db),
        db.charset || '默认',
        typeof db.size === 'number' ? size(db.size) : '—',
        databaseActions(engine, db),
      ]);
    }
  }
  table('#db-list', ['数据库', '引擎', '用户名', '密码', '字符集', '大小', '操作'], rows,
    total ? '没有匹配的数据库' : '还没有数据库',
    total ? '换个关键词试试。' : '数据库在引擎首次安装后出现，也可以用右上角「添加数据库」一步创建库和账号。');
}

function renderUsers() {
  const rows = [];
  for (const engine of sqlEngines) {
    for (const entry of data.users[engine] || []) {
      const [name, host = ''] = entry.split('\t');
      const cell = el('div', undefined, 'file-name');
      cell.append(el('span', '◎', 'file-icon'), name);
      rows.push([cell, host || 'localhost', engineNames[engine],
        button('修改密码', () => prefillPassword(engine, name), true)]);
    }
  }
  $('#db-user-count').textContent = String(rows.length);
  table('#db-user-list', ['用户', '主机', '引擎', '操作'], rows,
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

// ---- 添加数据库（一步建库 + 建号 + 授权） ----

function randomPassword(len = 16) {
  // 去掉易混淆字符（0/O、1/l/I）的字母表，全部字符都通过面板与 MySQL 的校验。
  const alphabet = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789!@#$%^&*-_=+';
  // 256 不整除 68：先丢弃落入尾部的字节再取模，否则前几个字符出现概率偏高。
  const limit = Math.floor(256 / alphabet.length) * alphabet.length;
  const bytes = new Uint8Array(len);
  let out = '';
  while (out.length < len) {
    crypto.getRandomValues(bytes);
    for (const b of bytes) {
      if (b >= limit) continue;
      out += alphabet[b % alphabet.length];
      if (out.length === len) break;
    }
  }
  return out;
}

// 用户名上限是 32 字符（ValidDBUser），库名可到 63：同名账号照抄库名会在
// 超长库名下变成非法默认值，截取前 32 位保证一步建库的建议账号始终可用。
function sameNameUser(name) {
  return name.trim().slice(0, 32);
}

function syncCharsetOptions() {
  const form = $('#db-create-form');
  if (!form) return;
  const isPG = form.elements.engine.value === 'postgresql';
  const charset = form.elements.charset;
  charset.disabled = isPG;
  charset.title = isPG ? 'PostgreSQL 使用引擎默认编码' : '';
  $('#db-charset-label')?.querySelector('small')?.replaceChildren(
    document.createTextNode(isPG ? 'PostgreSQL 暂不支持在面板中选择编码' : 'MySQL / MariaDB 数据库的默认字符集'));
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = el('textarea');
    ta.value = text;
    document.body.append(ta);
    ta.select();
    document.execCommand('copy');
    ta.remove();
  }
  message('已复制到剪贴板。');
}

function openCreateDialog() {
  const installed = installedEngines();
  if (!installed.length) {
    message('尚未检测到 MySQL / MariaDB / PostgreSQL，请先在「应用商店」安装。', true);
    return;
  }
  const form = $('#db-create-form');
  // 对话框在 #databases 区外，引擎选项在这里按检测状态同步。
  form.elements.engine.querySelectorAll('option').forEach(option => {
    option.disabled = !installed.includes(option.value);
  });
  if (!installed.includes(form.elements.engine.value)) form.elements.engine.value = installed[0];
  syncCharsetOptions();
  form.elements.password.value = randomPassword();
  // 每次打开都回到「用户名跟随库名」的默认状态。
  form.elements.user.dataset.touched = '';
  form.elements.user.value = sameNameUser(form.elements.name.value);
  form.elements.host.hidden = form.elements.access.value !== 'custom';
  $('#db-create-dialog').showModal();
}

function showCredentials(engine, name, user, password, host, charset) {
  const list = $('#db-credential-list');
  const items = [
    ['数据库', name],
    ['引擎', engineNames[engine] || engine],
    ['用户名', user],
    ['密码', password],
    ['访问权限', host || 'localhost'],
    ['字符集', charset || '引擎默认'],
  ];
  // dt/dd 必须是 .info-list 网格的直接子元素。
  list.replaceChildren(...items.flatMap(([label, value]) => {
    const dd = el('dd');
    dd.append(el('code', value), button('复制', () => copyText(value)));
    return [el('dt', label), dd];
  }));
  $('#db-credential-dialog').dataset.copyAll = JSON.stringify({ engine, name, user, password, host });
  $('#db-credential-dialog').showModal();
}

async function submitCreate(event) {
  const form = event.target;
  const engine = form.elements.engine.value;
  const name = form.elements.name.value.trim();
  const user = form.elements.user.value.trim() || sameNameUser(name);
  const password = form.elements.password.value;
  const charset = form.elements.charset.disabled ? '' : form.elements.charset.value;
  const access = form.elements.access.value;
  const host = access === 'custom' ? form.elements.host.value.trim() : access;
  if (access === 'custom' && !host) throw new Error('请填写自定义访问地址。');
  await mutate('/api/databases/create', { engine, name, user, password, charset, host });
  $('#db-create-dialog').close();
  form.reset();
  showCredentials(engine, name, user, password, host, charset);
  await refresh();
}

// ---- 备份中心 ----

async function refreshBackups() {
  const list = await api(`/api/databases/backups?engine=${encodeURIComponent(currentEngine)}&name=${encodeURIComponent(currentDb)}`);
  if (!$('#db-backup-dialog').open) return;
  const rows = (list.backups || []).map(entry => {
    const cell = el('div', undefined, 'actions');
    const download = el('a', '下载', 'text-button');
    download.href = `/api/databases/backup/download?engine=${encodeURIComponent(currentEngine)}&name=${encodeURIComponent(currentDb)}&file=${encodeURIComponent(entry.file)}`;
    download.setAttribute('download', entry.file);
    cell.append(download);
    cell.append(button('恢复', () => restoreBackup(entry.file), true));
    cell.append(button('删除', () => deleteBackup(entry.file), true, 'danger-text'));
    return [time(entry.modified), size(entry.size), cell];
  });
  table('#db-backup-list', ['备份时间', '大小', '操作'], rows, '暂无备份', '点击上方「立即备份」创建第一个备份。');
}

function watchBackupDialog() {
  // 对话框打开期间轮询，任务完成后列表自动出现新备份。
  clearInterval(backupTimer);
  backupTimer = setInterval(() => {
    if (!$('#db-backup-dialog').open) { clearInterval(backupTimer); return; }
    refreshBackups().catch(() => {});
  }, 4000);
}

async function openBackupDialog(engine, name) {
  currentEngine = engine;
  currentDb = name;
  $('#db-backup-target').textContent = `${name} · ${engineNames[engine]}`;
  $('#db-backup-dialog').showModal();
  await refreshBackups();
  watchBackupDialog();
}

async function createBackup() {
  const result = await mutate('/api/databases/backup', { engine: currentEngine, name: currentDb });
  if (result?.task_id) openTaskCenter(result.task_id);
}

async function restoreBackup(file) {
  const confirmed = await confirmAction({
    title: '恢复这个备份？',
    description: '将把备份中的数据重新导入数据库，覆盖现有同名表的数据，无法撤销。',
    target: `${currentDb} · ${file}`,
    confirm: '确认恢复',
  });
  if (!confirmed) return;
  const result = await mutate('/api/databases/backup/restore', { engine: currentEngine, name: currentDb, file });
  if (result?.task_id) openTaskCenter(result.task_id);
}

async function deleteBackup(file) {
  const confirmed = await confirmAction({
    title: '删除这个备份文件？',
    description: '将从服务器上永久删除该备份文件。',
    target: file,
    confirm: '确认删除',
  });
  if (!confirmed) return;
  await mutate('/api/databases/backup/delete', { engine: currentEngine, name: currentDb, file });
  await refreshBackups();
}

// ---- SQL 控制台 ----

function openQueryDialog(engine, name) {
  currentEngine = engine;
  currentDb = name;
  $('#db-query-target').textContent = `${name} · ${engineNames[engine]}`;
  $('#db-query-error').hidden = true;
  $('#db-query-dialog').showModal();
  $('#db-query-sql').focus();
}

// mysql --batch 会把值里的制表符、换行和反斜杠转义，这里还原单个字段。
function unescapeMysqlField(field) {
  let out = '';
  for (let i = 0; i < field.length; i++) {
    if (field[i] === '\\' && i + 1 < field.length) {
      const next = field[++i];
      out += next === 't' ? '\t' : next === 'n' ? '\n' : next === '0' ? '' : next;
    } else {
      out += field[i];
    }
  }
  return out;
}

const maxQueryRows = 200;

function renderQueryResult(engine, output) {
  const host = $('#db-query-result');
  const lines = output ? output.split('\n') : [];
  while (lines.length && lines[lines.length - 1] === '') lines.pop();
  if (!lines.length) {
    host.replaceChildren(el('p', '执行成功（无返回结果）。', 'muted'));
    return;
  }
  if (engine === 'postgresql' && !/^\(\d+ rows?\)$/.test(lines.at(-1) || '')) {
    // 语句标签（INSERT 0 1 / SET / ALTER TABLE …）：没有结果集，按文本展示。
    const pre = el('div', undefined, 'muted');
    pre.textContent = lines.join('\n');
    host.replaceChildren(pre);
    return;
  }
  const body = engine === 'postgresql' ? lines.filter(line => !/^\(\d+ rows?\)$/.test(line)) : lines;
  const cells = body.map(line => line.split('\t').map(field => engine === 'postgresql' ? field : unescapeMysqlField(field)));
  const headers = (cells.shift() || []).map(text => text || '列');
  table('#db-query-result', headers, cells.slice(0, maxQueryRows).map(row => row.map(v => v ?? 'NULL')),
    '查询完成，0 行结果', '查询已执行，但没有返回任何行。');
  if (cells.length > maxQueryRows) {
    host.append(el('p', `结果较多，仅显示前 ${maxQueryRows} 行（共 ${cells.length} 行）。`, 'muted'));
  }
}

async function runQuery() {
  const sql = $('#db-query-sql').value;
  if (!sql.trim()) throw new Error('请输入要执行的 SQL。');
  const errorBox = $('#db-query-error');
  errorBox.hidden = true;
  const result = await mutate('/api/databases/query', { engine: currentEngine, name: currentDb, sql });
  $('#db-query-result').replaceChildren();
  try {
    renderQueryResult(currentEngine, result?.output || '');
  } catch (err) {
    errorBox.textContent = `输出解析失败：${err.message}`;
    errorBox.hidden = false;
  }
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
  $('#db-search').addEventListener('input', guard(async event => {
    search = event.target.value.trim().toLowerCase();
    renderDatabases();
  }));
  $('#db-add').addEventListener('click', guard(openCreateDialog));
  const createForm = $('#db-create-form');
  createForm.elements.name.addEventListener('input', () => {
    // 用户名跟随库名，直到手动修改过为止。
    if (!createForm.elements.user.dataset.touched) createForm.elements.user.value = sameNameUser(createForm.elements.name.value);
  });
  createForm.elements.user.addEventListener('input', () => {
    createForm.elements.user.dataset.touched = 'true';
  });
  createForm.elements.engine.addEventListener('change', syncCharsetOptions);
  createForm.elements.access.addEventListener('change', () => {
    createForm.elements.host.hidden = createForm.elements.access.value !== 'custom';
    if (!createForm.elements.host.hidden) createForm.elements.host.focus();
  });
  createForm.addEventListener('submit', guard(submitCreate));
  $('#db-pass-regen').addEventListener('click', () => {
    createForm.elements.password.value = randomPassword();
  });
  $('#db-credential-copy').addEventListener('click', guard(async () => {
    const info = JSON.parse($('#db-credential-dialog').dataset.copyAll || '{}');
    await copyText(`数据库：${info.name}\n用户名：${info.user}\n密码：${info.password}\n访问权限：${info.host || 'localhost'}`);
  }));
  $('#db-backup-now').addEventListener('click', guard(createBackup));
  $('#db-query-run').addEventListener('click', guard(runQuery));
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
  // 三个新对话框的关闭按钮；备份对话框关闭时停止轮询。
  for (const id of ['db-create-dialog', 'db-credential-dialog', 'db-backup-dialog', 'db-query-dialog']) {
    $(`#${id}`).querySelectorAll('[data-close]').forEach(node =>
      node.addEventListener('click', () => $(`#${id}`).close()));
    $(`#${id}`).addEventListener('close', () => {
      if (id === 'db-backup-dialog') clearInterval(backupTimer);
    });
  }
  if (readOnly) {
    for (const id of ['db-add', 'db-backup-now', 'db-query-run']) {
      const node = $(`#${id}`);
      node.disabled = true;
      node.title = '当前账号只有查看权限';
    }
  }
}
