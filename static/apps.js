import { $, api, mutate, guard, el, badge, button, confirmAction, message } from './ui.js';
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
    // 同类型互斥：一个分组（如两个网页服务器）只允许安装一个，
    // 已被同分组其他应用占用时不再提供安装入口。
    const blockedBy = item.group && !item.installed
      ? items.find(other => other.installed && other.group === item.group && other.name !== item.name)
      : null;
    const state = item.installed
      ? (item.running ? ['已安装 · 运行中', 'good'] : ['已安装', 'good'])
      : blockedBy
        ? [`已被 ${blockedBy.title || blockedBy.name} 占用`, 'warn']
        : ['未安装', 'warn'];
    card.append(badge(state[0], state[1]));
    card.append(el('p', item.description || '—'));
    const meta = el('p', undefined, 'muted');
    meta.textContent = item.installed
      ? (item.version || '已安装')
      : blockedBy
        ? `已由 ${blockedBy.title || blockedBy.name} 提供同类服务，无法重复安装（可先卸载）`
        : item.package
          ? `软件包：${item.package}`
          : '当前软件包管理器不提供此应用';
    card.append(meta);
    if (!item.installed && !blockedBy && item.package) card.append(button('安装', () => install(item)));
    if (item.installed && item.package) card.append(button('卸载', () => uninstall(item), true, 'danger-text'));
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

async function uninstall(item) {
  const confirmed = await confirmAction({
    title: `卸载 ${appNames[item.name] || item.title || item.name}？`,
    description: '将通过系统软件包管理器卸载该软件，并一并删除其配置文件（APT 下为 purge）。数据库类软件的数据文件可能随卸载被删除且无法恢复，卸载前请确认已完成备份。',
    target: appNames[item.name] || item.title || item.name,
    confirm: '确认卸载',
  });
  if (!confirmed) return;
  const result = await mutate('/api/apps/remove', { name: item.name });
  if (result?.task_id) openTaskCenter(result.task_id);
}

export async function apps() {
  await refreshApps();
}

export function setupApps() {
  $('#app-refresh').addEventListener('click', guard(refreshApps));
  window.addEventListener('task-finished', event => {
    const kind = event.detail?.kind;
    if (kind !== 'app-install' && kind !== 'app-remove') return;
    // 数据库类安装完成引导到「数据库管理」，其余（网页服务器、Docker、
    // certbot）引导到「站点管理」。
    const name = (event.detail.title || '').replace(/^安装 |^卸载 /, '');
    const home = ['mysql', 'mariadb', 'postgresql', 'redis'].includes(name) ? '数据库管理' : '站点管理';
    message(event.detail.state === 'done'
      ? (kind === 'app-install'
        ? `${event.detail.title}完成，${name}已成功安装，可前往「${home}」使用。`
        : `${event.detail.title}完成。`)
      : `${event.detail.title}失败，详情见任务中心。`, event.detail.state !== 'done');
    refreshApps().catch(() => {});
  });
}
