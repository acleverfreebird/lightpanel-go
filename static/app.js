import { $, guard, message, readOnly } from './ui.js';
import { overview } from './overview.js';
import { diagnostics } from './diagnostics.js';
import { processes, services, setupProcesses, setupServices } from './services.js';
import { files, setupFiles } from './files.js';
import { logs, firewall, setupTools } from './tools.js';
import { sites, setupSites } from './sites.js';
import { databases, setupDatabases } from './databases.js';
import { apps, setupApps } from './apps.js';
import { setupUpdate } from './update.js';
import { hydrateIcons } from './icons.js';

hydrateIcons();

const pageKickers = {
  overview: 'OVERVIEW / 系统总览', processes: 'OPERATIONS / 进程',
  services: 'OPERATIONS / 服务', apps: 'DEPLOY / 应用', sites: 'DEPLOY / 站点',
  databases: 'DEPLOY / 数据', files: 'OPERATIONS / 文件',
  logs: 'OPERATIONS / 日志', firewall: 'SECURITY / 访问控制',
};

const pages = {
  overview: ['系统概览', '掌握资源使用情况，让每一次运维都有据可循。', () => Promise.all([overview(), diagnostics()])],
  processes: ['进程管理', '定位资源占用，安全地管理正在运行的进程。', processes],
  services: ['系统服务', '快速筛选服务状态，查看详情并执行维护操作。', services],
  apps: ['应用商店', '一键安装网页服务器与配套组件，部署环境一步到位。', apps],
  sites: ['站点管理', '自动识别网页服务器与 Docker，部署和管理站点。', sites],
  databases: ['数据库管理', '识别数据库引擎，管理数据库、用户与服务状态。', databases],
  files: ['文件管理', '浏览和管理服务器上的全部文件与目录。', files],
  logs: ['系统日志', '从系统事件到服务日志，让问题排查更有方向。', logs],
  firewall: ['防火墙', '查看访问规则，按端口与协议管理服务器连接。', firewall],
};
let current = 'overview';
const pending = new Map(), updated = new Map();
function refreshState() {
  const busy = pending.has(current);
  $('#refresh').disabled = busy;
  $('#loading').hidden = !busy;
  $(`#${current}`).setAttribute('aria-busy', String(busy));
  $('#updated').textContent = updated.has(current) ? `更新于 ${updated.get(current)}` : '尚未更新';
}
async function refresh(replace = false) {
  const requested = current;
  if (pending.has(requested) && !replace) return;
  const request = Symbol(requested);
  pending.set(requested, request);
  refreshState();
  try {
    await pages[requested][2]();
    if (pending.get(requested) !== request) return;
    updated.set(requested, new Date().toLocaleTimeString());
    $('#connection').textContent = '已连接';
    $('#connection').className = 'badge good';
  } catch (error) {
    if (pending.get(requested) !== request) return;
    $('#connection').textContent = '更新失败';
    $('#connection').className = 'badge warn';
    message(`${pages[requested][0]}：${error.message}`, true);
  } finally {
    if (pending.get(requested) === request) {
      pending.delete(requested);
      $(`#${requested}`).setAttribute('aria-busy', 'false');
    }
    refreshState();
  }
}
function navigate(name, focus = false) {
  current = Object.hasOwn(pages, name) ? name : 'overview';
  document.querySelectorAll('main > section').forEach(section => section.hidden = section.id !== current);
  document.querySelectorAll('[data-tab]').forEach(control => {
    control.classList.toggle('active', control.dataset.tab === current);
    if (control.dataset.tab === current) control.setAttribute('aria-current', 'page');
    else control.removeAttribute('aria-current');
  });
  $('#top-title').textContent = $('#page-title').textContent = pages[current][0];
  $('#page-description').textContent = pages[current][1];
  $('#page-kicker').textContent = pageKickers[current];
  document.title = `${pages[current][0]} · LightPanel`;
  message('');
  if (focus) $('#workspace').focus({ preventScroll: true });
  refresh(true);
}
function go(name) {
  if (location.hash === `#${name}`) navigate(name, true);
  else location.hash = name;
}
document.querySelectorAll('[data-tab], [data-go]').forEach(control => control.addEventListener('click', () => go(control.dataset.tab || control.dataset.go)));
window.addEventListener('hashchange', () => navigate(location.hash.slice(1), true));
$('.skip-link').addEventListener('click', event => {
  event.preventDefault();
  $('#workspace').focus();
});
$('#refresh').addEventListener('click', guard(() => { message(''); return refresh(); }));
$('#message-close').addEventListener('click', () => message(''));
setupProcesses(); setupServices(go); setupFiles(); setupTools(); setupApps(); setupSites(go); setupDatabases(); setupUpdate();
if (readOnly) document.querySelectorAll('[data-mutation]').forEach(control => { control.disabled = true; control.title = '当前账号只有查看权限'; });
$('#auto-refresh').addEventListener('change', () => { if ($('#auto-refresh').checked && current === 'overview') refresh(); });
setInterval(() => { if (!document.hidden && current === 'overview' && $('#auto-refresh').checked) refresh(); }, 3000);
navigate(location.hash.slice(1));
