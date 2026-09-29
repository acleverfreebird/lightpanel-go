import { $, readOnly } from './ui.js';

// 宝塔面板风格的网页终端：进入「终端」页即自动连接一个真实 PTY 会话，
// 整页黑色控制台由本地内置的 xterm.js 渲染（static/vendor/xterm，暴露
// Terminal 与 FitAddon 全局）。键盘输入与 PTY 输出经 /ws/terminal 以二进制
// 帧直传，窗口尺寸变化以 JSON 控制帧上报。切到其他模块时保持会话，回到
// 终端页自动恢复；关闭标签页即断开。
export function setupTerminal() {
  const status = $('#terminal-status');
  const container = $('#terminal-container');
  const clearButton = $('#terminal-clear');
  const reconnectButton = $('#terminal-reconnect');
  const available = !readOnly && document.body.dataset.terminalEnabled === 'true';
  let socket = null, term = null, fit = null, observer = null;

  function setStatus(text, state = '') {
    status.textContent = text;
    status.className = `badge ${state}`.trim();
  }

  function controls() {
    clearButton.disabled = !term;
    // 握手进行中不允许重连；已连接时点击重连表示放弃当前会话、另开新会话。
    reconnectButton.disabled = !available || socket?.readyState === WebSocket.CONNECTING;
  }

  function syncSize() {
    if (term && fit && socket?.readyState === WebSocket.OPEN) fit.fit();
  }

  function disconnect() {
    const ws = socket;
    if (!ws) return;
    // 先摘除引用，旧 socket 的 onclose 便不会覆盖后续状态。
    socket = null;
    observer?.disconnect();
    observer = null;
    term?.blur();
    ws.close();
  }

  function connect() {
    if (!available || socket) return;
    if (!term) {
      term = new Terminal({ cursorBlink: true, fontSize: 13, scrollback: 5000, theme: { background: '#101820' } });
      fit = new FitAddon.FitAddon();
      term.loadAddon(fit);
      term.open(container);
      term.onData(data => { if (socket?.readyState === WebSocket.OPEN) socket.send(new TextEncoder().encode(data)); });
      term.onResize(({ rows, cols }) => {
        if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify({ type: 'resize', rows, cols }));
      });
    }
    term.reset();
    term.focus();
    observer = new ResizeObserver(syncSize);
    observer.observe(container);
    const url = new URL('/ws/terminal', location.href);
    url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(url);
    socket = ws;
    ws.binaryType = 'arraybuffer';
    setStatus('连接中…', 'warn');
    controls();
    ws.onopen = () => {
      if (socket !== ws) return;
      setStatus('已连接', 'good');
      controls();
      syncSize();
      // 新 PTY 固定以 80x24 启动，即使终端网格没有变化也要同步一次实际尺寸。
      ws.send(JSON.stringify({ type: 'resize', rows: term.rows, cols: term.cols }));
      term.focus();
    };
    ws.onmessage = event => {
      if (socket === ws && event.data instanceof ArrayBuffer) term.write(new Uint8Array(event.data));
    };
    ws.onclose = event => {
      if (socket !== ws) return;
      socket = null;
      observer?.disconnect();
      observer = null;
      controls();
      setStatus(event.reason ? `已断开：${event.reason}` : '已断开，可重新连接。', 'bad');
    };
    ws.onerror = () => { if (socket === ws) setStatus('连接失败，请确认面板服务运行正常。', 'bad'); };
  }

  clearButton.addEventListener('click', () => { term?.clear(); term?.focus(); });
  reconnectButton.addEventListener('click', () => { disconnect(); connect(); });
  window.addEventListener('pagehide', disconnect);

  if (!available) setStatus(readOnly ? '只读账号不能使用终端。' : '管理员已关闭终端。', 'bad');
  controls();

  // app.js 在切换到终端页时调用：未连接则立即连接（打开即连），已连接则
  // 只重新适配一次尺寸（区块刚从隐藏变为可见）。
  return {
    activate() {
      if (available && !socket) connect();
      else syncSize();
    },
  };
}
