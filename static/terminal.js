import { $, guard, readOnly } from './ui.js';

// Full interactive Web Shell on a real PTY: xterm.js renders the terminal,
// keystrokes stream to /ws/terminal as binary frames, output streams back,
// and window resizing is reported as JSON control messages. xterm.js and the
// fit addon are vendored UMD builds (see static/vendor/xterm) exposing the
// window globals Terminal and FitAddon.
export function setupTerminal() {
  const connect = $('#terminal-connect'), disconnect = $('#terminal-disconnect');
  const status = $('#terminal-status'), container = $('#terminal-container');
  const enabled = document.body.dataset.terminalEnabled === 'true';
  let socket = null, term = null, fit = null, observer = null;

  function controls() {
    connect.disabled = readOnly || !enabled || !!socket;
    disconnect.disabled = !socket;
  }

  function syncSize() {
    // fit() recomputes rows/cols; onResize forwards the new grid to the server.
    if (term && fit && socket?.readyState === WebSocket.OPEN) fit.fit();
  }

  function stop(message) {
    observer?.disconnect();
    observer = null;
    socket?.close();
    socket = null;
    term?.blur();
    status.textContent = message ?? '已断开；重新连接会创建新会话。';
    controls();
  }

  connect.addEventListener('click', guard(() => {
    if (connect.disabled) return;
    if (!term) {
      term = new Terminal({ cursorBlink: true, fontSize: 13, scrollback: 5000, theme: { background: '#101820' } });
      fit = new FitAddon.FitAddon();
      term.loadAddon(fit);
      term.open(container);
      term.onData(data => { if (socket?.readyState === WebSocket.OPEN) socket.send(new TextEncoder().encode(data)); });
      term.onResize(({ rows, cols }) => {
        if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify({ type: 'resize', rows, cols }));
      });
      observer = new ResizeObserver(syncSize);
      observer.observe(container);
    }
    term.reset();
    term.focus();
    const url = new URL('/ws/terminal', location.href);
    url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(url);
    socket = ws;
    ws.binaryType = 'arraybuffer';
    status.textContent = '正在连接…';
    controls();
    ws.onopen = () => {
      if (socket !== ws) return;
      status.textContent = '已连接';
      controls();
      syncSize();
      term.focus();
    };
    ws.onmessage = event => {
      if (socket === ws && event.data instanceof ArrayBuffer) term.write(new Uint8Array(event.data));
    };
    ws.onclose = event => {
      if (socket !== ws) return;
      socket = null;
      status.textContent = `连接已结束${event.reason ? `：${event.reason}` : '，可重新连接。'}`;
      controls();
    };
    ws.onerror = () => { if (socket === ws) status.textContent = '连接失败，请确认面板服务运行正常。'; };
  }));
  disconnect.addEventListener('click', () => stop());
  window.addEventListener('pagehide', () => stop());
  status.textContent = readOnly ? '只读账号不能使用终端。' : !enabled ? '管理员已关闭 Web Terminal。' : '未连接';
  controls();
}
