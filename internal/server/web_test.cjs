// Lightweight SPA event-flow regressions, run with:
// node --test internal/server/web_test.cjs
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const script = fs.readFileSync(path.join(__dirname, 'web/index.html'), 'utf8').match(/<script>([\s\S]*?)<\/script>/)[1];
const flush = () => new Promise(setImmediate);

async function app() {
  class Element {
    constructor() {
      this.value = ''; this.textContent = ''; this.innerHTML = ''; this.disabled = false;
      this.style = {}; this.dataset = {}; this.children = []; this.events = {};
      const classes = new Set();
      this.classList = {toggle: (c, on) => on ? classes.add(c) : classes.delete(c), contains: c => classes.has(c), add: c => classes.add(c), remove: c => classes.delete(c)};
    }
    addEventListener(name, fn) { this.events[name] = fn; }
    appendChild(el) { this.children.push(el); if (!this.value && el.value) this.value = el.value; }
    querySelector() { return new Element(); }
    querySelectorAll() { return []; }
    focus() {}
    click() { return this.events.click?.({target: this, preventDefault(){}}); }
  }
  const elements = new Map(), requests = [], sockets = [], fixtures = new Map();
  const $ = id => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  const context = {
    document: {getElementById: $, querySelectorAll: () => [], createElement: () => new Element()},
    location: {protocol: 'http:', host: 'example.invalid'},
    setTimeout: () => 1, clearTimeout: () => {}, confirm: () => true,
    fetch: async (url, opts) => {
      requests.push({url, opts});
      const data = fixtures.has(url) ? fixtures.get(url) : url === '/api/agents' ? [{key:'demo', name:'Demo'}]
        : url.endsWith('/vision') ? {vision:true} : url === '/api/agents/demo' ? {config:{}}
        : url === '/api/providers' ? [] : {};
      return {ok:true, json:async()=>data};
    },
    WebSocket: class {
      static OPEN = 1;
      constructor() { this.readyState=1; this.sent=[]; sockets.push(this); }
      send(data) {this.sent.push(JSON.parse(data));}
    },
  };
  context.window=context;
  vm.runInNewContext(script, context);
  await flush();
  return {$, requests, sockets, context, fixtures};
}

test('initial vision status enables attach and reconnect unlocks chat', async () => {
  const {$, sockets} = await app();
  assert.equal($('attach').disabled, false);
  $('input').value='hello'; $('send').click();
  assert.equal($('send').disabled, true);
  assert.ok(sockets[0].sent[0].id);
  sockets[0].onclose();
  assert.equal($('send').disabled, false);
  assert.equal($('input').disabled, false);
  assert.equal($('chat-cancel').style.display, 'none');
});

test('Stop targets current turn and late events do not unlock a newer turn', async () => {
  const {$, sockets} = await app();
  $('input').value='hello'; $('send').click();
  const socket=sockets[0], id=socket.sent[0].id;
  $('chat-cancel').click();
  assert.equal(socket.sent[1].kind, 'cancel');
  assert.equal(socket.sent[1].id, id);
  socket.onmessage({data:JSON.stringify({id,kind:'cancelled'})});
  assert.equal($('send').disabled,false);
  $('input').value='next'; $('send').click();
  socket.onmessage({data:JSON.stringify({id,kind:'done'})});
  assert.equal($('send').disabled,true);
});

test('successful provider save reports success', async () => {
  const {$} = await app();
  $('pf-name').value='provider'; $('pf-baseurl').value='http://example.invalid/v1'; $('pf-model').value='model';
  $('pf-save').click(); await flush();
  assert.equal($('toast').textContent,'Provider saved');
});

test('MCP endpoint quotes remain inside the title attribute', async () => {
  const {$, context, fixtures} = await app();
  fixtures.set('/api/mcp-servers',[{id:'test',name:'test',transport:'http',url:'http://example.invalid/" onmouseover="injected',connected:false}]);
  context.showPage('mcp'); await flush();
  const html=$('mcp-tbody').children[0].innerHTML;
  assert.ok(html.includes('&quot; onmouseover=&quot;injected'));
  assert.ok(!html.includes('" onmouseover="'));
});

test('Huma details are displayed by API actions', async () => {
  const {$, context} = await app();
  context.fetch=async()=>({ok:false,statusText:'Bad Request',json:async()=>({detail:'Provider URL is invalid'})});
  $('pf-name').value='provider'; $('pf-baseurl').value='invalid'; $('pf-model').value='model';
  $('pf-save').click(); await flush();
  assert.equal($('toast').textContent,'Save failed: Provider URL is invalid');
});

test('LLM reset uses the capability-preserving endpoint', async () => {
  const {$, context, fixtures, requests} = await app();
  fixtures.set('/api/agents/demo', {key:'demo', name:'Demo', files:[], config:{
    model:'pinned', enabled_builtin_tools:[], enabled_skills:['review'],
    enabled_tools:['mcp_test_echo'], enabled_commands:['kubectl']
  }});
  context.showPage('agents'); await flush();
  $('agents-grid').children[0].click(); await flush();
  $('cf-reset').click(); await flush();
  const reset = requests.find(r => r.opts?.method === 'PUT');
  assert.equal(reset.url, '/api/agents/demo/llm-config');
  assert.deepEqual(JSON.parse(reset.opts.body), {});
});
