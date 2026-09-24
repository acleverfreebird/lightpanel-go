import { $, api, el, message } from './ui.js';
import { size, percent, duration } from './format.js';

const samples = [];
let version = 0;
const percentage = value => value === null ? '不可用' : `${value.toFixed(1)}%`;
function metric(label, value, detail, usage, icon) {
  const card = el('article', undefined, 'metric-card');
  const head = el('div', undefined, 'metric-head');
  const symbol = el('span', icon, 'metric-icon');
  symbol.setAttribute('aria-hidden', 'true');
  head.append(el('h2', label), symbol);
  card.append(head, el('strong', value, 'metric-value'), el('small', detail));
  if (usage !== null) {
    const progress = el('progress');
    progress.max = 100; progress.value = usage;
    progress.setAttribute('aria-label', `${label} ${percentage(usage)}`);
    card.append(progress);
  } else card.append(el('span', label === '网络接收' ? '↓ 接收 / ↑ 发送 · 所有非回环接口' : '等待服务器提供有效采样', 'metric-foot'));
  return card;
}
export async function overview() {
  const request = ++version;
  const m = await api('/api/metrics');
  if (request !== version) return;
  const cpu = m.sample_ready ? m.cpu_percent : null;
  const memory = percent(m.memory_used, m.memory_total), disk = percent(m.disk_used, m.disk_total);
  $('#host').textContent = $('#sidebar-host').textContent = m.hostname || '当前服务器';
  $('#sidebar-os').textContent = m.os || 'Linux';
  $('#host-subtitle').textContent = `${m.os} · 已运行 ${duration(m.uptime)}`;
  $('#metrics').replaceChildren(
    metric('CPU 使用率', cpu === null ? '采样中' : percentage(cpu), '每 3 秒更新 · 采样间隔平均值', cpu, '⌁'),
    metric('内存使用', percentage(memory), `${size(m.memory_used)} / ${size(m.memory_total)}`, memory, '▥'),
    metric('根分区磁盘', percentage(disk), `${size(m.disk_used)} / ${size(m.disk_total)}`, disk, '▤'),
    metric('网络接收', m.sample_ready ? `${size(m.rx_bytes_per_sec)}/s` : '采样中', `↑ 发送 ${m.sample_ready ? size(m.tx_bytes_per_sec) + '/s' : '采样中'}`, null, '⇅'),
  );
  const errors = m.errors || [];
  const high = [cpu, memory, disk].some(value => value !== null && value >= 85);
  $('#resource-status').textContent = errors.length ? '部分指标不可用' : high ? '资源占用较高' : cpu === null ? '等待采样' : '资源使用正常';
  $('#resource-status').className = `badge ${errors.length || high ? 'warn' : cpu === null ? '' : 'good'}`;
  const info = [['操作系统', m.os], ['运行时间', duration(m.uptime)], ['平均负载 · 1 / 5 / 15 分钟', m.load.map(n => n.toFixed(2)).join(' / ')], ['面板内存', size(m.self_rss)], ['访问权限', document.body.dataset.readonly === 'true' ? '只读访问' : '管理员']];
  $('#host-info').replaceChildren(...info.flatMap(([key, value]) => [el('dt', key), el('dd', value)]));
  if (errors.length) message(`部分指标读取失败：\n${errors.join('\n')}`, true);
  samples.push({ cpu, memory, time: Date.now() });
  if (samples.length > 60) samples.shift();
  drawChart();
}
function drawChart() {
  const canvas = $('#resource-chart'), width = canvas.clientWidth, height = canvas.clientHeight;
  if (!width) return;
  const ratio = window.devicePixelRatio || 1;
  canvas.width = width * ratio; canvas.height = height * ratio;
  const ctx = canvas.getContext('2d');
  ctx.scale(ratio, ratio);
  const left = 38, top = 12, bottom = height - 12;
  ctx.font = '11px sans-serif'; ctx.lineWidth = 1;
  [0, 25, 50, 75, 100].forEach(value => {
    const y = bottom - value / 100 * (bottom - top);
    ctx.fillStyle = '#667589'; ctx.fillText(`${value}%`, 0, y + 4);
    ctx.strokeStyle = '#edf1f5'; ctx.beginPath(); ctx.moveTo(left, y); ctx.lineTo(width, y); ctx.stroke();
  });
  const start = samples[0]?.time, end = samples.at(-1)?.time;
  for (const [key, color] of [['cpu', '#16856b'], ['memory', '#7485c1']]) {
    ctx.strokeStyle = color; ctx.lineWidth = 2; ctx.beginPath();
    let connected = false;
    samples.forEach(sample => {
      if (sample[key] === null) { connected = false; return; }
      const x = left + (sample.time - start) / Math.max(1, end - start) * (width - left - 4);
      const y = bottom - sample[key] / 100 * (bottom - top);
      if (connected) ctx.lineTo(x, y); else ctx.moveTo(x, y);
      connected = true;
    });
    ctx.stroke();
  }
  $('#chart-empty').hidden = samples.length >= 2;
  $('#chart-range').textContent = samples.length ? `${new Date(start).toLocaleTimeString()} · ${samples.length} 次采样` : '等待采样';
  const latest = samples.at(-1);
  if (latest) canvas.setAttribute('aria-label', `最近 ${samples.length} 次采样；当前 CPU ${percentage(latest.cpu)}，内存 ${percentage(latest.memory)}`);
}
new ResizeObserver(drawChart).observe($('#resource-chart'));
