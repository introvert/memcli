// mem's extension background:
//  - right-click → "Save … to mem" (one-shot requests);
//  - a long-lived mem ("bridge") that runs `mem do` tasks: it relays DevTools commands to a tab the
//    agent owns (chrome.debugger), and talks to the side panel for progress and confirmations.
//    Terminal `mem do` runs come through the same mem (see extbridge.go).
const HOST = 'com.introvert.mem';

chrome.runtime.onInstalled.addListener(() => {
  chrome.contextMenus.create({ id: 'selection', title: 'Save selection to mem', contexts: ['selection'] });
  chrome.contextMenus.create({ id: 'link', title: 'Save link to mem', contexts: ['link'] });
  chrome.contextMenus.create({ id: 'page', title: 'Save page to mem', contexts: ['page'] });
});

// ---------- one-shot requests (context menu) ----------

function request(op, params) {
  return new Promise((resolve) => {
    let port;
    try { port = chrome.runtime.connectNative(HOST); } catch (e) { resolve({ ev: 'error', text: String(e) }); return; }
    port.onMessage.addListener((m) => {
      if (m.id === 0 && m.ev === 'ready') { port.postMessage({ id: 1, op, ...params }); return; }
      if (m.id === 1) { resolve(m); port.disconnect(); }
    });
    port.onDisconnect.addListener(() => resolve({ ev: 'error', text: chrome.runtime.lastError?.message || 'mem stopped' }));
  });
}

function link(title, url) { return `[${(title || url).replace(/[\[\]]/g, '')}](${url})`; }

chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  let text;
  if (info.menuItemId === 'selection') {
    text = info.selectionText.split('\n').map((l) => '> ' + l).join('\n') + '\n\nSource: ' + link(tab.title, info.pageUrl);
  } else if (info.menuItemId === 'link') {
    text = link(info.selectionText || info.linkUrl, info.linkUrl) + '\n\nFound on: ' + link(tab.title, info.pageUrl);
  } else {
    text = link(tab.title, info.pageUrl);
  }
  const r = await request('add', { text: text + ' #web' });
  badge(tab.id, r.ev === 'saved', r.ev === 'saved' ? 'Saved ' + r.note : 'mem: ' + (r.text || 'failed'));
});

function badge(tabId, ok, title) {
  chrome.action.setBadgeBackgroundColor({ color: ok ? '#22a06b' : '#d9534f', tabId });
  chrome.action.setBadgeText({ text: ok ? '✓' : '!', tabId });
  chrome.action.setTitle({ title, tabId });
  setTimeout(() => chrome.action.setBadgeText({ text: '', tabId }), ok ? 2500 : 8000);
}

// ---------- the bridge ----------

let bridge = null, bridgeError = '', nextId = 100;
const runs = new Map(); // id → {id, task, steps: [], pending: null | {cid, kind, ...}, done: null | {...}}
const panels = new Set();
const attached = new Set(); // agent tabs with the debugger attached

function connectBridge() {
  try { bridge = chrome.runtime.connectNative(HOST); } catch (e) { bridgeError = String(e); return; }
  bridgeError = '';
  bridge.onMessage.addListener(onBridge);
  bridge.onDisconnect.addListener(() => {
    bridgeError = chrome.runtime.lastError?.message || 'mem stopped';
    bridge = null;
    for (const r of runs.values()) if (!r.done) { r.done = { answer: 'Stopped: ' + bridgeError, success: false }; r.pending = null; }
    broadcast();
    setTimeout(connectBridge, 3000);
  });
}

function onBridge(m) {
  if (m.id === 0 && m.ev === 'ready') { bridge.postMessage({ id: 1, op: 'bridge' }); return; }
  if (m.ev === 'cdp') { cdp(m); return; }
  const run = runs.get(m.id);
  if (!run) return;
  if (m.ev === 'do_step') run.steps.push(m.text);
  if (m.ev === 'do_confirm') run.pending = { kind: 'confirm', cid: m.cid, what: m.what, url: m.url, fields: m.fields || [] };
  if (m.ev === 'do_ask') run.pending = { kind: 'ask', cid: m.cid, question: m.question };
  if (m.ev === 'done' || m.ev === 'error') { run.done = { answer: m.answer || m.text, success: !!m.success, note: m.note }; run.pending = null; }
  broadcast();
}

// cdp runs one relayed DevTools command; "mem.*" methods manage the agent's tab.
async function cdp(m) {
  const reply = (result, error) => bridge && bridge.postMessage({ id: 0, op: 'cdp_result', n: m.cid, result, error });
  try {
    if (m.method === 'mem.newTab') {
      const win = await chrome.windows.getLastFocused({ windowTypes: ['normal'] });
      const tab = await chrome.tabs.create({ windowId: win.id, url: 'about:blank', active: true });
      const group = await chrome.tabs.group({ tabIds: tab.id });
      chrome.tabGroups.update(group, { title: 'mem', color: 'purple' });
      await chrome.debugger.attach({ tabId: tab.id }, '1.3');
      attached.add(tab.id);
      return reply({ tabId: tab.id });
    }
    if (m.method === 'mem.close') {
      if (attached.delete(m.tab)) await chrome.debugger.detach({ tabId: m.tab }).catch(() => {});
      return reply({});
    }
    if (!attached.has(m.tab)) return reply(undefined, 'the agent tab was closed or detached');
    if (m.method === 'Page.bringToFront') {
      const tab = await chrome.tabs.update(m.tab, { active: true });
      await chrome.windows.update(tab.windowId, { focused: true });
      return reply({});
    }
    reply(await chrome.debugger.sendCommand({ tabId: m.tab }, m.method, m.params || {}));
  } catch (e) {
    reply(undefined, String(e.message || e));
  }
}

chrome.debugger.onDetach.addListener((src) => attached.delete(src.tabId)); // e.g. you pressed "Cancel" on Chrome's bar
chrome.tabs.onRemoved.addListener((tabId) => attached.delete(tabId));

// ---------- side panel ----------

chrome.runtime.onConnect.addListener((port) => {
  if (port.name !== 'panel') return;
  panels.add(port);
  port.onDisconnect.addListener(() => panels.delete(port));
  port.onMessage.addListener((m) => {
    if (m.type === 'start') start(m.task);
    if (m.type === 'reply') answer(m.run, m.ok, m.text);
  });
  send(port);
});

function state() {
  return { connected: !!bridge, error: bridgeError, runs: [...runs.values()].slice(-10).reverse() };
}
function send(port) { port.postMessage(state()); }
function broadcast() { for (const p of panels) send(p); }

function start(task) {
  task = (task || '').trim();
  if (!task) return;
  if (!bridge) connectBridge();
  if (!bridge) { bridgeError = bridgeError || 'mem is not connected: run `mem extension`'; broadcast(); return; }
  const id = nextId++;
  runs.set(id, { id, task, steps: [], pending: null, done: null, started: Date.now() });
  bridge.postMessage({ id, op: 'do', q: task });
  broadcast();
}

function answer(id, ok, text) {
  const run = runs.get(id);
  if (!run || !run.pending || !bridge) return;
  bridge.postMessage({ id: 0, op: 'do_reply', n: run.pending.cid, ok: !!ok, text: text || '' });
  run.steps.push(run.pending.kind === 'confirm' ? (ok ? '✓ you confirmed: ' : '✗ you declined: ') + run.pending.what : 'you answered: ' + (text || '(nothing)'));
  run.pending = null;
  broadcast();
}

// Tasks typed in the popup ("!task") arrive here; the popup opens the side panel itself.
chrome.runtime.onMessage.addListener((m) => { if (m.type === 'start') start(m.task); });

connectBridge();
