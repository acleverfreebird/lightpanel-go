import { $, api, el, badge } from './ui.js';

let checkedAt = 0;
export async function diagnostics() {
  if (Date.now() - checkedAt < 30000) return;
  const status = $('#diagnostics-status');
  try {
    const data = await api('/api/health');
    const names = { root: 'Root 直接管理', helper: '最小特权 Helper', unprivileged: '普通用户', 'read-only': '只读模式' };
    const rows = $('#diagnostics-details');
    rows.replaceChildren();
    function row(name, value) { rows.append(el('dt', name), el('dd', value)); }
    row('运行身份', `${names[data.mode] || data.mode} · UID ${data.uid}`);
    row('systemd', data.systemd ? '可用' : '未检测到运行环境，服务管理可能不可用');
    row('系统工具', Object.entries(data.tools).map(([name, available]) => `${name}: ${available ? '已安装' : '未找到'}`).join(' · '));
    row('Helper 连接', !data.helper.configured ? '未配置' : data.helper.reachable ? '连接成功（不代表操作授权）' : '无法连接，请检查 Helper 服务与 socket 权限');
    if (data.helper.configured) {
      row('Helper 配置授权', `服务规则 ${data.helper.service_rules} 条${data.helper.wildcard_services ? '（含通配授权）' : ''}；防火墙 ${data.helper.allow_firewall ? '允许' : '禁止'}；进程信号 ${data.helper.allow_kill ? '允许' : '禁止'}；更新 ${data.helper.allow_update ? '允许' : '禁止'}`);
    }
    $('#diagnostics-notes').replaceChildren(...data.notes.map(note => el('p', note, 'section-note')));
    status.replaceChildren(badge(data.warnings.length ? '需要留意' : '检测完成', data.warnings.length ? 'warn' : 'good'));
    $('#diagnostics-warnings').replaceChildren(...data.warnings.map(note => el('p', note, 'notice')));
    checkedAt = Date.now();
  } catch (error) {
    status.textContent = '检测失败';
    $('#diagnostics-warnings').replaceChildren(el('p', error.message, 'notice'));
    throw error;
  }
}
