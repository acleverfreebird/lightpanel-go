import { $, api, mutate, guard, el, button, table, confirmAction, message, readOnly } from './ui.js';
import { size, normalizePath, resolveInputPath, time } from './format.js';

const maxUploadMB = Number(document.body.dataset.maxUpload) || 32;
const maxEdit = 1024 * 1024;
const hintDefault = () => `未选择文件 · 最大 ${maxUploadMB} MiB`;
const trashNote = '回收站位于服务器 /var/lib/lightpanel/trash';

let currentPath = '/', currentOffset = 0, version = 0, items = [], sortMode = 'name', filterText = '';
const selected = new Set(); // 勾选的完整路径，翻页后保留
let clipboard = null;       // { mode: 'copy' | 'cut', paths: [...] }
let trashItems = [];

const sorters = {
  'name': (a, b) => dirsFirst(a, b) || a.name.localeCompare(b.name, 'zh-Hans-CN'),
  'name-desc': (a, b) => dirsFirst(a, b) || b.name.localeCompare(a.name, 'zh-Hans-CN'),
  'size': (a, b) => dirsFirst(a, b) || a.size - b.size,
  'size-desc': (a, b) => dirsFirst(a, b) || b.size - a.size,
  'mtime': (a, b) => dirsFirst(a, b) || b.modified - a.modified,
};
const dirsFirst = (a, b) => (b.is_dir ? 1 : 0) - (a.is_dir ? 1 : 0);

function kindOf(item) {
  return item.is_dir ? '目录' : item.regular ? '文件' : item.symlink ? '链接' : '特殊';
}
function iconOf(item) {
  return item.is_dir ? '▰' : item.symlink ? '⇱' : '▤';
}

// reportBatch 汇总一次批量操作的结果：全部成功显示一条简讯，部分失败时
// 列出前几条服务器返回的原始原因。
function reportBatch(result, total, verb) {
  const failed = result?.failed || [];
  if (!failed.length) { message(`${verb} ${total} 项完成。`); return; }
  const detail = failed.slice(0, 2).map(f => `${f.path}：${f.error}`).join('；');
  message(`${verb}完成 ${total - failed.length} 项，${failed.length} 项失败：${detail}${failed.length > 2 ? '…' : ''}`, true);
}

function updateBatchBar() {
  $('#file-batch').hidden = selected.size === 0;
  $('#file-batch-count').textContent = `已选 ${selected.size} 项`;
}

function updateClipboardBar() {
  const active = clipboard !== null && clipboard.paths.length > 0;
  $('#file-clipboard').hidden = !active;
  if (!active) return;
  $('#file-clipboard-text').textContent = `剪贴板：${clipboard.paths.length} 个条目（${clipboard.mode === 'copy' ? '复制' : '剪切'}），粘贴到当前浏览的目录`;
  $('#file-paste').disabled = readOnly;
}

function render() {
  const term = filterText.trim().toLowerCase();
  const shown = items.filter(item => !term || item.name.toLowerCase().includes(term)).sort(sorters[sortMode] || sorters.name);
  const allBox = el('input');
  allBox.type = 'checkbox';
  allBox.checked = shown.length > 0 && shown.every(item => selected.has(item.path));
  allBox.indeterminate = !allBox.checked && shown.some(item => selected.has(item.path));
  allBox.title = '全选 / 取消全选本页';
  allBox.setAttribute('aria-label', '全选本页');
  allBox.addEventListener('change', () => {
    shown.forEach(item => allBox.checked ? selected.add(item.path) : selected.delete(item.path));
    updateBatchBar();
    render();
  });
  const rows = shown.map(item => {
    const box = el('input');
    box.type = 'checkbox';
    box.checked = selected.has(item.path);
    box.setAttribute('aria-label', `选择 ${item.name}`);
    box.addEventListener('change', () => {
      if (box.checked) selected.add(item.path); else selected.delete(item.path);
      updateBatchBar();
    });
    const name = el('div', undefined, 'file-name');
    name.append(el('span', iconOf(item), `file-icon ${item.is_dir ? 'folder' : ''}`));
    name.append(item.is_dir ? button(item.name, () => files(item.path, 0), false, 'file-link')
      : el('span', item.name + (item.regular ? '' : `（${kindOf(item)}）`)));
    const actions = el('div', undefined, 'actions');
    if (item.regular || item.symlink) {
      const download = el('a', '下载');
      download.href = '/api/file/download?' + new URLSearchParams({ path: item.path }); download.download = item.name;
      actions.append(download);
    }
    if (item.regular && item.size <= maxEdit) actions.append(button('编辑', () => openEditor(item), true));
    if (!item.symlink && (item.regular || item.is_dir)) actions.append(button('权限', async () => {
      const mode = await confirmAction({ title: item.is_dir ? '修改目录权限' : '修改文件权限', description: '设置读、写和执行权限（不含 setuid/setgid）。', target: item.path, confirm: '保存权限', danger: false, input: item.mode });
      if (mode === null) return;
      await mutate('/api/file/chmod', { path: item.path, mode }); await files();
    }, true));
    actions.append(button('重命名', async () => {
      const name = await confirmAction({
        title: item.is_dir ? '重命名文件夹' : '重命名文件', description: '输入新名称；路径中带 / 可同时移动到其他目录。',
        target: item.path, confirm: '保存名称', danger: false, input: item.name,
        inputLabel: '新路径', pattern: '.+', maxLength: 4096, placeholder: item.path, hint: '名称相对于当前目录；以 / 开头表示绝对路径；不会覆盖已有文件', numeric: false,
      });
      if (name === null) return;
      const to = resolveInputPath(name, currentPath);
      if (to === item.path) return;
      await mutate('/api/file/rename', { path: item.path, to }); await files();
    }, true));
    actions.append(button('删除', async () => {
      if (!await confirmAction({ title: '删除这个条目？', description: item.is_dir ? '将整个目录移入服务器回收站，可随时恢复或彻底清除。' : '将移入服务器回收站，可随时恢复或彻底清除。', target: item.path, confirm: '移入回收站' })) return;
      await mutate('/api/file/trash', { path: item.path }); await files();
    }, true, 'danger-text'));
    return [box, name, item.is_dir ? '目录' : item.regular ? size(item.size) : '—', time(item.modified), item.mode, actions];
  });
  const empty = filterText.trim() ? ['没有匹配的条目', '调整筛选关键词，或清空后查看全部内容。'] : ['这个目录还是空的', '可以上传文件，或用「＋ 新建文件夹」建立目录结构。'];
  table('#file-list', [allBox, '名称', '大小', '修改时间', '权限', '操作'], rows, empty[0], empty[1]);
}

function join(dir, name) {
  return dir === '/' ? `/${name}` : `${dir}/${name}`;
}
function parentOf(p) {
  return p === '/' ? '/' : p.slice(0, p.lastIndexOf('/')) || '/';
}

function batchBody(paths) {
  const body = new URLSearchParams();
  paths.forEach(p => body.append('path', p));
  return body;
}

async function trashSelected() {
  const paths = [...selected];
  if (!paths.length) return;
  if (!await confirmAction({ title: `删除选中的 ${paths.length} 项？`, description: '选中的条目将移入服务器回收站，可随时恢复或彻底清除。', target: paths.length === 1 ? paths[0] : `${paths.length} 个条目`, confirm: '移入回收站' })) return;
  const result = await mutate('/api/file/trash', batchBody(paths));
  selected.clear();
  await files();
  reportBatch(result, paths.length, '移入回收站');
}

async function pasteClipboard() {
  if (!clipboard?.paths.length) return;
  const copying = clipboard.mode === 'copy';
  // 剪切粘贴跳过已在当前目录的条目（原地剪切没有意义）；复制不做跳转，
  // 目标同名时由服务器逐项报错。
  const entries = copying ? clipboard.paths : clipboard.paths.filter(p => parentOf(p) !== currentPath);
  if (!entries.length) { message('所选条目已在当前目录，无需粘贴。'); return; }
  const body = batchBody(entries);
  body.set('to', currentPath);
  const result = await mutate(copying ? '/api/file/copy' : '/api/file/move', body);
  if (!copying) clipboard = null;
  updateClipboardBar();
  await files();
  reportBatch(result, entries.length, copying ? '复制' : '移动');
}

export async function files(path = currentPath, offset = currentOffset) {
  const request = ++version;
  const data = await api('/api/files?' + new URLSearchParams({ path, offset }));
  if (request !== version) return;
  currentPath = data.path; currentOffset = offset; items = data.items;
  $('#file-path-form [name=path]').value = data.path;
  $('#file-up').disabled = path === '/';
  const crumbs = [button('根目录', () => files('/', 0))];
  const parts = data.path.split('/').filter(Boolean);
  parts.forEach((part, index) => {
    crumbs.push(el('span', '/'), button(part, () => files('/' + parts.slice(0, index + 1).join('/'), 0)));
  });
  $('#file-breadcrumbs').replaceChildren(...crumbs);
  render();
  updateBatchBar();
  updateClipboardBar();
  $('#file-page').textContent = `第 ${Math.floor(offset / 200) + 1} 页 · 本页 ${data.items.length} 项`;
  $('#file-prev').disabled = offset === 0; $('#file-next').disabled = !data.more;
}

async function mkdir() {
  const name = await confirmAction({
    title: '新建文件夹', description: '在当前目录创建文件夹，路径中带 / 可以一次创建多级目录。',
    target: currentPath === '/' ? '根目录' : currentPath, confirm: '创建', danger: false, input: '',
    inputLabel: '文件夹路径', pattern: '.+', maxLength: 4096, placeholder: '例如 backups', hint: '在当前目录创建；以 / 开头可指定绝对路径', numeric: false,
  });
  if (name === null) return;
  await mutate('/api/file/mkdir', { path: resolveInputPath(name, currentPath) }); await files();
}

async function openEditor(item) {
  const data = await api('/api/file/read?' + new URLSearchParams({ path: item.path }));
  $('#editor-target').textContent = data.path;
  $('#editor-text').value = data.content;
  $('#editor-dialog').showModal();
  $('#editor-text').focus();
}

async function openTrash() {
  await loadTrash();
  $('#trash-dialog').showModal();
}

async function loadTrash() {
  const data = await api('/api/file/trash-list');
  trashItems = data.items || [];
  $('#trash-count').textContent = `共 ${trashItems.length} 项 · ${trashNote}`;
  $('#trash-empty').disabled = readOnly || trashItems.length === 0;
  renderTrash();
}

function renderTrash() {
  const rows = trashItems.map(item => {
    const actions = el('div', undefined, 'actions');
    actions.append(button('恢复', async () => {
      if (!await confirmAction({ title: '恢复这个条目？', description: '将恢复到删除时的原始位置；原位置已存在同名条目时无法恢复。', target: item.original, confirm: '恢复', danger: false })) return;
      await mutate('/api/file/trash/restore', { id: item.id });
      await loadTrash();
    }, true));
    actions.append(button('彻底删除', async () => {
      if (!await confirmAction({ title: '彻底删除这个条目？', description: '将从回收站永久删除，之后无法再恢复。', target: item.original, confirm: '彻底删除' })) return;
      await mutate('/api/file/trash/delete', { id: item.id });
      await loadTrash();
    }, true, 'danger-text'));
    const name = el('div', undefined, 'file-name');
    name.append(el('span', iconOf(item), `file-icon ${item.is_dir ? 'folder' : ''}`));
    name.append(el('span', item.name));
    return [name, item.is_dir ? '目录' : item.regular ? size(item.size) : '—', el('code', item.original, 'trash-origin'), time(item.deleted), actions];
  });
  table('#trash-list', ['名称', '大小', '原位置', '删除时间', '操作'], rows, '回收站是空的', '删除的文件与目录会先存放在服务器回收站中，可随时恢复。');
}

export function setupFiles() {
  $('#file-path-form').addEventListener('submit', guard(() => files(normalizePath(new FormData($('#file-path-form')).get('path')), 0)));
  $('#file-up').addEventListener('click', guard(() => files(parentOf(currentPath), 0)));
  $('#file-prev').addEventListener('click', guard(() => files(currentPath, Math.max(0, currentOffset - 200))));
  $('#file-next').addEventListener('click', guard(() => files(currentPath, currentOffset + 200)));
  $('#file-filter').value = '';
  $('#file-filter').addEventListener('input', () => { filterText = $('#file-filter').value; render(); });
  $('#file-sort').addEventListener('change', () => { sortMode = $('#file-sort').value; render(); });
  $('#file-mkdir').addEventListener('click', guard(mkdir));
  $('#file-trash-open').addEventListener('click', guard(openTrash));
  $('#file-copy-sel').addEventListener('click', guard(async () => {
    clipboard = { mode: 'copy', paths: [...selected] };
    updateClipboardBar();
  }));
  $('#file-cut-sel').addEventListener('click', guard(async () => {
    clipboard = { mode: 'cut', paths: [...selected] };
    updateClipboardBar();
  }));
  $('#file-trash-sel').addEventListener('click', guard(trashSelected));
  $('#file-clear-sel').addEventListener('click', guard(() => {
    selected.clear();
    render();
    updateBatchBar();
  }));
  $('#file-paste').addEventListener('click', guard(pasteClipboard));
  $('#file-clipboard-clear').addEventListener('click', guard(() => {
    clipboard = null;
    updateClipboardBar();
  }));
  $('#trash-dialog').querySelectorAll('[data-close]').forEach(node => node.addEventListener('click', () => $('#trash-dialog').close()));
  $('#trash-dialog').addEventListener('close', () => { files(); });
  $('#trash-empty').addEventListener('click', guard(async () => {
    if (!trashItems.length) return;
    if (!await confirmAction({ title: '清空回收站？', description: `将永久删除回收站中的全部 ${trashItems.length} 项，之后无法再恢复。`, target: '/var/lib/lightpanel/trash', confirm: '清空回收站' })) return;
    await mutate('/api/file/trash/empty', {});
    await loadTrash();
  }));
  $('#editor-cancel').addEventListener('click', () => $('#editor-dialog').close());
  $('#editor-form').addEventListener('submit', guard(async event => {
    event.preventDefault();
    const path = $('#editor-target').textContent;
    await mutate('/api/file/write?' + new URLSearchParams({ path }), $('#editor-text').value, true);
    $('#editor-dialog').close();
    await files();
  }));
  if (readOnly) { $('#file-mkdir').disabled = true; $('#editor-save').disabled = true; }
  $('#upload-form input').addEventListener('change', () => {
    const list = [...$('#upload-form input').files];
    $('#upload-name').textContent = list.length === 0 ? hintDefault()
      : list.length === 1 ? `${list[0].name} · ${size(list[0].size)}`
      : `${list.length} 个文件 · 共 ${size(list.reduce((total, file) => total + file.size, 0))}`;
  });
  $('#upload-form').addEventListener('submit', guard(async () => {
    const list = [...$('#upload-form input').files];
    if (!list.length) return;
    const limit = maxUploadMB * 1024 * 1024;
    const oversized = list.find(file => file.size > limit);
    if (oversized) throw new Error(`文件 ${oversized.name} 超过 ${maxUploadMB} MiB 上限，请选择更小的文件。`);
    for (const file of list) {
      await mutate('/api/file/upload?' + new URLSearchParams({ path: join(currentPath, file.name) }), file, true);
    }
    $('#upload-form').reset(); $('#upload-name').textContent = hintDefault();
    await files();
  }));
}
