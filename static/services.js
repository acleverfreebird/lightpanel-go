import { $, api, mutate, guard, el, button, badge, table, confirmAction, readOnly } from './ui.js';
import { size, parseServices } from './format.js';

let processPage = 1, processQuery = '', processVersion = 0;
export async function processes() {
  const version = ++processVersion;
  const data = await api('/api/processes?' + new URLSearchParams({ q: processQuery, page: processPage }));
  if (version !== processVersion) return;
  table('#process-list', ['PID', '进程名', 'UID', '状态', '驻留内存', '操作'], data.items.map(p => {
    const actions = el('div', undefined, 'actions');
    [15, 9].forEach(signal => actions.append(button(signal === 15 ? '结束' : '强制结束', async () => {
      if (!await confirmAction({ title: signal === 15 ? '结束进程？' : '强制结束进程？', description: signal === 15 ? '向进程发送 TERM，允许它保存状态并正常退出。' : '立即发送 KILL，未保存的数据可能丢失。请优先尝试正常结束。', target: `${p.name} · PID ${p.pid}`, confirm: signal === 15 ? '结束进程' : '强制结束' })) return;
      await mutate('/api/process/kill', { pid: p.pid, signal, start_time: p.start_time });
      await processes();
    }, true, signal === 9 ? 'danger-text' : 'text-button')));
    const states = { R: '运行', S: '休眠', D: '等待 I/O', Z: '僵尸', T: '已停止', I: '空闲' };
    return [p.pid, p.name, p.uid, badge(states[p.state] || p.state, p.state === 'Z' ? 'warn' : ''), size(p.rss_bytes), actions];
  }), '没有匹配的进程');
  $('#process-count').textContent = data.total;
  $('#process-page').textContent = `第 ${processPage} 页 · 共 ${data.total} 个进程`;
  $('#process-prev').disabled = processPage <= 1;
  $('#process-next').disabled = processPage * 100 >= data.total;
}
export function setupProcesses() {
  $('#process-search').addEventListener('submit', guard(async () => { processQuery = new FormData($('#process-search')).get('q').trim(); processPage = 1; await processes(); }));
  $('#process-reset').addEventListener('click', guard(async () => { $('#process-search').reset(); processQuery = ''; processPage = 1; await processes(); }));
  $('#process-prev').addEventListener('click', guard(async () => { processPage = Math.max(1, processPage - 1); await processes(); }));
  $('#process-next').addEventListener('click', guard(async () => { processPage++; await processes(); }));
}

let serviceItems = [], servicePage = 1, serviceVersion = 0, detailVersion = 0, navigate;
const actionNames = { start: '启动', stop: '停止', restart: '重启', reload: '重载配置', enable: '启用开机启动', disable: '禁用开机启动' };
async function serviceDetail(name) {
  const version = ++detailVersion;
  const data = await api('/api/services?' + new URLSearchParams({ name }));
  if (version !== detailVersion) return;
  $('#service-detail-title').textContent = name;
  $('#service-output').textContent = data.output || '暂无状态信息';
  $('#service-detail').hidden = false;
  $('#service-detail').scrollIntoView({ behavior: 'auto', block: 'nearest' });
}
async function serviceAction(name, action) {
  const description = ['enable', 'disable'].includes(action) ? '仅修改开机启动配置，不会立即启动或停止当前服务。' : action === 'reload' ? '要求服务重新加载配置；服务必须支持重载，否则操作会返回错误。' : '服务状态将发生变化，依赖它的连接或任务可能受到影响。';
  if (!await confirmAction({ title: `${actionNames[action]}？`, description, target: name, confirm: actionNames[action], danger: ['stop', 'restart', 'disable'].includes(action) })) return;
  await mutate('/api/service/action', { name, action });
  await services();
  await serviceDetail(name);
}
function renderServices() {
  const query = $('#service-search').value.toLowerCase().trim(), filter = $('#service-filter').value;
  const items = serviceItems.filter(s => `${s.name} ${s.description}`.toLowerCase().includes(query) && (filter === 'all' || s.state === filter));
  const pages = Math.max(1, Math.ceil(items.length / 25));
  servicePage = Math.min(servicePage, pages);
  table('#service-list', ['服务名称', '状态', '开机启动', '描述', '操作'], items.slice((servicePage - 1) * 25, servicePage * 25).map(s => {
    const actions = el('div', undefined, 'actions');
    actions.append(button('详情', () => serviceDetail(s.name)), button('日志', () => { $('#logs-form [name=name]').value = s.name; navigate('logs'); }));
    if (s.state === 'active') actions.append(button('重启', () => serviceAction(s.name, 'restart'), true), button('重载', () => serviceAction(s.name, 'reload'), true), button('停止', () => serviceAction(s.name, 'stop'), true, 'danger-text'));
    else actions.append(button('启动', () => serviceAction(s.name, 'start'), true));
    if (s.unit_file_state === 'enabled' || s.unit_file_state === 'enabled-runtime') actions.append(button('禁用自启', () => serviceAction(s.name, 'disable'), true));
    else if (s.unit_file_state === 'disabled') actions.append(button('启用自启', () => serviceAction(s.name, 'enable'), true));
    const state = { active: '运行中', inactive: '未运行', failed: '失败', activating: '启动中', deactivating: '停止中' };
    const boot = { enabled: '已启用', disabled: '已禁用', 'enabled-runtime': '临时启用', static: '静态依赖', indirect: '间接启用', masked: '已屏蔽', 'masked-runtime': '临时屏蔽', generated: '动态生成', transient: '临时服务', alias: '别名', linked: '外部链接', 'linked-runtime': '临时链接', unknown: '未知' };
    return [s.name, badge(state[s.state] || s.state, s.state === 'active' ? 'good' : s.state === 'failed' ? 'bad' : ''), badge(boot[s.unit_file_state] || s.unit_file_state || '未知', s.unit_file_state === 'enabled' ? 'good' : ''), s.description || '—', actions];
  }), '没有匹配的服务', '调整关键词或状态筛选，也可以在下方输入完整服务名。');
  $('#service-count').textContent = serviceItems.length;
  $('#service-page').textContent = `${items.length} 个匹配 · 第 ${servicePage} / ${pages} 页`;
  $('#service-prev').disabled = servicePage <= 1; $('#service-next').disabled = servicePage >= pages;
}
export async function services() {
  const version = ++serviceVersion;
  const data = await api('/api/services');
  if (version !== serviceVersion) return;
  serviceItems = Array.isArray(data.items) ? data.items : parseServices(data.output); renderServices();
}
export function setupServices(go) {
  navigate = go;
  $('#service-search').addEventListener('input', () => { servicePage = 1; renderServices(); });
  $('#service-filter').addEventListener('change', () => { servicePage = 1; renderServices(); });
  $('#service-prev').addEventListener('click', () => { servicePage--; renderServices(); });
  $('#service-next').addEventListener('click', () => { servicePage++; renderServices(); });
  $('#service-detail-close').addEventListener('click', () => { detailVersion++; $('#service-detail').hidden = true; });
  $('#service-form').addEventListener('submit', guard(async () => {
    const { name, action } = Object.fromEntries(new FormData($('#service-form')));
    if (action === 'status') await serviceDetail(name); else await serviceAction(name, action);
  }));
  if (readOnly) $('#service-form [name=action]').querySelectorAll('option').forEach(option => option.disabled = option.value !== 'status');
}
