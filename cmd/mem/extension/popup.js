// Popup: save a note (with the page link and selection), search notes, ask the AI.
// One mem process per popup, through native messaging; same requests as `mem serve`.
const $ = (id) => document.getElementById(id);
const handlers = new Map();
let nextId = 1, ready, page = { title: '', url: '' }, windowId;

const port = chrome.runtime.connectNative('com.introvert.mem');
const readyP = new Promise((r) => (ready = r));
port.onMessage.addListener((m) => {
  if (m.id === 0) { if (m.ev === 'ready') ready(m); else status(m.text, 'err'); return; }
  const h = handlers.get(m.id);
  if (h) h(m);
  if (['notes', 'done', 'error', 'ok', 'saved', 'output'].includes(m.ev)) handlers.delete(m.id);
});
port.onDisconnect.addListener(() => {
  const why = chrome.runtime.lastError?.message || 'mem stopped';
  status(why.includes('not found') ? 'mem is not connected: run `mem extension` in a terminal' : why, 'err');
});

async function send(op, params, handler) {
  await readyP;
  const id = nextId++;
  handlers.set(id, handler || (() => {}));
  port.postMessage({ id, op, ...params });
  return id;
}

function status(text, kind = '') { $('status').textContent = text || ''; $('status').className = kind; }

// ---------- save ----------

async function init() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  windowId = tab ? tab.windowId : (await chrome.windows.getCurrent()).id;
  if (tab && /^https?:/.test(tab.url)) {
    page = { title: tab.title, url: tab.url };
    $('pageTitle').textContent = tab.title || tab.url;
    try {
      const [{ result }] = await chrome.scripting.executeScript({ target: { tabId: tab.id }, func: () => String(getSelection()) });
      if (result && result.trim()) $('note').value = result.trim().split('\n').map((l) => '> ' + l).join('\n') + '\n\n';
    } catch (_) { /* pages scripts can't touch */ }
  } else {
    $('withLink').checked = false;
    $('withLink').parentElement.hidden = true;
  }
  $('note').focus();
  $('note').setSelectionRange($('note').value.length, $('note').value.length);
  search('');
}

function save() {
  let text = $('note').value.trim();
  if ($('withLink').checked && page.url) text += (text ? '\n\n' : '') + 'Source: [' + (page.title || page.url).replace(/[\[\]]/g, '') + '](' + page.url + ')';
  if (!text) return;
  $('saveBtn').disabled = true;
  send('add', { text }, (m) => {
    $('saveBtn').disabled = false;
    if (m.ev === 'saved') { status('Saved ' + m.note, 'ok'); $('note').value = ''; setTimeout(() => window.close(), 900); }
    else status(m.text, 'err');
  });
}

$('saveBtn').onclick = save;
$('note').addEventListener('keydown', (e) => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); save(); } });

// ---------- search & ask ----------

let searchId = 0, timer;
$('q').addEventListener('input', () => {
  clearTimeout(timer);
  const q = $('q').value;
  if (q.startsWith('?')) { $('results').innerHTML = ''; status('↩ to ask'); return; }
  if (q.startsWith('!')) { $('results').innerHTML = ''; status('↩ to let mem do it in a new tab (opens the side panel)'); return; }
  status('');
  timer = setTimeout(() => search(q), 80);
});
$('q').addEventListener('keydown', (e) => {
  if (e.key !== 'Enter') return;
  const q = $('q').value.trim();
  if (q.startsWith('!') && q.length > 1) doTask(q.slice(1).trim());
  else if (q.startsWith('?') && q.length > 1) ask(q.slice(1).trim());
  else if (q && (e.metaKey || e.ctrlKey)) ask(q);
});

async function search(q) {
  const id = await send('search', { q, limit: 20 }, (m) => {
    if (id !== searchId || m.ev !== 'notes') return;
    $('answer').hidden = true;
    const ul = $('results');
    ul.innerHTML = '';
    for (const n of m.notes) {
      const li = document.createElement('li');
      li.innerHTML = `<div class="title"></div><div class="meta"></div>`;
      li.querySelector('.title').textContent = n.title.replace(/(^|\s)#[\p{L}\p{N}_/-]+/gu, '').trim() || n.id;
      li.querySelector('.meta').innerHTML = esc(n.created.slice(0, 10) + '  ' + n.id) + '  ' + n.tags.map((t) => `<span class="tag">#${esc(t)}</span>`).join(' ');
      li.onclick = () => {
        if (li.classList.toggle('open')) {
          const b = document.createElement('div');
          b.className = 'body';
          b.innerHTML = inline(n.body);
          const copy = document.createElement('button');
          copy.className = 'small'; copy.textContent = 'Copy note';
          copy.onclick = (e) => { e.stopPropagation(); send('copy_note', { q: n.id }, (r) => status(r.text, r.ev === 'ok' ? 'ok' : 'err')); };
          li.append(b, copy);
        } else {
          li.querySelectorAll('.body, button').forEach((x) => x.remove());
        }
      };
      ul.append(li);
    }
    if (q && !m.notes.length) status('No matches · ⌘↩ asks AI instead');
    else if (m.corrected) status('Showing close matches');
  });
  searchId = id;
}

// doTask hands the task to the background (which runs it) and opens the side panel to follow it.
// sidePanel.open must be called right in the key handler (it needs the user gesture): no awaits first.
function doTask(task) {
  chrome.sidePanel.open({ windowId }).catch(() => {});
  chrome.runtime.sendMessage({ type: 'start', task });
  setTimeout(() => window.close(), 150);
}

function ask(q) {
  $('results').innerHTML = '';
  const box = $('answer');
  box.hidden = false;
  box.innerHTML = `<div class="q"></div><div class="text"></div>`;
  box.querySelector('.q').textContent = q;
  const textEl = box.querySelector('.text');
  let raw = '';
  status('searching notes…');
  send('ask', { q, new: true }, (m) => {
    if (m.ev === 'status') status(m.text);
    if (m.ev === 'text') { raw += m.text; textEl.innerHTML = render(raw); status(''); }
    if (m.ev === 'error') status(m.text, 'err');
    if (m.ev === 'done') {
      textEl.innerHTML = render(m.answer);
      status('');
      if (m.items.length) {
        const list = document.createElement('div');
        list.className = 'items';
        for (const it of m.items.filter((i) => i.kind !== 'cmd')) {
          const row = document.createElement('div');
          row.className = 'item';
          row.innerHTML = `<button class="small">Copy</button><span></span>`;
          row.querySelector('span').textContent = (it.kind === 'secret' ? '🔑 ' : '') + it.label;
          row.querySelector('button').onclick = () => send('item', { n: it.n, action: 'copy' }, (r) => status(r.text, r.ev === 'ok' ? 'ok' : 'err'));
          list.append(row);
        }
        box.append(list);
      }
    }
  });
}

// ---------- rendering (escape first, then a little markdown) ----------

function esc(s) { return s.replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c])); }

function inline(s) {
  return esc(s)
    .replace(/\[REDACTED:([a-z-]+)#\d+\]/g, '<span class="secret">‹$1›</span>')
    .replace(/`([^`\n]+)`/g, '<code>$1</code>')
    .replace(/\*\*([^*\n]+)\*\*/g, '<b>$1</b>')
    .replace(/\[([^\[\]\s]+|[^\[\]]*\/[^\[\]]*)\]/g, '<span class="cite">[$1]</span>');
}

function render(s) {
  return s.split(/```[^\n]*\n?/).map((part, i) => (i % 2 ? `<pre><code>${inline(part.replace(/\n$/, ''))}</code></pre>` : inline(part))).join('');
}

init();
