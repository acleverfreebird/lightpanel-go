export function size(value) {
  let n = Number(value);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1024) return `${Math.round(n)} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return `${n.toFixed(1)} ${units[i]}`;
}
export function percent(used, total) {
  return total > 0 ? Math.min(100, Math.max(0, used / total * 100)) : null;
}
export function duration(seconds) {
  const n = Math.max(0, Math.floor(seconds));
  return `${Math.floor(n / 86400)} 天 ${Math.floor(n % 86400 / 3600)} 小时 ${Math.floor(n % 3600 / 60)} 分钟`;
}
export function parseServices(output) {
  return output.split('\n').flatMap(line => {
    const match = line.trim().match(/^(?:[●○]\s+)?(\S+\.service)\s+(\S+)\s+(\S+)\s+(\S+)\s*(.*)$/);
    return match ? [{ name: match[1], load: match[2], state: match[3], sub: match[4], description: match[5] }] : [];
  });
}
export function time(value) {
  const n = Number(value);
  if (!Number.isFinite(n) || n <= 0) return '—';
  return new Date(n * 1000).toLocaleString('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false });
}
export function normalizePath(value) {
  const body = String(value).trim().replace(/^\/+/, '').replace(/\/+$/, '');
  return '/' + body;
}
export function resolveInputPath(value, directory) {
  const input = String(value).trim();
  if (!input || /[\\\0]/.test(input) || input.split('/').includes('..')) {
    throw new Error('请输入有效路径，不能包含反斜杠或 ..');
  }
  return normalizePath(input.startsWith('/') ? input : `${directory}/${input}`).replace(/\/{2,}/g, '/');
}
export function unitName(value) {
  const name = value.trim();
  return name && !name.endsWith('.service') ? `${name}.service` : name;
}
