import { $, api, mutate, guard, el, button, table, badge, confirmAction, readOnly } from './ui.js';
import { files } from './files.js';

const engineNames = { nginx: 'Nginx', apache: 'Apache', docker: 'Docker' };
const kindNames = { static: '静态站点', proxy: '反向代理', container: '容器' };

let go = () => {};
let version = 0, environment = [], items = [];

function siteState(site) {
  if (site.engine === 'docker') return site.state === 'running' ? ['运行中', 'good'] : ['已停止', 'warn'];
  return site.state === 'active' ? ['服务运行中', 'good'] : ['服务未运行', 'warn'];
}

function renderEnvironment() {
  const available = environment.filter(info => info.installed);
  $('#site-env-count').textContent = environment.length ? `${available.length} / ${environment.length} 可用` : '—';
  if (!environment.length) {
    $('#site-environment').replaceChildren(el('article', '未能检测运行环境。', 'metric-card'));
    return;
  }
  $('#site-environment').replaceChildren(...environment.map(info => {
    const card = el('article', undefined, 'metric-card');
    card.append(el('h3', engineNames[info.engine] || info.engine));
    const state = info.installed
      ? (info.running ? ['已安装 · 运行中', 'good'] : ['已安装 · 未运行', ''])
      : ['未安装', 'warn'];
    card.append(badge(state[0], state[1]));
    card.append(el('p', info.version || info.detail || '—'));
    return card;
  }));
}

function syncEngineOptions() {
  const installed = Object.fromEntries(environment.map(info => [info.engine, info.installed]));
  document.querySelectorAll('#site-form [name=engine] option').forEach(option => {
    if (option.value === 'auto') return;
    option.disabled = !installed[option.value];
  });
  const select = $('#site-form [name=engine]');
  if (select.selectedOptions[0].disabled) select.value = 'auto';
  syncForm();
}

function syncForm() {
  const docker = $('#site-form [name=engine]').value === 'docker';
  $('#site-docker-fields').hidden = !docker;
  $('#site-root-label').hidden = docker;
  $('#site-help').textContent = docker
    ? '将以站点名称启动容器（lightpanel-名称），映射监听端口到容器端口，并配置自动重启。镜像会按需自动拉取。'
    : '部署方式选择「自动识别」时，面板会检测已安装的 Nginx 或 Apache 并创建静态站点配置；选择「Docker 容器」则按镜像启动一个自动重启的容器并映射端口。';
}

function renderSites() {
  $('#site-count').textContent = String(items.length);
  const rows = items.map(site => {
    const name = el('div', undefined, 'file-name');
    name.append(el('span', site.engine === 'docker' ? '▣' : '▤', 'file-icon'));
    name.append(site.server_names.join(', '));
    const kind = el('span', undefined, undefined);
    kind.textContent = `${engineNames[site.engine] || site.engine} · ${kindNames[site.kind] || site.kind}`;
    const ports = site.ports.length ? site.ports.join(', ') : '—';
    const [text, state] = siteState(site);
    const actions = el('div', undefined, 'actions');
    if (site.engine === 'docker') {
      if (site.managed) {
        if (site.state === 'running') actions.append(button('停止', () => act(site, 'stop'), true));
        else actions.append(button('启动', () => act(site, 'start'), true));
        actions.append(button('删除', () => removeSite(site), true, 'danger-text'));
      }
    } else {
      if (site.root && site.root !== '/' && site.kind !== 'proxy') {
        actions.append(button('打开目录', () => { go('files'); files(site.root, 0); }));
      }
      actions.append(button('重载配置', () => act(site, 'reload'), true));
      if (site.managed) actions.append(button('删除', () => removeSite(site), true, 'danger-text'));
    }
    const source = site.managed ? el('span', site.detail + ' · 面板创建') : site.detail;
    return [name, kind, ports, badge(text, state), source, actions];
  });
  table('#site-list', ['站点', '类型', '端口', '状态', '来源', '操作'], rows,
    '还没有发现任何站点', '安装 Nginx / Apache 或 Docker 后点击「重新检测」，或使用下方表单部署第一个站点。');
}

async function act(site, op) {
  await mutate('/api/sites/action', { id: site.id, op, engine: site.engine });
  await sites();
}

async function removeSite(site) {
  const isDocker = site.engine === 'docker';
  const confirmed = await confirmAction({
    title: '删除这个站点？',
    description: isDocker
      ? '将强制移除容器；容器内的数据不会随站点删除而保留，请确认已有所需备份。'
      : '将删除站点配置文件（仅限 LightPanel 创建的配置）并重载 Web 服务器；站点目录中的文件不会被删除。',
    target: `${site.server_names.join(', ')} · ${site.detail}`,
    confirm: '确认删除',
  });
  if (!confirmed) return;
  await mutate('/api/sites/action', { id: site.id, op: 'delete', engine: site.engine });
  await sites();
}

export async function sites() {
  const request = ++version;
  const data = await api('/api/sites');
  if (request !== version) return;
  environment = data.environment || [];
  items = data.items || [];
  renderEnvironment();
  syncEngineOptions();
  renderSites();
}

export function setupSites(navigate) {
  go = navigate;
  $('#site-detect').addEventListener('click', guard(sites));
  $('#site-form [name=engine]').addEventListener('change', syncForm);
  $('#site-form').addEventListener('submit', guard(async () => {
    const data = Object.fromEntries(new FormData($('#site-form')));
    if (data.engine === 'docker' && !data.image?.trim()) throw new Error('Docker 部署需要填写镜像名称。');
    if (readOnly) throw new Error('当前为只读模式，不能修改服务器。');
    await mutate('/api/sites/create', data);
    $('#site-create-panel').open = false;
    $('#site-form').reset();
    syncForm();
    await sites();
  }));
  syncForm();
}
