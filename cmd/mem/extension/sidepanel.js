// Side panel: start `mem do` tasks and follow them — steps, confirmations, questions, reports.
const $ = (id) => document.getElementById(id);
const port = chrome.runtime.connect({ name: 'panel' });
let drafts = {}; // run id → text typed in an answer box, kept across re-renders

port.onMessage.addListener(render);

function start() {
  const task = $('task').value.trim();
  if (!task) return;
  port.postMessage({ type: 'start', task });
  $('task').value = '';
}
$('go').onclick = start;
$('task').addEventListener('keydown', (e) => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); start(); } });

function el(tag, cls, text) { const e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; }

function render(st) {
  $('conn').textContent = st.connected ? '' : (st.error || 'connecting to mem…');
  const box = $('runs');
  const focused = document.activeElement && document.activeElement.dataset.run;
  box.innerHTML = '';
  for (const r of st.runs) {
    const div = el('div', 'run');
    div.append(el('div', 'task', r.task));
    div.append(el('div', 'state', r.done ? (r.done.success ? 'done' : 'not done') : r.pending ? 'waiting for you' : 'working… (' + r.steps.length + ' steps)'));
    const ul = el('ul', 'steps');
    for (const s of r.steps.slice(-40)) ul.append(el('li', '', s));
    div.append(ul);
    if (r.pending) div.append(card(r));
    if (r.done) {
      div.append(el('div', 'summary ' + (r.done.success ? 'ok' : 'fail'), r.done.answer));
      if (r.done.note) div.append(el('div', 'note', 'Logged to ' + r.done.note));
    }
    box.append(div);
    setTimeout(() => (ul.scrollTop = ul.scrollHeight));
  }
  if (focused) { const i = box.querySelector(`input[data-run="${focused}"]`); if (i) { i.focus(); i.setSelectionRange(i.value.length, i.value.length); } }
}

function card(r) {
  const p = r.pending, c = el('div', 'card');
  const reply = (ok, text) => { delete drafts[r.id]; port.postMessage({ type: 'reply', run: r.id, ok, text }); };
  const input = el('input');
  input.dataset.run = r.id;
  input.value = drafts[r.id] || '';
  input.oninput = () => (drafts[r.id] = input.value);
  const btns = el('div', 'btns');
  if (p.kind === 'confirm') {
    c.append(el('div', 'what', '⚠ About to ' + p.what));
    c.append(el('div', 'url', p.url));
    if (p.fields.length) c.append(el('div', 'fields', p.fields.join('\n')));
    const yes = el('button', '', 'Send');
    yes.onclick = () => reply(true, '');
    const no = el('button', 'secondary', "Don't send");
    no.onclick = () => reply(false, input.value.trim());
    input.placeholder = 'optional: why not / what to change';
    input.onkeydown = (e) => { if (e.key === 'Enter' && input.value.trim()) reply(false, input.value.trim()); };
    btns.append(yes, no, input);
  } else {
    c.append(el('div', 'what', '? ' + p.question));
    input.placeholder = 'Your answer · ↩ sends';
    input.onkeydown = (e) => { if (e.key === 'Enter') reply(true, input.value.trim()); };
    const ok = el('button', '', 'Answer');
    ok.onclick = () => reply(true, input.value.trim());
    btns.append(input, ok);
  }
  c.append(btns);
  return c;
}
