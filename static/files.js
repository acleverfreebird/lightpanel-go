import { $, api, mutate, guard, el, button, table, confirmAction } from './ui.js';
import { size, normalizePath } from './format.js';

let currentPath = '.', currentOffset = 0, version = 0;
export async function files(path = currentPath, offset = currentOffset) {
  const request = ++version;
  const data = await api('/api/files?' + new URLSearchParams({ path, offset }));
  if (request !== version) return;
  currentPath = path; currentOffset = offset;
  $('#file-path-form [name=path]').value = path;
  $('#file-up').disabled = path === '.';
  const crumbs = [button('文件空间', () => files('.', 0))];
  const parts = path.split('/').filter(part => part && part !== '.');
  parts.forEach((part, index) => {
    crumbs.push(el('span', '/'), button(part, () => files(parts.slice(0, index + 1).join('/'), 0)));
  });
  $('#file-breadcrumbs').replaceChildren(...crumbs);
  table('#file-list', ['名称', '大小', '权限', '操作'], data.items.map(file => {
    const name = el('div', undefined, 'file-name');
    name.append(el('span', file.is_dir ? '▰' : '▤', `file-icon ${file.is_dir ? 'folder' : ''}`));
    name.append(file.is_dir ? button(file.name, () => files(file.path, 0), false, 'file-link') : el('span', file.name + (file.regular ? '' : '（特殊文件 / 链接）')));
    const actions = el('div', undefined, 'actions');
    if (file.regular) {
      const download = el('a', '下载');
      download.href = '/api/file/download?' + new URLSearchParams({ path: file.path }); download.download = file.name;
      actions.append(download, button('权限', async () => {
        const mode = await confirmAction({ title: '修改文件权限', description: '设置此文件的读、写和执行权限。', target: file.path, confirm: '保存权限', danger: false, input: file.mode });
        if (mode === null) return;
        await mutate('/api/file/chmod', { path: file.path, mode }); await files();
      }, true));
    }
    actions.append(button('删除', async () => {
      if (!await confirmAction({ title: '删除这个条目？', description: file.is_dir ? '只能删除空目录。删除后无法通过面板恢复。' : '删除后无法通过面板恢复，请确认已有所需备份。', target: file.path, confirm: '确认删除' })) return;
      await mutate('/api/file/delete', { path: file.path }); await files();
    }, true, 'danger-text'));
    return [name, file.is_dir ? '目录' : file.regular ? size(file.size) : '—', file.mode, actions];
  }), '这个目录还是空的', '可以选择文件上传，或通过路径栏前往其他目录。');
  $('#file-page').textContent = `第 ${Math.floor(offset / 200) + 1} 页 · 本页 ${data.items.length} 项`;
  $('#file-prev').disabled = offset === 0; $('#file-next').disabled = !data.more;
}
export function setupFiles() {
  $('#file-path-form').addEventListener('submit', guard(() => files(normalizePath(new FormData($('#file-path-form')).get('path')), 0)));
  $('#file-up').addEventListener('click', guard(() => files(currentPath.split('/').slice(0, -1).join('/') || '.', 0)));
  $('#file-prev').addEventListener('click', guard(() => files(currentPath, Math.max(0, currentOffset - 200))));
  $('#file-next').addEventListener('click', guard(() => files(currentPath, currentOffset + 200)));
  $('#upload-form input').addEventListener('change', () => {
    const file = $('#upload-form input').files[0];
    $('#upload-name').textContent = file ? `${file.name} · ${size(file.size)}` : '未选择文件 · 最大 32 MiB';
  });
  $('#upload-form').addEventListener('submit', guard(async () => {
    const file = $('#upload-form input').files[0];
    if (!file) return;
    if (file.size > 32 * 1024 * 1024) throw new Error('文件超过 32 MiB 上限，请选择更小的文件。');
    const destination = currentPath;
    const path = (destination === '.' ? '' : destination + '/') + file.name;
    await mutate('/api/file/upload?' + new URLSearchParams({ path }), file, true);
    $('#upload-form').reset(); $('#upload-name').textContent = '未选择文件 · 最大 32 MiB';
    await files();
  }));
}
