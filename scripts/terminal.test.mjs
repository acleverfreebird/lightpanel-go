import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../static/terminal.js', import.meta.url), 'utf8');

// Minimal stubs for the vendored UMD globals and the DOM surface the module
// touches; the socket records everything sent through it.
function harness({ readOnly = false, enabled = true } = {}) {
  const nodes = new Map(), events = {}, sockets = [], writes = [], observers = [], cleared = [];
  let onData, onResize;
  const $ = selector => {
    if (!nodes.has(selector)) nodes.set(selector, { disabled: false, textContent: '', className: '', handlers: {}, addEventListener(name, fn) { this.handlers[name] = fn; }, focus() {}, blur() {} });
    return nodes.get(selector);
  };
  class Terminal {
    constructor() { this.handlers = {}; this.rows = 24; this.cols = 80; }
    loadAddon() {}
    open() {}
    reset() {}
    focus() {}
    blur() {}
    clear() { cleared.push('clear'); }
    onData(fn) { onData = fn; }
    onResize(fn) { onResize = fn; }
    write(data) { writes.push(data); }
  }
  const FitAddon = { FitAddon: class { fit() {} } };
  class ResizeObserver {
    constructor(fn) { this.fn = fn; observers.push(this); }
    observe() { this.active = true; }
    disconnect() { this.active = false; }
  }
  class WebSocket {
    static OPEN = 1;
    static CONNECTING = 0;
    constructor(url) { this.url = String(url); this.readyState = 0; this.sent = []; sockets.push(this); }
    send(data) { this.sent.push(data); }
    close() { this.closed = true; this.readyState = 3; }
    open() { this.readyState = 1; this.onopen(); }
  }
  const location = { href: 'https://panel.example/#terminal', protocol: 'https:' };
  const context = vm.createContext({ $, readOnly,
    // Host-realm globals: the module does `instanceof ArrayBuffer` on message
    // payloads the test creates outside the vm, so both sides must agree.
    ArrayBuffer, TextEncoder, Terminal, FitAddon, ResizeObserver, WebSocket, URL, location,
    document: { body: { dataset: { terminalEnabled: String(enabled) } } },
    window: { addEventListener(name, fn) { events[name] = fn; } },
  });
  vm.runInContext(source.replace(/^import .*;\r?\n/gm, '').replace(/^export /gm, ''), context);
  const terminal = context.setupTerminal();
  return { $, events, sockets, writes, observers, cleared, terminal, keystroke: data => onData(data), resize: (rows, cols) => onResize({ rows, cols }) };
}

test('activating the page connects immediately and streams both ways', () => {
  const { $, sockets, writes, keystroke, resize, cleared, terminal } = harness();
  terminal.activate();
  const ws = sockets[0];
  assert.equal(sockets.length, 1);
  assert.equal(ws.url, 'wss://panel.example/ws/terminal');
  assert.equal($('#terminal-reconnect').disabled, true, 'reconnect blocked while connecting');
  ws.open();
  assert.equal($('#terminal-status').textContent, '已连接');
  assert.equal($('#terminal-status').className, 'badge good');
  assert.equal(ws.sent[0], JSON.stringify({ type: 'resize', rows: 24, cols: 80 }));
  keystroke('echo hello\r');
  assert.equal(new TextDecoder().decode(ws.sent[1]), 'echo hello\r');
  resize(30, 120);
  assert.equal(ws.sent[2], JSON.stringify({ type: 'resize', rows: 30, cols: 120 }));
  ws.onmessage({ data: new TextEncoder().encode('hello\n').buffer });
  assert.equal(new TextDecoder().decode(writes.at(-1)), 'hello\n');
  // 回到终端页时保留在跑会话，而不是再开一条连接。
  terminal.activate();
  assert.equal(sockets.length, 1);
  $('#terminal-clear').handlers.click();
  assert.deepEqual(cleared, ['clear']);
});

test('reconnect replaces the live session and ignores the stale socket', () => {
  const { $, sockets, terminal } = harness();
  terminal.activate();
  const first = sockets[0];
  first.open();
  $('#terminal-reconnect').handlers.click();
  assert.equal(first.closed, true, 'old socket closed immediately');
  const second = sockets[1];
  first.onclose({ reason: 'client_disconnected' });
  second.open();
  assert.equal($('#terminal-status').textContent, '已连接', 'superseded close did not clobber the live session');
  assert.equal(sockets.length, 2);
});

test('remote close frees the slot and reconnect starts a new session', () => {
  const { $, sockets, observers, terminal } = harness();
  terminal.activate();
  const ws = sockets[0];
  ws.open();
  ws.onclose({ reason: 'shell_exit' });
  assert.equal($('#terminal-status').textContent, '已断开：shell_exit');
  assert.equal($('#terminal-status').className, 'badge bad');
  assert.equal(observers.some(observer => observer.active), false);
  assert.equal($('#terminal-reconnect').disabled, false);
  $('#terminal-reconnect').handlers.click();
  assert.equal(sockets.length, 2, 'reconnect opens a fresh session');
  sockets[1].open();
  assert.equal($('#terminal-status').textContent, '已连接');
});

test('pagehide closes the socket', () => {
  const { events, sockets, terminal } = harness();
  terminal.activate();
  const ws = sockets[0];
  events.pagehide();
  assert.equal(ws.closed, true);
});

test('readonly and disabled configuration never connect', () => {
  for (const options of [{ readOnly: true }, { enabled: false }]) {
    const { $, sockets, terminal } = harness(options);
    terminal.activate();
    assert.equal(sockets.length, 0);
    assert.equal($('#terminal-reconnect').disabled, true);
    assert.notEqual($('#terminal-status').textContent, '已连接');
  }
});
