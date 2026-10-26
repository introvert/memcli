// Right-click → "Save … to mem". Talks to mem through native messaging (see native.go).
const HOST = 'com.introvert.mem';

chrome.runtime.onInstalled.addListener(() => {
  chrome.contextMenus.create({ id: 'selection', title: 'Save selection to mem', contexts: ['selection'] });
  chrome.contextMenus.create({ id: 'link', title: 'Save link to mem', contexts: ['link'] });
  chrome.contextMenus.create({ id: 'page', title: 'Save page to mem', contexts: ['page'] });
});

// request sends one request to a fresh mem and resolves with its final reply.
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
  const ok = r.ev === 'saved';
  chrome.action.setBadgeBackgroundColor({ color: ok ? '#22a06b' : '#d9534f', tabId: tab.id });
  chrome.action.setBadgeText({ text: ok ? '✓' : '!', tabId: tab.id });
  chrome.action.setTitle({ title: ok ? 'Saved ' + r.note : 'mem: ' + (r.text || 'failed'), tabId: tab.id });
  setTimeout(() => chrome.action.setBadgeText({ text: '', tabId: tab.id }), ok ? 2500 : 8000);
});
