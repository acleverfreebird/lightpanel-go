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
const actionNames = { start: '启动', stop: '停止', restart: '重启' };
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
  if (!await confirmAction({ title: `${actionNames[action]}服务？`, description: '服务状态将发生变化，依赖它的连接或任务可能受到影响。', target: name, confirm: `${actionNames[action]}服务`, danger: action !== 'start' })) return;
  await mutate('/api/service/action', { name, action });
  await services();
  await serviceDetail(name);
}
function renderServices() {
  const query = $('#service-search').value.toLowerCase().trim(), filter = $('#service-filter').value;
  const items = serviceItems.filter(s => `${s.name} ${s.description}`.toLowerCase().includes(query) && (filter === 'all' || s.state === filter));
  const pages = Math.max(1, Math.ceil(items.length / 25));
  servicePage = Math.min(servicePage, pages);
  table('#service-list', ['服务名称', '状态', '描述', '操作'], items.slice((servicePage - 1) * 25, servicePage * 25).map(s => {
    const actions = el('div', undefined, 'actions');
    actions.append(button('详情', () => serviceDetail(s.name)), button('日志', () => { $('#logs-form [name=name]').value = s.name; navigate('logs'); }));
    if (s.state === 'active') actions.append(button('重启', () => serviceAction(s.name, 'restart'), true), button('停止', () => serviceAction(s.name, 'stop'), true, 'danger-text'));
    else actions.append(button('启动', () => serviceAction(s.name, 'start'), true));
    const state = { active: '运行中', inactive: '未运行', failed: '失败', activating: '启动中', deactivating: '停止中' };
    return [s.name, badge(state[s.state] || s.state, s.state === 'active' ? 'good' : s.state === 'failed' ? 'bad' : ''), s.description || '—', actions];
  }), '没有匹配的服务', '调整关键词或状态筛选，也可以在下方输入完整服务名。');
  $('#service-count').textContent = serviceItems.length;
  $('#service-page').textContent = `${items.length} 个匹配 · 第 ${servicePage} / ${pages} 页`;
  $('#service-prev').disabled = servicePage <= 1; $('#service-next').disabled = servicePage >= pages;
}
export async function services() {
  const version = ++serviceVersion;
  const data = await api('/api/services');
  if (version !== serviceVersion) return;
  serviceItems = parseServices(data.output); renderServices();
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
