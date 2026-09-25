import { $, api, mutate, guard, el, badge, button, message } from './ui.js';

const managerNames = { 'apt-get': 'APT', dnf: 'DNF', yum: 'YUM', zypper: 'ZYpp', apk: 'APK' };
const appNames = {
  nginx: 'Nginx', apache: 'Apache', docker: 'Docker', certbot: 'Certbot',
  mysql: 'MySQL', mariadb: 'MariaDB', postgresql: 'PostgreSQL', redis: 'Redis',
};

let version = 0, items = [], polling = false, job = {};

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

function renderJob() {
  const output = $('#app-job');
  if (!job.state) {
    output.hidden = true;
    return;
  }
  output.hidden = false;
  const label = { running: '进行中', done: '已完成', error: '失败' }[job.state] || job.state;
  let text = `安装 ${appNames[job.app] || job.app || ''} · ${label}\n`;
  if (job.error) text += `错误：${job.error}\n`;
  if (job.output) text += `\n${job.output}`;
  output.textContent = text;
}

async function refreshApps() {
  const request = ++version;
  const data = await api('/api/apps');
  if (request !== version) return;
  items = data.items || [];
  job = data.job || {};
  const manager = data.package_manager ? (managerNames[data.package_manager] || data.package_manager) : '';
  $('#app-manager').textContent = manager ? `软件包管理器：${manager}` : '未识别到受支持的软件包管理器（apt / dnf / yum / zypper / apk）';
  renderApps();
  renderJob();
  pollJob();
}

async function pollJob() {
  if (polling) return;
  polling = true;
  try {
    for (;;) {
      if (job.state !== 'running') return;
      await new Promise(resolve => setTimeout(resolve, 3000));
      job = await api('/api/apps/job');
      renderJob();
      if (job.state === 'done') {
        message(`${appNames[job.app] || job.app} 安装完成，可前往「站点管理」部署站点。`);
        await refreshApps();
      } else if (job.state === 'error') {
        message(`${appNames[job.app] || job.app} 安装失败：${job.error}`, true);
        await refreshApps();
      }
    }
  } catch (error) {
    message(error.message, true);
  } finally {
    polling = false;
  }
}

async function install(item) {
  await mutate('/api/apps/install', { name: item.name });
  job = { app: item.name, state: 'running' };
  renderJob();
  await pollJob();
}

export async function apps() {
  await refreshApps();
}

export function setupApps() {
  $('#app-refresh').addEventListener('click', guard(refreshApps));
}
