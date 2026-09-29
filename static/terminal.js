import { $, api, guard, readOnly, confirmAction } from './ui.js';

export function setupTerminal() {
  const connect = $('#terminal-connect'), disconnect = $('#terminal-disconnect');
  const input = $('#terminal-input'), output = $('#terminal-output'), status = $('#terminal-status');
  const enabled = document.body.dataset.terminalEnabled === 'true';
  const ready = document.body.dataset.terminalReady === 'true';
  let socket = null, pending = false, generation = 0;
  const encoder = new TextEncoder();
  function controls() {
    const active = socket?.readyState === WebSocket.OPEN;
    connect.disabled = readOnly || !enabled || !ready || pending || !!socket;
    disconnect.disabled = !socket && !pending;
    input.disabled = !active;
    $('#terminal-send').disabled = !active;
    $('#terminal-interrupt').disabled = !active;
    $('#terminal-eof').disabled = !active;
  }
  function stop() {
    generation++;
    const previous = socket;
    socket = null; pending = false;
    previous?.close();
    input.value = '';
    status.textContent = '已断开；重新连接会创建新会话。';
    controls();
  }
  function send(text) {
    if (socket?.readyState !== WebSocket.OPEN) throw new Error('终端尚未连接。');
    const bytes = encoder.encode(text);
    if (bytes.length > 4096) throw new Error('单次输入不能超过 4096 字节。');
    if (socket.bufferedAmount > 16384) throw new Error('终端输入繁忙，请稍后再试。');
    socket.send(bytes);
  }
  connect.addEventListener('click', guard(async () => {
    if (connect.disabled) return;
    const epoch = ++generation;
    pending = true; controls();
    try {
      const confirmed = await confirmAction({ title: '连接高风险终端', description: '命令将以面板服务的系统账号执行，可能修改或删除服务器数据。会话开始、结束和关闭原因会被审计。', target: 'Web Terminal', confirm: '连接终端' });
      if (!confirmed || epoch !== generation || location.hash !== '#terminal') return;
      const { ticket } = await api('/api/terminal/ticket', { method: 'POST', headers: { 'X-CSRF-Token': $('meta[name="csrf-token"]').content } });
      if (epoch !== generation || location.hash !== '#terminal') return;
      const url = new URL('/ws/terminal', location.href);
      url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
      const ws = new WebSocket(url, ['lightpanel-terminal', `lp-ticket.${ticket}`]);
      socket = ws; ws.binaryType = 'arraybuffer';
      const decoder = new TextDecoder();
      output.textContent = '';
      status.textContent = '正在连接…';
      ws.onopen = () => { if (socket !== ws) return; status.textContent = '已连接 · 5 分钟无输入或 30 分钟后自动结束'; controls(); input.focus(); };
      ws.onmessage = event => {
        if (socket !== ws || !(event.data instanceof ArrayBuffer)) return;
        output.textContent = (output.textContent + decoder.decode(event.data, { stream: true })).slice(-65536);
        output.scrollTop = output.scrollHeight;
      };
      ws.onclose = event => {
        if (socket !== ws) return;
        socket = null; input.value = '';
        status.textContent = `连接已结束${event.reason ? `：${event.reason}` : '，可重新连接。'}`;
        controls();
      };
      ws.onerror = () => { if (socket === ws) status.textContent = '连接失败，请检查会话、连接额度和服务器配置。'; };
    } finally { if (epoch === generation) { pending = false; controls(); } }
  }));
  disconnect.addEventListener('click', stop);
  $('#terminal-form').addEventListener('submit', guard(() => { send(input.value + '\r'); input.value = ''; }));
  input.addEventListener('keydown', event => { if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) { event.preventDefault(); $('#terminal-form').requestSubmit(); } });
  $('#terminal-interrupt').addEventListener('click', guard(() => send('\x03')));
  $('#terminal-eof').addEventListener('click', guard(() => send('\x04')));
  window.addEventListener('pagehide', stop);
  window.addEventListener('hashchange', () => { if (location.hash !== '#terminal') stop(); });
  status.textContent = readOnly ? '只读账号不能使用终端。' : !enabled ? '管理员已关闭 Web Terminal。' : !ready ? '请将 public_origin 配置为实际访问的固定地址，终端不接受通配地址。' : '未连接';
  controls();
}
