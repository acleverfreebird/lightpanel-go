import { $, api, mutate, guard, el, button, table, confirmAction, readOnly } from './ui.js';
import { size, normalizePath, time } from './format.js';

const maxUploadMB = Number(document.body.dataset.maxUpload) || 32;
const maxEdit = 1024 * 1024;
const hintDefault = () => `未选择文件 · 最大 ${maxUploadMB} MiB`;

let currentPath = '.', currentOffset = 0, version = 0, items = [], sortMode = 'name', filterText = '';

const sorters = {
  'name': (a, b) => dirsFirst(a, b) || a.name.localeCompare(b.name, 'zh-Hans-CN'),
  'name-desc': (a, b) => dirsFirst(a, b) || b.name.localeCompare(a.name, 'zh-Hans-CN'),
  'size': (a, b) => dirsFirst(a, b) || a.size - b.size,
  'size-desc': (a, b) => dirsFirst(a, b) || b.size - a.size,
  'mtime': (a, b) => dirsFirst(a, b) || b.modified - a.modified,
};
const dirsFirst = (a, b) => (b.is_dir ? 1 : 0) - (a.is_dir ? 1 : 0);

function render() {
  const term = filterText.trim().toLowerCase();
  const shown = items.filter(item => !term || item.name.toLowerCase().includes(term)).sort(sorters[sortMode] || sorters.name);
  const rows = shown.map(item => {
    const name = el('div', undefined, 'file-name');
    name.append(el('span', item.is_dir ? '▰' : '▤', `file-icon ${item.is_dir ? 'folder' : ''}`));
    name.append(item.is_dir ? button(item.name, () => files(item.path, 0), false, 'file-link') : el('span', item.name + (item.regular ? '' : '（特殊文件 / 链接）')));
    const actions = el('div', undefined, 'actions');
    if (item.regular) {
      const download = el('a', '下载');
      download.href = '/api/file/download?' + new URLSearchParams({ path: item.path }); download.download = item.name;
      actions.append(download);
      if (item.size <= maxEdit) actions.append(button('编辑', () => openEditor(item), true));
      actions.append(button('权限', async () => {
        const mode = await confirmAction({ title: '修改文件权限', description: '设置此文件的读、写和执行权限。', target: item.path, confirm: '保存权限', danger: false, input: item.mode });
        if (mode === null) return;
        await mutate('/api/file/chmod', { path: item.path, mode }); await files();
      }, true));
    }
    actions.append(button('重命名', async () => {
      const name = await confirmAction({
        title: item.is_dir ? '重命名文件夹' : '重命名文件', description: '输入不含 / 的新名称；名称中带 / 可同时移动到子目录。',
        target: item.path, confirm: '保存名称', danger: false, input: item.name,
        inputLabel: '新名称', pattern: '[^/\\\\]+', maxLength: 255, placeholder: '新名称', hint: '不能包含 / 或 \\', numeric: false,
      });
      if (name === null || name === item.name) return;
      await mutate('/api/file/rename', { path: item.path, to: join(currentPath, name) }); await files();
    }, true));
    actions.append(button('删除', async () => {
      if (!await confirmAction({ title: '删除这个条目？', description: item.is_dir ? '将递归删除该目录及其全部内容，删除后无法通过面板恢复。' : '删除后无法通过面板恢复，请确认已有所需备份。', target: item.path, confirm: item.is_dir ? '递归删除' : '确认删除' })) return;
      await mutate('/api/file/delete', { path: item.path, recursive: item.is_dir ? 'true' : '' }); await files();
    }, true, 'danger-text'));
    return [name, item.is_dir ? '目录' : item.regular ? size(item.size) : '—', time(item.modified), item.mode, actions];
  });
  const empty = filterText.trim() ? ['没有匹配的条目', '调整筛选关键词，或清空后查看全部内容。'] : ['这个目录还是空的', '可以上传文件，或用「＋ 新建文件夹」建立目录结构。'];
  table('#file-list', ['名称', '大小', '修改时间', '权限', '操作'], rows, empty[0], empty[1]);
}

function join(dir, name) {
  return dir === '.' ? name : `${dir}/${name}`;
}

export async function files(path = currentPath, offset = currentOffset) {
  const request = ++version;
  const data = await api('/api/files?' + new URLSearchParams({ path, offset }));
  if (request !== version) return;
  currentPath = path; currentOffset = offset; items = data.items;
  $('#file-path-form [name=path]').value = path;
  $('#file-up').disabled = path === '.';
  const crumbs = [button('文件空间', () => files('.', 0))];
  const parts = path.split('/').filter(part => part && part !== '.');
  parts.forEach((part, index) => {
    crumbs.push(el('span', '/'), button(part, () => files(parts.slice(0, index + 1).join('/'), 0)));
  });
  $('#file-breadcrumbs').replaceChildren(...crumbs);
  render();
  $('#file-page').textContent = `第 ${Math.floor(offset / 200) + 1} 页 · 本页 ${data.items.length} 项`;
  $('#file-prev').disabled = offset === 0; $('#file-next').disabled = !data.more;
}

async function mkdir() {
  const name = await confirmAction({
    title: '新建文件夹', description: '在当前目录创建文件夹，名称中带 / 可以一次创建多级目录。',
    target: currentPath === '.' ? '文件空间根目录' : currentPath, confirm: '创建', danger: false, input: '',
    inputLabel: '文件夹名称', pattern: '.+', maxLength: 400, placeholder: '例如 backups 或 site/assets', hint: '可用 / 表示层级，不能以 / 开头', numeric: false,
  });
  if (name === null) return;
  await mutate('/api/file/mkdir', { path: join(currentPath, name) }); await files();
}

async function openEditor(item) {
  const data = await api('/api/file/read?' + new URLSearchParams({ path: item.path }));
  $('#editor-target').textContent = item.path;
  $('#editor-text').value = data.content;
  $('#editor-dialog').showModal();
  $('#editor-text').focus();
}

export function setupFiles() {
  $('#file-path-form').addEventListener('submit', guard(() => files(normalizePath(new FormData($('#file-path-form')).get('path')), 0)));
  $('#file-up').addEventListener('click', guard(() => files(currentPath.split('/').slice(0, -1).join('/') || '.', 0)));
  $('#file-prev').addEventListener('click', guard(() => files(currentPath, Math.max(0, currentOffset - 200))));
  $('#file-next').addEventListener('click', guard(() => files(currentPath, currentOffset + 200)));
  $('#file-filter').value = '';
  $('#file-filter').addEventListener('input', () => { filterText = $('#file-filter').value; render(); });
  $('#file-sort').addEventListener('change', () => { sortMode = $('#file-sort').value; render(); });
  $('#file-mkdir').addEventListener('click', guard(mkdir));
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
