import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../static/terminal.js', import.meta.url), 'utf8');
function harness({ readOnly = false, enabled = true, ready = true, api = async () => ({ ticket: 'once' }) } = {}) {
  const nodes = new Map(), events = {}, sockets = [];
  const $ = selector => {
    if (!nodes.has(selector)) nodes.set(selector, { disabled: false, value: '', content: 'csrf', textContent: '', handlers: {}, addEventListener(name, fn) { this.handlers[name] = fn; }, focus() {} });
    return nodes.get(selector);
  };
  class WebSocket {
    static OPEN = 1;
    constructor(url, protocols) { this.url = String(url); this.protocols = protocols; this.readyState = 0; this.bufferedAmount = 0; this.sent = []; sockets.push(this); }
    send(data) { this.sent.push(data); }
    close() { this.closed = true; this.readyState = 3; }
    open() { this.readyState = 1; this.onopen(); }
  }
  const location = { hash: '#terminal', href: 'https://panel.example/#terminal', protocol: 'https:' };
  const context = vm.createContext({ $, api, guard: fn => fn, readOnly, confirmAction: async () => true,
    TextEncoder, TextDecoder, ArrayBuffer, URL, WebSocket, location,
    document: { body: { dataset: { terminalEnabled: String(enabled), terminalReady: String(ready) } } },
    window: { addEventListener(name, fn) { events[name] = fn; } },
  });
  vm.runInContext(source.replace(/^import .*;\r?\n/gm, '').replace(/^export /gm, ''), context);
  context.setupTerminal();
  return { $, events, sockets, location };
}

test('ticket is sent in subprotocol, output is bounded text, leave disconnects', async () => {
  const { $, sockets, events, location } = harness();
  await $('#terminal-connect').handlers.click();
  const ws = sockets[0];
  assert.equal(ws.url, 'wss://panel.example/ws/terminal');
  assert.deepEqual([...ws.protocols], ['lightpanel-terminal', 'lp-ticket.once']);
  ws.open();
  ws.onmessage({ data: new TextEncoder().encode('<script>bad()</script>').buffer });
  assert.equal($('#terminal-output').textContent, '<script>bad()</script>');
  ws.onmessage({ data: new TextEncoder().encode('x'.repeat(70000)).buffer });
  assert.equal($('#terminal-output').textContent.length, 65536);
  $('#terminal-input').value = 'echo hello';
  $('#terminal-form').handlers.submit();
  assert.equal(new TextDecoder().decode(ws.sent[0]), 'echo hello\r');
  location.hash = '#overview'; events.hashchange();
  assert.equal(ws.closed, true);
  assert.equal($('#terminal-input').disabled, true);
});

test('late ticket response after navigating away cannot open a shell', async () => {
  let resolve;
  const { $, sockets, events, location } = harness({ api: () => new Promise(done => { resolve = done; }) });
  const connecting = $('#terminal-connect').handlers.click();
  await new Promise(done => setImmediate(done));
  location.hash = '#overview'; events.hashchange();
  resolve({ ticket: 'late' }); await connecting;
  assert.equal(sockets.length, 0);
});

test('readonly, disabled and wildcard configuration block connecting', async () => {
  for (const options of [{ readOnly: true }, { enabled: false }, { ready: false }]) {
    const { $, sockets } = harness(options);
    assert.equal($('#terminal-connect').disabled, true);
    await $('#terminal-connect').handlers.click();
    assert.equal(sockets.length, 0);
  }
});
