import { $, api, mutate, guard, el, badge, table, confirmAction, readOnly, message } from './ui.js';
import { openTaskCenter } from './tasks.js';
import { files } from './files.js';

// 宝塔式网站管理：站点表格 + 「设置」弹窗（基本信息 / SSL / 域名）+ 添加站点。
// SSL 申请走面板内置 ACME 客户端（/api/sites/ssl），不依赖任何外部工具。
const engineNames = { nginx: 'Nginx', apache: 'Apache', docker: 'Docker' };
const kindNames = { static: '静态站点', proxy: '反向代理', container: '容器' };

let go = () => {};
let version = 0, certVersion = 0;
let environment = [], items = [], certs = [];
let selected = null; // 当前在设置弹窗中的站点

function siteState(site) {
  if (site.engine === 'docker') return site.state === 'running' ? ['运行中', 'good'] : ['已停止', 'bad'];
  return site.state === 'active' ? ['服务运行中', 'good'] : ['服务未运行', 'warn'];
}

function realDomain(site) {
  const name = site.server_names?.[0];
  return name && name !== '_' && !name.startsWith('*.') ? name : '';
}

const certFor = site => certs.find(c => c.domain === realDomain(site));
const certForDomain = domain => certs.find(c => c.domain === domain);

function fmtDate(value) {
  return value ? new Date(value).toLocaleDateString('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit' }) : '—';
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
      ? info.running ? ['已安装 · 运行中', 'good'] : ['已安装 · 未运行', 'warn']
      : ['未安装', 'warn'];
    card.append(badge(state[0], state[1]));
    card.append(el('p', info.version || info.detail || '—'));
    return card;
  }));
}

function siteFilter() {
  return ($('#site-search').value || '').trim().toLowerCase();
}

function renderSites() {
  const term = siteFilter();
  const shown = items.filter(site => !term || site.server_names.join(' ').toLowerCase().includes(term) || (site.root || '').toLowerCase().includes(term));
  $('#site-count').textContent = String(items.length);
  const rows = shown.map(site => {
    const name = el('div', undefined, 'file-name');
    name.append(el('span', site.engine === 'docker' ? '▣' : '▤', 'file-icon'));
    name.append(site.server_names.join(', '));
    const kind = el('span');
    kind.textContent = `${engineNames[site.engine] || site.engine} · ${kindNames[site.kind] || site.kind}`;
    const ports = site.ports.length ? site.ports.join(', ') : '—';
    const [text, state] = siteState(site);
    const cert = certFor(site);
    let sslBadge;
    if (site.engine === 'docker') sslBadge = el('span', '—', 'muted');
    else if (cert) {
      sslBadge = badge(cert.days_left <= 15 ? `剩余 ${cert.days_left} 天` : `有效期至 ${fmtDate(cert.not_after)}`, cert.days_left <= 15 ? 'warn' : 'good');
      sslBadge.title = cert.staging ? '测试证书（Let\'s Encrypt Staging）' : `${cert.issuer} · 签发于 ${fmtDate(cert.not_before)}`;
    } else if (site.ssl) sslBadge = badge('已启用', 'good');
    else sslBadge = badge('未启用', 'warn');
    const actions = el('div', undefined, 'actions');
    if (site.engine === 'docker') {
      if (site.managed) {
        actions.append(button('设置', () => openSiteDialog(site)));
        if (site.state === 'running') actions.append(action('停止', () => act(site, 'stop')));
        else actions.append(action('启动', () => act(site, 'start')));
        actions.append(action('删除', () => removeSite(site), true));
      }
    } else {
      actions.append(button('设置', () => openSiteDialog(site)));
      if (site.managed) actions.append(action('删除', () => removeSite(site), true));
    }
    const source = site.managed ? el('span', site.detail + ' · 面板创建', 'muted') : el('span', site.detail, 'muted');
    return [name, badge(text, state), kind, ports, sslBadge, source, actions];
  });
  table('#site-list', ['站点', '状态', '类型', '端口', 'SSL', '来源', '操作'], rows,
    term ? '没有匹配的站点' : '还没有发现任何站点',
    term ? '调整搜索关键词，或清除后查看全部。' : '安装 Nginx / Apache 或 Docker 后点击「重新检测」，或使用右上角「添加站点」部署第一个站点。');
}

function button(text, action, mutation = false, className = 'text-button') {
  const node = el('button', text, className);
  node.type = 'button';
  if (mutation) { node.dataset.mutation = ''; node.disabled = readOnly; }
  node.addEventListener('click', guard(action));
  return node;
}
const action = (text, fn, danger = false) => button(text, fn, true, danger ? 'danger-text' : 'text-button');

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
      : '将删除站点配置文件（仅限 LightPanel 创建的配置）并重载 Web 服务器；站点目录中的文件不会被删除，已签发的证书会保留。',
    target: `${site.server_names.join(', ')} · ${site.detail}`,
    confirm: '确认删除',
  });
  if (!confirmed) return;
  await act(site, 'delete');
}

// ---- 站点设置弹窗 ----

function switchPane(name) {
  document.querySelectorAll('#site-dialog .settings-tabs button').forEach(node =>
    node.classList.toggle('active', node.dataset.pane === name));
  document.querySelectorAll('#site-dialog .settings-pane').forEach(node => {
    node.hidden = node.id !== name;
  });
}

function openSiteDialog(site) {
  selected = site;
  $('#site-dialog-title').textContent = `站点设置 — ${site.server_names.join(', ')}`;
  const info = $('#site-info');
  const domain = realDomain(site);
  const rows = [
    ['站点名称', site.server_names.join(', ')],
    ['类型', `${engineNames[site.engine] || site.engine} · ${kindNames[site.kind] || site.kind}`],
    ['监听端口', site.ports.join(', ') || '—'],
  ];
  if (site.root) rows.push(['站点目录', site.root]);
  if (site.proxy_pass) rows.push(['反代目标', site.proxy_pass]);
  if (site.engine !== 'docker') rows.push(['配置文件', site.detail, site.id]);
  rows.push(['来源', site.managed ? 'LightPanel 创建（支持一键 SSL）' : '外部配置（不支持面板改写）']);
  info.replaceChildren(...rows.flatMap(([key, value]) => [el('dt', key), el('dd', value)]));
  const dirButton = $('#site-open-dir');
  dirButton.hidden = !(site.root && site.root !== '/' && site.kind !== 'proxy');
  const reloadButton = $('#site-reload');
  reloadButton.hidden = site.engine === 'docker';
  const deleteButton = $('#site-delete');
  deleteButton.hidden = !site.managed;
  renderSSLPane();
  renderDomainsPane();
  switchPane('site-info-pane');
  $('#site-dialog').showModal();
}

function renderDomainsPane() {
  const site = selected;
  const list = $('#site-domains');
  if (!site) { list.replaceChildren(); return; }
  const rows = site.server_names.map(name => [name === '_' ? '默认站点（_）' : name,
    name === '_' ? '未绑定具体域名' : name.startsWith('*.') ? '通配符域名（不支持 SSL 签发）' : '主域名可用']);
  list.replaceChildren(...rows.flatMap(([key, value]) => [el('dt', key), el('dd', value)]));
}

function renderSSLPane() {
  const site = selected;
  const host = $('#site-ssl-status');
  const form = $('#ssl-form');
  const offButton = $('#ssl-off');
  if (!site || site.engine === 'docker') {
    host.replaceChildren(el('p', 'Docker 容器站点暂不支持面板签发证书；可为容器前的 Nginx/Apache 反代站点申请证书。', 'notice'));
    form.hidden = true;
    offButton.hidden = true;
    return;
  }
  form.hidden = false;
  const domain = realDomain(site);
  if (!domain) {
    host.replaceChildren(el('p', '该站点没有绑定具体域名（server_name 为 _ 或通配符），无法申请证书。请先在站点配置中绑定域名。', 'notice'));
    form.hidden = true;
    offButton.hidden = true;
    return;
  }
  if (!site.managed) {
    host.replaceChildren(el('p', '该站点配置不是由面板创建的，不能一键配置 SSL；只有面板创建的站点才支持。', 'notice'));
    form.hidden = true;
    offButton.hidden = true;
    return;
  }
  const cert = certForDomain(domain);
  offButton.hidden = !site.ssl && !cert;
  if (cert) {
    const card = el('article', undefined, 'panel');
    card.style.marginBottom = '14px';
    const head = el('div', undefined, 'panel-heading');
    const title = el('div');
    title.append(el('h2', `当前证书 — ${cert.domain}`));
    title.append(el('p', cert.staging ? 'Let\'s Encrypt Staging 测试证书（浏览器不信任，不能用于生产）' : cert.issuer));
    head.append(title, badge(cert.days_left <= 15 ? `剩余 ${cert.days_left} 天` : '有效', cert.days_left <= 15 ? 'warn' : 'good'));
    const dl = el('dl', undefined, 'info-list');
    [['域名', cert.domain], ['颁发者', cert.issuer], ['签发时间', fmtDate(cert.not_before)], ['到期时间', `${fmtDate(cert.not_after)}（剩余 ${cert.days_left} 天）`],
      ['强制 HTTPS', cert.force_https ? '已开启' : '未开启'], ['自动续期', cert.staging ? '否（测试证书不续期）' : '到期前 30 天自动续期']]
      .forEach(([key, value]) => { dl.append(el('dt', key), el('dd', value)); });
    card.append(head, dl);
    host.replaceChildren(card);
  } else if (site.ssl) {
    host.replaceChildren(el('p', '站点配置已启用 HTTPS，但面板没有该域名的证书记录（可能由外部工具签发）。', 'notice'));
  } else {
    host.replaceChildren(el('p', '该站点还没有证书。填写下方信息一键申请 Let\'s Encrypt 免费证书。', 'notice'));
  }
}

async function issueCert() {
  const site = selected;
  if (!site) return;
  const domain = realDomain(site);
  const form = $('#ssl-form');
  const data = Object.fromEntries(new FormData(form));
  const email = (data.email || '').trim();
  if (readOnly) throw new Error('当前为只读模式，不能修改服务器。');
  const confirmed = await confirmAction({
    title: `为 ${domain} 申请 SSL 证书？`,
    description: '面板将通过 HTTP-01 验证域名（域名必须解析到本服务器且 80 端口可从公网访问），自动改写站点配置启用 HTTPS。签发过程在后台执行，可打开任务中心查看进度。',
    target: `${engineNames[site.engine] || site.engine} · ${site.detail}`,
    confirm: '申请证书',
    danger: false,
  });
  if (!confirmed) return;
  const result = await mutate('/api/sites/ssl', {
    op: 'issue', id: site.id, email,
    force_https: data.force_https ? 'true' : '',
    staging: data.staging ? 'true' : '',
  });
  if (result?.task_id) openTaskCenter(result.task_id);
}

async function disableSSL() {
  const site = selected;
  if (!site) return;
  const confirmed = await confirmAction({
    title: '关闭该站点的 SSL？',
    description: '将把站点配置恢复为纯 HTTP 并重载 Web 服务器；已签发的证书文件会保留，之后可随时重新开启。',
    target: `${site.server_names.join(', ')} · ${site.detail}`,
    confirm: '关闭 SSL',
  });
  if (!confirmed) return;
  await mutate('/api/sites/ssl', { op: 'off', id: site.id });
  message('站点已关闭 SSL 并重载 Web 服务器。');
  await sites();
  if ($('#site-dialog').open) { renderSSLPane(); }
}

async function loadCerts() {
  const request = ++certVersion;
  const data = await api('/api/sites/certs');
  if (request !== certVersion) return;
  certs = data.items || [];
}

export async function sites() {
  const request = ++version;
  const [siteData, certData] = await Promise.all([api('/api/sites'), api('/api/sites/certs').catch(() => ({ items: [] }))]);
  if (request !== version) return;
  environment = siteData.environment || [];
  items = siteData.items || [];
  certs = certData.items || [];
  renderEnvironment();
  syncEngineOptions();
  renderSites();
  if ($('#site-dialog').open && selected) {
    selected = items.find(item => item.id === selected.id) || selected;
    renderSSLPane();
    renderDomainsPane();
  }
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
  const engine = $('#site-form [name=engine]').value;
  const docker = engine === 'docker';
  const proxy = !docker && $('#site-form [name=mode]').value === 'proxy';
  $('#site-native-fields').hidden = docker;
  $('#site-docker-fields').hidden = !docker;
  $('#site-root-label').hidden = docker || proxy;
  $('#site-proxy-label').hidden = docker || !proxy;
  $('#site-help').textContent = docker
    ? '将以站点名称启动容器（lightpanel-名称），映射监听端口到容器端口，并配置自动重启。镜像会按需自动拉取。'
    : proxy
      ? '反向代理站点把该域名收到的请求转发到反代目标（本机端口或其他上游），需要 Nginx/Apache 已安装；Apache 需要 mod_proxy 模块。'
      : '部署方式选择「自动识别」时，面板会检测已安装的 Nginx 或 Apache 并创建站点配置；反向代理会把请求转发到反代目标。';
}

export function setupSites(navigate) {
  go = navigate;
  $('#site-detect').addEventListener('click', guard(sites));
  $('#site-search').addEventListener('input', renderSites);
  $('#site-create-open').addEventListener('click', () => $('#site-create-dialog').showModal());
  $('#site-form [name=engine]').addEventListener('change', syncForm);
  $('#site-form [name=mode]').addEventListener('change', syncForm);
  $('#site-dialog').querySelectorAll('[data-close]').forEach(node =>
    node.addEventListener('click', () => $('#site-dialog').close()));
  $('#site-create-dialog').querySelectorAll('[data-close]').forEach(node =>
    node.addEventListener('click', () => $('#site-create-dialog').close()));
  document.querySelectorAll('#site-dialog .settings-tabs button').forEach(node =>
    node.addEventListener('click', () => switchPane(node.dataset.pane)));
  $('#site-open-dir').addEventListener('click', () => {
    if (!selected?.root) return;
    $('#site-dialog').close();
    go('files');
    files(selected.root, 0);
  });
  $('#site-reload').addEventListener('click', async () => {
    await act(selected, 'reload');
    message('Web 服务配置已重载。');
  });
  $('#site-delete').addEventListener('click', () => removeSite(selected));
  $('#ssl-form').addEventListener('submit', guard(issueCert));
  $('#ssl-off').addEventListener('click', guard(disableSSL));
  window.addEventListener('task-finished', event => {
    const kind = event.detail?.kind;
    if (kind !== 'issue-cert') return;
    message(event.detail.state === 'done'
      ? `${event.detail.title}完成。`
      : `${event.detail.title}失败，详情见任务中心。`, event.detail.state !== 'done');
    sites().catch(() => {});
  });
  window.addEventListener('task-finished', event => {
    if (event.detail?.kind !== 'site-create') return;
    message(event.detail.state === 'done'
      ? `${event.detail.title}完成。`
      : `${event.detail.title}失败，详情见任务中心。`, event.detail.state !== 'done');
    sites().catch(() => {});
  });
  $('#site-form').addEventListener('submit', guard(async () => {
    const data = Object.fromEntries(new FormData($('#site-form')));
    if (data.engine === 'docker' && !data.image?.trim()) throw new Error('Docker 部署需要填写镜像名称。');
    if (data.engine !== 'docker' && data.mode === 'proxy' && !data.proxy_target?.trim()) throw new Error('反向代理站点需要填写反代目标。');
    if (readOnly) throw new Error('当前为只读模式，不能修改服务器。');
    const result = await mutate('/api/sites/create', data);
    $('#site-create-dialog').close();
    $('#site-form').reset();
    syncForm();
    if (result?.task_id) openTaskCenter(result.task_id);
    await sites();
  }));
  syncForm();
}
