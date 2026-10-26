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

// cdpCaller sends one DevTools command to the agent's tab and returns the raw result. Two kinds:
// mem's own Chrome over a websocket (openBrowser), or your everyday Chrome through the extension,
// which relays commands to a tab it opens for the agent (extBrowser in extbridge.go).
type cdpCaller interface {
	call(method string, params map[string]any) (json.RawMessage, error)
	close()
}

// browser does page-level actions on top of raw DevTools commands, so both kinds behave the same.
type browser struct {
	cdp cdpCaller
}

// ---------- mem's own Chrome ----------

type chromedpCaller struct {
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
	return &browser{cdp: &chromedpCaller{ctx, func() { cancel(); acancel() }}}, nil
}

func (c *chromedpCaller) call(method string, params map[string]any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	var res map[string]any
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return chromedp.FromContext(ctx).Target.Execute(ctx, method, params, &res)
	}))
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

func (c *chromedpCaller) close() { c.cancel() }

// ---------- page actions ----------

func (b *browser) close() { b.cdp.close() }

// eval runs JavaScript in the page and decodes its (JSON-able) value into out.
func (b *browser) eval(js string, out any) error {
	raw, err := b.cdp.call("Runtime.evaluate", map[string]any{"expression": js, "returnByValue": true, "awaitPromise": true})
	if err != nil {
		return err
	}
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	if r.Exception != nil {
		return fmt.Errorf("page script failed: %s %s", r.Exception.Text, clip(r.Exception.Exception.Description, 200))
	}
	if out == nil || len(r.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(r.Result.Value, out)
}

// settle waits for the page to finish loading after an action (bounded, never fails).
func (b *browser) settle() {
	time.Sleep(400 * time.Millisecond)
	for i := 0; i < 20; i++ {
		var state string
		if b.eval(`document.readyState`, &state) == nil && state == "complete" {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
}

func (b *browser) snapshot() (pageState, error) {
	var st pageState
	err := b.eval(snapshotJS, &st)
	return st, err
}

func (b *browser) element(n int) (elementInfo, error) {
	var info elementInfo
	err := b.eval(fmt.Sprintf("%s(%d)", elementInfoJS, n), &info)
	return info, err
}

func (b *browser) url() string {
	var u string
	b.eval(`location.href`, &u)
	return u
}

func memSel(n int) string { return fmt.Sprintf(`[data-mem="%d"]`, n) }

func (b *browser) open(u string) error {
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	raw, err := b.cdp.call("Page.navigate", map[string]any{"url": u})
	if err != nil {
		return err
	}
	var r struct {
		ErrorText string `json:"errorText"`
	}
	json.Unmarshal(raw, &r)
	if r.ErrorText != "" {
		return fmt.Errorf("%s", r.ErrorText)
	}
	return nil
}

// click scrolls element n into view and clicks its center with real mouse events.
func (b *browser) click(n int) error {
	var pt struct {
		Found bool
		X, Y  float64
	}
	// Links that would open a new tab open in this one, so the agent keeps seeing them.
	js := fmt.Sprintf(`(() => { const e = document.querySelector('%s'); if (!e) return {Found: false};
	  e.removeAttribute('target'); e.scrollIntoView({block: 'center', inline: 'center'});
	  const r = e.getBoundingClientRect(); return {Found: true, X: r.left + r.width / 2, Y: r.top + r.height / 2}; })()`, memSel(n))
	if err := b.eval(js, &pt); err != nil {
		return err
	}
	if !pt.Found {
		return fmt.Errorf("element [%d] is gone; use the latest page", n)
	}
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		if _, err := b.cdp.call("Input.dispatchMouseEvent", map[string]any{
			"type": typ, "x": pt.X, "y": pt.Y, "button": "left", "clickCount": 1,
		}); err != nil {
			return err
		}
	}
	return nil
}

// typeText replaces the text of an input, textarea or editable element, then types like a user.
func (b *browser) typeText(n int, text string) error {
	var ok bool
	js := fmt.Sprintf(`(() => { const e = document.querySelector('%s'); if (!e) return false;
	  e.scrollIntoView({block: 'center'}); e.focus();
	  if (e.isContentEditable) { document.execCommand('selectAll'); document.execCommand('delete'); }
	  else if ('value' in e) {
	    const proto = e.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
	    Object.getOwnPropertyDescriptor(proto, 'value').set.call(e, '');  // works with React too
	    e.dispatchEvent(new Event('input', {bubbles: true}));
	  }
	  return true; })()`, memSel(n))
	if err := b.eval(js, &ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("element [%d] is gone; use the latest page", n)
	}
	_, err := b.cdp.call("Input.insertText", map[string]any{"text": text})
	return err
}

func (b *browser) selectOption(n int, option string) error {
	js := fmt.Sprintf(`(() => { const e = document.querySelector('%s'); if (!e) return 'no such element';
	  const want = %q.toLowerCase(); const o = [...e.options].find(o => o.text.trim().toLowerCase() === want || o.value.toLowerCase() === want)
	    || [...e.options].find(o => o.text.toLowerCase().includes(want));
	  if (!o) return 'no option ' + %q;
	  e.value = o.value; e.dispatchEvent(new Event('input', {bubbles: true})); e.dispatchEvent(new Event('change', {bubbles: true})); return ''; })()`, memSel(n), option, option)
	var msg string
	if err := b.eval(js, &msg); err != nil {
		return err
	}
	if msg != "" {
		return fmt.Errorf("%s", msg)
	}
	return nil
}

var keys = map[string]struct {
	key, code, text string
	vk              int
}{
	"enter": {"Enter", "Enter", "\r", 13}, "tab": {"Tab", "Tab", "", 9}, "escape": {"Escape", "Escape", "", 27},
	"backspace": {"Backspace", "Backspace", "", 8}, "arrowdown": {"ArrowDown", "ArrowDown", "", 40}, "arrowup": {"ArrowUp", "ArrowUp", "", 38},
}

func (b *browser) press(name string) error {
	k, ok := keys[strings.ToLower(name)]
	if !ok {
		return fmt.Errorf("unknown key %q (enter, tab, escape, backspace, arrowdown, arrowup)", name)
	}
	down := map[string]any{"type": "rawKeyDown", "key": k.key, "code": k.code, "windowsVirtualKeyCode": k.vk}
	if k.text != "" {
		down = map[string]any{"type": "keyDown", "key": k.key, "code": k.code, "windowsVirtualKeyCode": k.vk, "text": k.text}
	}
	if _, err := b.cdp.call("Input.dispatchKeyEvent", down); err != nil {
		return err
	}
	_, err := b.cdp.call("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": k.key, "code": k.code, "windowsVirtualKeyCode": k.vk})
	return err
}

func (b *browser) scroll(down bool) error {
	dir := "1"
	if !down {
		dir = "-1"
	}
	return b.eval(`window.scrollBy(0, `+dir+` * window.innerHeight * 0.8)`, nil)
}

func (b *browser) back() error { return b.eval(`history.back()`, nil) }

// activate brings the agent's tab to the front so you can watch (or step in).
func (b *browser) activate() { b.cdp.call("Page.bringToFront", nil) }

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
