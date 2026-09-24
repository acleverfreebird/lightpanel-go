import { $, api, mutate, guard, confirmAction, readOnly } from './ui.js';
import { unitName } from './format.js';

let logContent = '', logVersion = 0, firewallVersion = 0, detectedEngine = '';
function renderLogs() {
  const query = $('#log-search').value.toLowerCase();
  const lines = logContent ? logContent.trimEnd().split('\n') : [];
  const matches = lines.filter(line => line.toLowerCase().includes(query));
  $('#logs-output').textContent = matches.join('\n') || (lines.length ? '没有匹配的日志，请调整关键词。' : '暂无日志。');
  $('#log-count').textContent = `${matches.length} / ${lines.length} 行`;
}
export async function logs() {
  const version = ++logVersion;
  logContent = '';
  $('#logs-output').textContent = '正在读取日志…';
  $('#log-count').textContent = '读取中';
  const form = new FormData($('#logs-form'));
  form.set('name', unitName(form.get('name')));
  let data;
  try {
    data = await api('/api/logs?' + new URLSearchParams(form));
  } catch (error) {
    if (version === logVersion) {
      $('#logs-output').textContent = '日志读取失败，请检查服务名称后重试。';
      $('#log-count').textContent = '未加载';
    }
    throw error;
  }
  if (version !== logVersion) return;
  logContent = data.output || ''; renderLogs();
}
function firewallOptions() {
  const engine = $('#firewall-engine').value || detectedEngine;
  const action = $('#firewall-form [name=action]');
  [...action.options].forEach(option => option.disabled = engine !== 'ufw' && ['deny', 'remove-deny'].includes(option.value));
  if (action.selectedOptions[0].disabled) action.value = 'allow';
  $('#firewall-help').textContent = engine === 'firewalld' ? '规则仅修改默认区域的运行时配置，重载或重启后丢弃。移除放行不等于显式拒绝。' : engine === 'ufw' ? 'UFW 规则会持久保存，但不会自动启用防火墙。修改管理端口可能中断远程连接。' : '先读取防火墙状态。确认实际引擎后才可应用规则。';
  $('#firewall-form button').disabled = readOnly || !detectedEngine;
}
export async function firewall() {
  const version = ++firewallVersion;
  detectedEngine = ''; firewallOptions();
  $('#firewall-output').textContent = '正在读取防火墙状态…';
  try {
    const data = await api('/api/firewall?' + new URLSearchParams({ engine: $('#firewall-engine').value }));
    if (version !== firewallVersion) return;
    detectedEngine = data.engine;
    $('#firewall-output').textContent = `${data.engine}\n\n${data.output}`;
  } catch (error) {
    if (version === firewallVersion) $('#firewall-output').textContent = '未能读取状态，请检查引擎是否已安装、正在运行且面板具有访问权限。';
    throw error;
  } finally { if (version === firewallVersion) firewallOptions(); }
}
export function setupTools() {
  $('#logs-form').addEventListener('submit', guard(logs));
  $('#log-search').addEventListener('input', renderLogs);
  $('#log-wrap').addEventListener('change', () => $('#logs-output').classList.toggle('no-wrap', !$('#log-wrap').checked));
  $('#log-bottom').addEventListener('click', () => { const output = $('#logs-output'); output.scrollTop = output.scrollHeight; });
  $('#firewall-engine').addEventListener('change', guard(firewall));
  $('#firewall-form').addEventListener('submit', guard(async () => {
    const data = Object.fromEntries(new FormData($('#firewall-form')));
    data.engine = detectedEngine;
    if (!data.engine) throw new Error('请先成功读取防火墙状态。');
    const label = $('#firewall-form [name=action]').selectedOptions[0].textContent;
    if (!await confirmAction({ title: '应用端口规则？', description: '请确认此规则不会阻断 SSH 或面板的管理连接。', target: `${data.engine} · ${data.port}/${data.protocol} · ${label}`, confirm: '应用规则' })) return;
    await mutate('/api/firewall/rule', data); await firewall();
  }));
  firewallOptions();
}
