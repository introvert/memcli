package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// The browser is a real, visible Chrome with its own profile in ~/.mem/chrome, driven over the
// DevTools protocol. Its own profile because Chrome refuses remote control of your everyday one;
// log in to the sites you want mem to use once (`mem browser`) and the profile keeps the session.

const chromePort = 9333

func chromeProfile() string { return filepath.Join(home, "chrome") }

func chromeBinary() (string, error) {
	if p := os.Getenv("MEM_CHROME"); p != "" {
		return p, nil
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		}
	case "windows":
		candidates = []string{`C:\Program Files\Google\Chrome\Application\chrome.exe`, `C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`}
	default:
		candidates = []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("Chrome not found; set MEM_CHROME to its executable")
}

// debuggerURL returns the DevTools websocket of mem's Chrome, starting it when it isn't running.
func debuggerURL(startURL string) (string, error) {
	get := func() (string, error) {
		c := http.Client{Timeout: 700 * time.Millisecond}
		resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", chromePort))
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		var v struct {
			WS string `json:"webSocketDebuggerUrl"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || v.WS == "" {
			return "", fmt.Errorf("no debugger url")
		}
		return v.WS, nil
	}
	if ws, err := get(); err == nil {
		return ws, nil
	}
	bin, err := chromeBinary()
	if err != nil {
		return "", err
	}
	if startURL == "" {
		startURL = "about:blank"
	}
	cmd := exec.Command(bin, fmt.Sprintf("--remote-debugging-port=%d", chromePort), "--remote-debugging-address=127.0.0.1",
		"--user-data-dir="+chromeProfile(), "--no-first-run", "--no-default-browser-check", startURL)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("starting Chrome: %v", err)
	}
	go cmd.Wait() // Chrome outlives us; don't leave a zombie meanwhile
	for i := 0; i < 50; i++ {
		time.Sleep(200 * time.Millisecond)
		if ws, err := get(); err == nil {
			return ws, nil
		}
	}
	return "", fmt.Errorf("Chrome started but its debugging port %d never answered", chromePort)
}

// cmdBrowser opens mem's Chrome so you can log in to sites the agent should use.
func cmdBrowser(args []string) {
	u := "about:blank"
	if len(args) > 0 {
		u = args[0]
		if !strings.Contains(u, "://") {
			u = "https://" + u
		}
	}
	_, err := debuggerURL(u)
	check(err)
	fmt.Println("mem's Chrome is open (profile " + chromeProfile() + ").")
	fmt.Println(c("2", "Log in to the sites you want `mem do` to use; sessions are kept in this profile."))
}

type browser struct {
	ctx    context.Context
	cancel func()
}

func openBrowser() (*browser, error) {
	ws, err := debuggerURL("")
	if err != nil {
		return nil, err
	}
	actx, acancel := chromedp.NewRemoteAllocator(context.Background(), ws)
	ctx, cancel := chromedp.NewContext(actx) // a new tab of our own; your other tabs are left alone
	if err := chromedp.Run(ctx); err != nil {
		cancel()
		acancel()
		return nil, fmt.Errorf("connecting to Chrome: %v", err)
	}
	return &browser{ctx: ctx, cancel: func() { cancel(); acancel() }}, nil
}

func (b *browser) close() { b.cancel() }

func (b *browser) run(timeout time.Duration, actions ...chromedp.Action) error {
	ctx, cancel := context.WithTimeout(b.ctx, timeout)
	defer cancel()
	return chromedp.Run(ctx, actions...)
}

// settle waits for the page to finish loading after an action (bounded, never fails).
func (b *browser) settle() {
	time.Sleep(400 * time.Millisecond)
	for i := 0; i < 20; i++ {
		var state string
		if b.run(2*time.Second, chromedp.Evaluate(`document.readyState`, &state)) == nil && state == "complete" {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
}

type pageState struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Items string `json:"items"`
	Text  string `json:"text"`
	More  bool   `json:"more"`
}

// snapshotJS numbers the visible interactive elements (data-mem="n") and describes them, ⚠ marking
// the ones that send or commit something; it also returns the page's visible text.
const snapshotJS = `(() => {
  const MAX = 200;
  const visible = el => { const r = el.getBoundingClientRect(); const s = getComputedStyle(el);
    return r.width > 0 && r.height > 0 && s.visibility !== 'hidden' && s.display !== 'none'; };
  const risky = /\b(send|submit|buy|pay|purchase|order|checkout|confirm|delete|remove|subscribe|sign ?up|register|book|reserve|publish|post|apply|transfer|place|donate|unsubscribe|cancel (my )?(account|subscription))\b/i;
  document.querySelectorAll('[data-mem]').forEach(e => { e.removeAttribute('data-mem'); e.removeAttribute('data-mem-risky'); });
  const sel = 'a[href],button,input:not([type=hidden]),textarea,select,[role=button],[role=link],[role=checkbox],[role=radio],[role=tab],[role=menuitem],[role=option],[role=combobox],[role=switch],[contenteditable=""],[contenteditable=true],summary';
  let n = 0; const items = [];
  for (const el of document.querySelectorAll(sel)) {
    if (n >= MAX) break;
    if (!visible(el) || el.disabled) continue;
    n++; el.setAttribute('data-mem', n);
    const tag = el.tagName.toLowerCase(), type = (el.getAttribute('type') || '').toLowerCase(), role = el.getAttribute('role') || '';
    let label = el.getAttribute('aria-label') || '';
    if (!label && el.labels && el.labels.length) label = el.labels[0].innerText;
    if (!label && (tag === 'button' || tag === 'a' || role || tag === 'summary')) label = el.innerText;
    if (!label && type === 'submit') label = el.value;
    if (!label) label = el.getAttribute('placeholder') || el.getAttribute('title') || el.getAttribute('name') || el.getAttribute('alt') || '';
    label = (label || '').replace(/\s+/g, ' ').trim().slice(0, 80);
    let kind = role || tag;
    if (tag === 'input') kind = 'input:' + (type || 'text');
    let extra = '';
    if (tag === 'a') extra = ' -> ' + (el.getAttribute('href') || '').slice(0, 90);
    if (tag === 'input' || tag === 'textarea') {
      if (type === 'password') extra = el.value ? ' (filled)' : '';
      else if (type === 'checkbox' || type === 'radio') extra = el.checked ? ' [x]' : ' [ ]';
      else if (el.value) extra = ' = "' + el.value.slice(0, 120) + '"';
    }
    if (el.isContentEditable && el.innerText.trim()) extra = ' = "' + el.innerText.trim().slice(0, 120) + '"';
    if (tag === 'select') extra = ' = "' + (el.selectedOptions[0] ? el.selectedOptions[0].text.trim() : '') + '" options: ' +
      [...el.options].slice(0, 20).map(o => o.text.trim()).join(' | ');
    if (el.required) extra += ' (required)';
    const submit = (tag === 'button' && (type === '' || type === 'submit') && el.form) || (tag === 'input' && (type === 'submit' || type === 'image'));
    const isRisky = submit || ((tag === 'button' || tag === 'a' || role === 'button' || tag === 'input') && risky.test(label));
    if (isRisky) el.setAttribute('data-mem-risky', '1');
    items.push('[' + n + '] ' + kind + ' "' + label + '"' + extra + (isRisky ? ' ⚠' : ''));
  }
  const text = (document.body ? document.body.innerText : '').replace(/[ \t]+/g, ' ').replace(/\n\s*\n+/g, '\n').trim();
  return {url: location.href, title: document.title, items: items.join('\n'), text: text.slice(0, 6000), more: n >= MAX};
})()`

func (b *browser) snapshot() (pageState, error) {
	var st pageState
	err := b.run(10*time.Second, chromedp.Evaluate(snapshotJS, &st))
	return st, err
}

// elementInfo describes element n for confirmations: what it is and, for a form, what will be sent.
type elementInfo struct {
	Found  bool     `json:"found"`
	Risky  bool     `json:"risky"`
	Label  string   `json:"label"`
	Fields []string `json:"fields"`
}

const elementInfoJS = `((n) => {
  const el = document.querySelector('[data-mem="' + n + '"]');
  if (!el) return {found: false};
  const label = (el.getAttribute('aria-label') || el.innerText || el.value || el.getAttribute('placeholder') || '').replace(/\s+/g, ' ').trim().slice(0, 80);
  const fields = [];
  const form = el.form || el.closest('form');
  if (form) for (const f of form.elements) {
    if (!f.name && !f.id) continue;
    const t = (f.type || '').toLowerCase();
    if (['submit', 'button', 'hidden', 'image', 'reset'].includes(t)) continue;
    let name = (f.labels && f.labels.length ? f.labels[0].innerText : '') || f.getAttribute('aria-label') || f.getAttribute('placeholder') || f.name || f.id;
    name = name.replace(/\s+/g, ' ').trim().slice(0, 40);
    let v = f.value;
    if (t === 'password') v = f.value ? '••••••' : '';
    if (t === 'checkbox' || t === 'radio') { if (!f.checked) continue; }
    if (f.tagName === 'SELECT') v = f.selectedOptions[0] ? f.selectedOptions[0].text : '';
    if (v) fields.push(name + ': ' + v);
  }
  return {found: true, risky: el.getAttribute('data-mem-risky') === '1', label, fields};
})`

func (b *browser) element(n int) (elementInfo, error) {
	var info elementInfo
	err := b.run(5*time.Second, chromedp.Evaluate(fmt.Sprintf("%s(%d)", elementInfoJS, n), &info))
	return info, err
}

func (b *browser) url() string {
	var u string
	b.run(3*time.Second, chromedp.Location(&u))
	return u
}

func memSel(n int) string { return fmt.Sprintf(`[data-mem="%d"]`, n) }

func (b *browser) open(u string) error {
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	return b.run(30*time.Second, chromedp.Navigate(u))
}

func (b *browser) click(n int) error {
	sel := memSel(n)
	// Links that would open a new tab open in this one, so the agent keeps seeing them.
	b.run(3*time.Second, chromedp.Evaluate(fmt.Sprintf(`(() => { const e = document.querySelector('%s'); if (e) { e.removeAttribute('target'); e.scrollIntoView({block: 'center'}); } })()`, sel), nil))
	return b.run(10*time.Second, chromedp.Click(sel, chromedp.ByQuery, chromedp.NodeVisible))
}

// typeText replaces an input's value (or a contenteditable's text) by typing like a user.
func (b *browser) typeText(n int, text string) error {
	sel := memSel(n)
	clear := fmt.Sprintf(`(() => { const e = document.querySelector('%s'); if (!e) return; e.scrollIntoView({block: 'center'}); e.focus();
	  if (e.isContentEditable) { document.execCommand('selectAll'); document.execCommand('delete'); }
	  else if ('value' in e) { e.select && e.select(); } })()`, sel)
	return b.run(20*time.Second,
		chromedp.Evaluate(clear, nil),
		chromedp.KeyEvent("\b"), // deletes the selection, through the page's own handlers
		chromedp.SendKeys(sel, text, chromedp.ByQuery),
	)
}

func (b *browser) selectOption(n int, option string) error {
	js := fmt.Sprintf(`(() => { const e = document.querySelector('%s'); if (!e) return 'no such element';
	  const want = %q.toLowerCase(); const o = [...e.options].find(o => o.text.trim().toLowerCase() === want || o.value.toLowerCase() === want)
	    || [...e.options].find(o => o.text.toLowerCase().includes(want));
	  if (!o) return 'no option ' + %q;
	  e.value = o.value; e.dispatchEvent(new Event('input', {bubbles: true})); e.dispatchEvent(new Event('change', {bubbles: true})); return ''; })()`, memSel(n), option, option)
	var msg string
	if err := b.run(5*time.Second, chromedp.Evaluate(js, &msg)); err != nil {
		return err
	}
	if msg != "" {
		return fmt.Errorf("%s", msg)
	}
	return nil
}

var keyNames = map[string]string{"enter": "\r", "tab": "\t", "escape": "\x1b", "backspace": "\b", "arrowdown": "\ue015", "arrowup": "\ue013"}

func (b *browser) press(key string) error {
	k, ok := keyNames[strings.ToLower(key)]
	if !ok {
		return fmt.Errorf("unknown key %q (enter, tab, escape, backspace, arrowdown, arrowup)", key)
	}
	return b.run(5*time.Second, chromedp.KeyEvent(k))
}

func (b *browser) scroll(down bool) error {
	dir := "1"
	if !down {
		dir = "-1"
	}
	return b.run(5*time.Second, chromedp.Evaluate(`window.scrollBy(0, `+dir+` * window.innerHeight * 0.8)`, nil))
}

func (b *browser) back() error { return b.run(15*time.Second, chromedp.NavigateBack()) }

// activate brings the agent's tab to the front so you can watch (or step in).
func (b *browser) activate() {
	if t := chromedp.FromContext(b.ctx).Target; t != nil {
		b.run(3*time.Second, target.ActivateTarget(t.TargetID))
	}
}
