import { $, api, mutate, guard, el, badge, button, message } from './ui.js';
import { openTaskCenter } from './tasks.js';

const managerNames = { 'apt-get': 'APT', dnf: 'DNF', yum: 'YUM', zypper: 'ZYpp', apk: 'APK' };
const appNames = {
  nginx: 'Nginx', apache: 'Apache', docker: 'Docker', certbot: 'Certbot',
  mysql: 'MySQL', mariadb: 'MariaDB', postgresql: 'PostgreSQL', redis: 'Redis',
};

let version = 0, items = [];

function renderApps() {
  const installed = items.filter(item => item.installed).length;
  $('#app-count').textContent = items.length ? `${installed} / ${items.length} 已安装` : '—';
  if (!items.length) {
    $('#app-list').replaceChildren(el('article', '未能读取应用目录。', 'metric-card'));
    return;
  }
  $('#app-list').replaceChildren(...items.map(item => {
    const card = el('article', undefined, 'metric-card');
    card.append(el('h3', appNames[item.name] || item.title || item.name));
    const state = item.installed
      ? (item.running ? ['已安装 · 运行中', 'good'] : ['已安装', 'good'])
      : ['未安装', 'warn'];
    card.append(badge(state[0], state[1]));
    card.append(el('p', item.description || '—'));
    const meta = el('p', undefined, 'muted');
    meta.textContent = item.installed
      ? (item.version || '已安装')
      : item.package
        ? `软件包：${item.package}`
        : '当前软件包管理器不提供此应用';
    card.append(meta);
    if (!item.installed && item.package) card.append(button('安装', () => install(item)));
    return card;
  }));
}

async function refreshApps() {
  const request = ++version;
  const data = await api('/api/apps');
  if (request !== version) return;
  items = data.items || [];
  const manager = data.package_manager ? (managerNames[data.package_manager] || data.package_manager) : '';
  $('#app-manager').textContent = manager ? `软件包管理器：${manager}` : '未识别到受支持的软件包管理器（apt / dnf / yum / zypper / apk）';
  renderApps();
}

async function install(item) {
  const result = await mutate('/api/apps/install', { name: item.name });
  if (result?.task_id) openTaskCenter(result.task_id);
}

export async function apps() {
  await refreshApps();
}

export function setupApps() {
  $('#app-refresh').addEventListener('click', guard(refreshApps));
  window.addEventListener('task-finished', event => {
    if (event.detail?.kind !== 'app-install') return;
    message(event.detail.state === 'done'
      ? `${event.detail.title}完成，可前往「站点管理」部署站点。`
      : `${event.detail.title}失败，详情见任务中心。`, event.detail.state !== 'done');
    refreshApps().catch(() => {});
  });
}
