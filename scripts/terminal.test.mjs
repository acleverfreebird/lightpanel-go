import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../static/terminal.js', import.meta.url), 'utf8');

// Minimal stubs for the vendored UMD globals and the DOM surface the module
// touches; the socket records everything sent through it.
function harness({ readOnly = false, enabled = true } = {}) {
  const nodes = new Map(), events = {}, sockets = [], writes = [];
  let onData, onResize;
  const $ = selector => {
    if (!nodes.has(selector)) nodes.set(selector, { disabled: false, textContent: '', handlers: {}, addEventListener(name, fn) { this.handlers[name] = fn; }, focus() {}, blur() {} });
    return nodes.get(selector);
  };
  class Terminal {
    constructor() { this.handlers = {}; }
    loadAddon() {}
    open() {}
    reset() {}
    focus() {}
    blur() {}
    onData(fn) { onData = fn; }
    onResize(fn) { onResize = fn; }
    write(data) { writes.push(data); }
  }
  const FitAddon = { FitAddon: class { fit() {} } };
  class ResizeObserver { constructor(fn) { this.fn = fn; } observe() {} disconnect() {} }
  class WebSocket {
    static OPEN = 1;
    constructor(url) { this.url = String(url); this.readyState = 0; this.sent = []; sockets.push(this); }
    send(data) { this.sent.push(data); }
    close() { this.closed = true; this.readyState = 3; }
    open() { this.readyState = 1; this.onopen(); }
  }
  const location = { href: 'https://panel.example/#terminal', protocol: 'https:' };
  const context = vm.createContext({ $, guard: fn => fn, readOnly,
    // Host-realm globals: the module does `instanceof ArrayBuffer` on message
    // payloads the test creates outside the vm, so both sides must agree.
    ArrayBuffer, Terminal, FitAddon, ResizeObserver, WebSocket, URL, location,
    document: { body: { dataset: { terminalEnabled: String(enabled) } } },
    window: { addEventListener(name, fn) { events[name] = fn; } },
  });
  vm.runInContext(source.replace(/^import .*;\r?\n/gm, '').replace(/^export /gm, ''), context);
  context.setupTerminal();
  return { $, events, sockets, writes, keystroke: data => onData(data), resize: (rows, cols) => onResize({ rows, cols }) };
}

test('connect opens a plain session websocket and streams both ways', async () => {
  const { $, sockets, writes, keystroke, resize } = harness();
  await $('#terminal-connect').handlers.click();
  const ws = sockets[0];
  assert.equal(ws.url, 'wss://panel.example/ws/terminal');
  assert.equal(ws.readyState, 0);
  assert.equal($('#terminal-connect').disabled, true);
  ws.open();
  assert.equal($('#terminal-status').textContent, '已连接');
  keystroke('echo hello\r');
  assert.deepEqual(ws.sent, ['echo hello\r']);
  resize(30, 120);
  assert.deepEqual(ws.sent, ['echo hello\r', JSON.stringify({ type: 'resize', rows: 30, cols: 120 })]);
  ws.onmessage({ data: new TextEncoder().encode('hello\n').buffer });
  assert.equal(new TextDecoder().decode(writes.at(-1)), 'hello\n');
  $('#terminal-disconnect').handlers.click();
  assert.equal(ws.closed, true);
  assert.equal($('#terminal-connect').disabled, false);
});

test('output arriving before open and status transitions are handled', async () => {
  const { $, sockets, writes } = harness();
  await $('#terminal-connect').handlers.click();
  const ws = sockets[0];
  ws.onmessage({ data: new TextEncoder().encode('banner\n').buffer });
  assert.equal(new TextDecoder().decode(writes.at(-1)), 'banner\n');
  assert.equal($('#terminal-status').textContent, '正在连接…');
  ws.onclose({ reason: 'shell_exit' });
  assert.equal($('#terminal-status').textContent, '连接已结束：shell_exit');
  assert.equal($('#terminal-connect').disabled, false);
});

test('late onclose from a superseded socket is ignored', async () => {
  const { $, sockets } = harness();
  await $('#terminal-connect').handlers.click();
  const first = sockets[0];
  $('#terminal-disconnect').handlers.click();
  await $('#terminal-connect').handlers.click();
  const second = sockets[1];
  first.onclose({ reason: 'client_disconnected' });
  assert.equal($('#terminal-connect').disabled, true, 'second session still active');
  second.close();
});

test('pagehide closes the socket', async () => {
  const { $, events, sockets } = harness();
  await $('#terminal-connect').handlers.click();
  const ws = sockets[0];
  events.pagehide();
  assert.equal(ws.closed, true);
  assert.equal($('#terminal-connect').disabled, false);
});

test('readonly and disabled configuration block connecting', async () => {
  for (const options of [{ readOnly: true }, { enabled: false }]) {
    const { $, sockets } = harness(options);
    assert.equal($('#terminal-connect').disabled, true);
    await $('#terminal-connect').handlers.click();
    assert.equal(sockets.length, 0);
  }
});
