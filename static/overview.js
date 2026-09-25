import { $, api, el, message } from './ui.js';
import { size, percent, duration } from './format.js';
import { icon } from './icons.js';

const COLORS = { cpu: '#c05a30', memory: '#6d78b8', disk: '#a48132', net: '#347f81' };
const samples = [];
let version = 0;
let cards = null;

const percentage = value => value === null ? '不可用' : `${value.toFixed(1)}%`;

function meter(color) {
  const track = el('div', undefined, 'metric-meter');
  track.setAttribute('role', 'img');
  const bar = el('span');
  bar.style.background = color;
  bar.style.transform = 'scaleX(0)';
  track.append(bar);
  return { track, bar };
}

function buildCard({ label, symbolName, color, gauge }) {
  const card = el('article', undefined, 'metric-card');
  const head = el('div', undefined, 'metric-head');
  const symbol = el('span', undefined, 'metric-icon');
  symbol.append(icon(symbolName));
  symbol.setAttribute('aria-hidden', 'true');
  head.append(el('h2', label), symbol);
  card.append(head);
  const parts = { card, label };
  if (gauge) {
    const body = el('div', undefined, 'metric-body');
    const { track, bar } = meter(color);
    parts.track = track;
    parts.bar = bar;
    parts.value = el('strong', '—', 'metric-value');
    parts.detail = el('small', '');
    const text = el('div', undefined, 'metric-text');
    text.append(parts.value, parts.detail);
    body.append(text, track);
    card.append(body);
  } else {
    parts.value = el('strong', '—', 'metric-value');
    parts.detail = el('small', '');
    parts.foot = el('span', '', 'metric-foot');
    card.append(parts.value, parts.detail, parts.foot);
  }
  return parts;
}

function ensureCards() {
  if (cards) return;
  cards = [
    buildCard({ label: 'CPU 使用率', symbolName: 'cpu', color: COLORS.cpu, gauge: true }),
    buildCard({ label: '内存使用', symbolName: 'memory', color: COLORS.memory, gauge: true }),
    buildCard({ label: '根分区磁盘', symbolName: 'disk', color: COLORS.disk, gauge: true }),
    buildCard({ label: '网络接收', symbolName: 'network', color: COLORS.net, gauge: false }),
  ];
  $('#metrics').replaceChildren(...cards.map(part => part.card));
}

function setGauge(part, usage) {
  if (!part.bar) return;
  const ratio = usage === null ? 0 : Math.min(usage, 100) / 100;
  part.bar.classList.toggle('idle', usage === null);
  part.bar.classList.toggle('high', usage !== null && usage >= 85);
  part.bar.style.transform = `scaleX(${ratio})`;
  part.track.setAttribute('aria-label', `${part.label} ${percentage(usage)}`);
}

export async function overview() {
  const request = ++version;
  const m = await api('/api/metrics');
  if (request !== version) return;
  ensureCards();
  const [cpuCard, memoryCard, diskCard, networkCard] = cards;
  const cpu = m.sample_ready ? m.cpu_percent : null;
  const memory = percent(m.memory_used, m.memory_total), disk = percent(m.disk_used, m.disk_total);
  $('#host').textContent = $('#sidebar-host').textContent = m.hostname || '当前服务器';
  $('#sidebar-os').textContent = m.os || 'Linux';
  $('#host-subtitle').textContent = `${m.os} · 已运行 ${duration(m.uptime)}`;

  cpuCard.value.textContent = cpu === null ? '采样中' : percentage(cpu);
  cpuCard.detail.textContent = '每 3 秒更新 · 采样间隔平均值';
  setGauge(cpuCard, cpu);

  memoryCard.value.textContent = percentage(memory);
  memoryCard.detail.textContent = `${size(m.memory_used)} / ${size(m.memory_total)}`;
  setGauge(memoryCard, memory);

  diskCard.value.textContent = percentage(disk);
  diskCard.detail.textContent = `${size(m.disk_used)} / ${size(m.disk_total)}`;
  setGauge(diskCard, disk);

  networkCard.value.textContent = m.sample_ready ? `${size(m.rx_bytes_per_sec)}/s` : '采样中';
  networkCard.detail.textContent = m.sample_ready ? `↑ 发送 ${size(m.tx_bytes_per_sec)}/s` : '等待有效采样';
  networkCard.foot.textContent = '↓ 接收 / ↑ 发送 · 所有非回环接口';

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
  const pw = Math.round(width * ratio), ph = Math.round(height * ratio);
  if (canvas.width !== pw || canvas.height !== ph) { canvas.width = pw; canvas.height = ph; }
  const ctx = canvas.getContext('2d');
  ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
  ctx.clearRect(0, 0, width, height);
  const left = 38, top = 12, bottom = height - 12, span = width - left - 6;
  ctx.font = '11px sans-serif';
  [0, 25, 50, 75, 100].forEach(value => {
    const y = bottom - value / 100 * (bottom - top);
    ctx.fillStyle = '#848b7b';
    ctx.fillText(`${value}%`, 0, y + 4);
    ctx.strokeStyle = value === 0 ? '#dce1d3' : '#eff1e9';
    ctx.lineWidth = 1;
    ctx.beginPath(); ctx.moveTo(left, y); ctx.lineTo(width, y); ctx.stroke();
  });
  const start = samples[0]?.time, end = samples.at(-1)?.time;
  if (start !== undefined) {
    for (const [key, color, fill] of [['cpu', COLORS.cpu, true], ['memory', COLORS.memory, false]]) {
      const segments = [];
      let current = null;
      samples.forEach(sample => {
        if (sample[key] === null) { if (current?.length > 1) segments.push(current); current = null; return; }
        const x = left + (sample.time - start) / Math.max(1, end - start) * span;
        const y = bottom - sample[key] / 100 * (bottom - top);
        (current ??= []).push([x, y]);
      });
      if (current?.length > 1) segments.push(current);
      for (const points of segments) {
        if (fill) {
          ctx.beginPath();
          points.forEach(([x, y], i) => i ? ctx.lineTo(x, y) : ctx.moveTo(x, y));
          ctx.lineTo(points.at(-1)[0], bottom);
          ctx.lineTo(points[0][0], bottom);
          ctx.closePath();
          const grad = ctx.createLinearGradient(0, top, 0, bottom);
          grad.addColorStop(0, 'rgba(192,90,48,.18)');
          grad.addColorStop(1, 'rgba(192,90,48,0)');
          ctx.fillStyle = grad;
          ctx.fill();
        }
        ctx.beginPath();
        points.forEach(([x, y], i) => i ? ctx.lineTo(x, y) : ctx.moveTo(x, y));
        ctx.strokeStyle = color;
        ctx.lineWidth = 2;
        ctx.lineJoin = 'round';
        ctx.stroke();
        const [endX, endY] = points.at(-1);
        ctx.beginPath();
        ctx.arc(endX, endY, 3.2, 0, Math.PI * 2);
        ctx.fillStyle = color;
        ctx.fill();
        ctx.strokeStyle = '#fff';
        ctx.lineWidth = 1.5;
        ctx.stroke();
      }
    }
  }
  $('#chart-empty').hidden = samples.length >= 2;
  $('#chart-range').textContent = samples.length ? `${new Date(start).toLocaleTimeString()} · ${samples.length} 次采样` : '等待采样';
  const latest = samples.at(-1);
  if (latest) canvas.setAttribute('aria-label', `最近 ${samples.length} 次采样；当前 CPU ${percentage(latest.cpu)}，内存 ${percentage(latest.memory)}`);
}
new ResizeObserver(drawChart).observe($('#resource-chart'));
