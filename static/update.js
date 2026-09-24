import { $, api, mutate, guard, confirmAction, message } from './ui.js';

const current = document.body.dataset.version || 'dev';
let latest = null;

function setStatus(text) {
  $('#update-status').textContent = text;
}

export function setupUpdate() {
  $('#update-current').textContent = current;
  const applyButton = $('#update-apply');
  if (document.body.dataset.readonly === 'true') applyButton.disabled = true;
  $('#update-check').addEventListener('click', guard(async () => {
    setStatus('正在连接 GitHub…');
    const data = await api('/api/update/check');
    latest = data.update_available ? data.latest : null;
    if (!data.latest) {
      setStatus(`当前 ${data.current} · 仓库还没有发布版本`);
      applyButton.hidden = true;
    } else if (data.update_available) {
      setStatus(`发现新版本 ${data.latest}（当前 ${data.current}）`);
      applyButton.hidden = false;
    } else if (data.current === data.latest) {
      setStatus(`已是最新版本 ${data.latest}`);
      applyButton.hidden = true;
    } else {
      setStatus(`当前 ${data.current} · 最新发布 ${data.latest}（版本号无法比较，可手动更新）`);
      applyButton.hidden = false;
    }
  }));
  applyButton.addEventListener('click', guard(async () => {
    if (!latest) return;
    if (!await confirmAction({
      title: `更新到 ${latest}？`,
      description: '面板将下载并校验官方二进制，替换本机文件后自动重启服务。更新过程约需一分钟，完成后需要重新登录。',
      target: `${current} → ${latest}`,
      confirm: '下载并更新',
    })) return;
    await mutate('/api/update/apply', { tag: latest });
    message('更新完成，服务正在重启，页面将在几秒后刷新…');
    let attempts = 0;
    const timer = setInterval(() => {
      attempts++;
      fetch('/', { credentials: 'same-origin' }).then(response => {
        if (response.ok || attempts > 40) { clearInterval(timer); location.reload(); }
      }).catch(() => {});
    }, 3000);
  }));
}
