package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The extension drives your everyday Chrome for `mem do`: it opens a tab for the agent (in a "mem"
// tab group), attaches chrome.debugger to it and relays DevTools commands. All the logic stays in
// Go; the extension only forwards {method, params} and returns the result.
//
// The extension's background keeps one mem process running (op "bridge"). It serves:
//   - runs started in the browser (side panel): op "do", with the panel as the doUI;
//   - runs started in a terminal: `mem do` connects to ~/.mem/ext.sock and its commands are
//     relayed to the extension the same way, while confirmations stay in the terminal.

// extSocket is ~/.mem/ext.sock, or a short name in /tmp when that path is too long for a unix
// socket (104 bytes on macOS), e.g. with a deep MEM_HOME.
func extSocket() string {
	p := filepath.Join(home, "ext.sock")
	if len(p) < 100 {
		return p
	}
	sum := sha256.Sum256([]byte(home))
	return fmt.Sprintf("/tmp/mem-%d-%x.sock", os.Getuid(), sum[:6])
}

// ---------- inside the extension's mem (native host) ----------

type bridge struct {
	s       *server
	mu      sync.Mutex
	next    int
	pending map[int]chan request // cdp results and user replies, by cid
}

func (s *server) bridge() *bridge {
	s.bmu.Lock()
	defer s.bmu.Unlock()
	if s.br == nil {
		s.br = &bridge{s: s, pending: map[int]chan request{}}
	}
	return s.br
}

// await sends an event that expects a reply carrying the same cid, and waits for it.
func (br *bridge) await(id int, e event, timeout time.Duration) (request, error) {
	br.mu.Lock()
	br.next++
	cid := br.next
	ch := make(chan request, 1)
	br.pending[cid] = ch
	br.mu.Unlock()
	defer func() { br.mu.Lock(); delete(br.pending, cid); br.mu.Unlock() }()
	e["cid"] = cid
	br.s.send(id, e)
	select {
	case r := <-ch:
		return r, nil
	case <-time.After(timeout):
		return request{}, fmt.Errorf("no answer from the browser")
	}
}

func (br *bridge) reply(r request) {
	br.mu.Lock()
	ch := br.pending[r.N]
	br.mu.Unlock()
	if ch != nil {
		ch <- r
	}
}

// cdp relays one DevTools command to the extension. Tab-less "mem.*" methods manage the agent tab.
func (br *bridge) cdp(tab int, method string, params map[string]any) (json.RawMessage, error) {
	r, err := br.await(0, event{"ev": "cdp", "tab": tab, "method": method, "params": params}, 60*time.Second)
	if err != nil {
		return nil, err
	}
	if r.Error != "" {
		return nil, fmt.Errorf("%s", r.Error)
	}
	return r.Result, nil
}

// extCaller is a cdpCaller for one agent tab in the user's Chrome.
type extCaller struct {
	br  *bridge
	tab int
}

func (br *bridge) newTab() (*browser, error) {
	raw, err := br.cdp(0, "mem.newTab", nil)
	if err != nil {
		return nil, err
	}
	var t struct {
		TabID int `json:"tabId"`
	}
	if err := json.Unmarshal(raw, &t); err != nil || t.TabID == 0 {
		return nil, fmt.Errorf("the extension could not open a tab")
	}
	return &browser{cdp: &extCaller{br, t.TabID}}, nil
}

func (c *extCaller) call(method string, params map[string]any) (json.RawMessage, error) {
	return c.br.cdp(c.tab, method, params)
}

func (c *extCaller) close() { c.br.cdp(c.tab, "mem.close", nil) }

// panelUI is the side panel as the user's side of a run.
type panelUI struct {
	br *bridge
	id int // the "do" request: all events of the run carry it
}

func (u panelUI) progress(line string) { u.br.s.send(u.id, event{"ev": "do_step", "text": line}) }

func (u panelUI) ask(q string) string {
	r, err := u.br.await(u.id, event{"ev": "do_ask", "question": q}, 30*time.Minute)
	if err != nil {
		return ""
	}
	return r.Text
}

func (u panelUI) confirm(what, url string, fields []string) (bool, string) {
	r, err := u.br.await(u.id, event{"ev": "do_confirm", "what": what, "url": url, "fields": fields}, 30*time.Minute)
	if err != nil {
		return false, "no answer"
	}
	return r.OK, r.Text
}

// doInBrowser runs a task started from the side panel.
func (s *server) doInBrowser(r request) {
	br := s.bridge()
	b, err := br.newTab()
	if err != nil {
		s.fail(r.ID, err)
		return
	}
	defer b.close()
	summary, ok, note, _ := runDo(s.db, s.cfg, s.cfg.Model, strings.TrimSpace(r.Q), b, panelUI{br, r.ID})
	s.send(r.ID, event{"ev": "done", "answer": summary, "success": ok, "note": note})
}

// serveSocket lets `mem do` in a terminal use the extension: one connection per run, JSON lines
// {"method","params"} → {"result"} or {"error"}, on a socket only this user can open.
func (s *server) serveSocket() {
	br := s.bridge()
	path := extSocket()
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		warn("mem do from terminals won't use this browser: %v", err)
		return
	}
	os.Chmod(path, 0o600)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			tab := 0
			defer func() {
				if tab != 0 {
					br.cdp(tab, "mem.close", nil)
				}
			}()
			sc := bufio.NewScanner(conn)
			sc.Buffer(make([]byte, 1<<20), 16<<20)
			enc := json.NewEncoder(conn)
			for sc.Scan() {
				var m struct {
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				json.Unmarshal(sc.Bytes(), &m)
				if m.Method == "mem.newTab" {
					b, err := br.newTab()
					if err != nil {
						enc.Encode(map[string]any{"error": err.Error()})
						continue
					}
					tab = b.cdp.(*extCaller).tab
					enc.Encode(map[string]any{"result": map[string]any{"tabId": tab}})
					continue
				}
				res, err := br.cdp(tab, m.Method, m.Params)
				if err != nil {
					enc.Encode(map[string]any{"error": err.Error()})
				} else {
					enc.Encode(map[string]any{"result": res})
				}
			}
		}()
	}
}

// ---------- in `mem do` (terminal) ----------

type socketCaller struct {
	conn net.Conn
	sc   *bufio.Scanner
}

// dialExtension connects to the extension's mem, which opens a tab for this run.
func dialExtension() (*browser, error) {
	conn, err := net.DialTimeout("unix", extSocket(), time.Second)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	c := &socketCaller{conn, sc}
	if _, err := c.call("mem.newTab", nil); err != nil {
		conn.Close()
		return nil, err
	}
	return &browser{cdp: c}, nil
}

func (c *socketCaller) call(method string, params map[string]any) (json.RawMessage, error) {
	if err := json.NewEncoder(c.conn).Encode(map[string]any{"method": method, "params": params}); err != nil {
		return nil, err
	}
	c.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	if !c.sc.Scan() {
		return nil, fmt.Errorf("lost the connection to Chrome")
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(c.sc.Bytes(), &r); err != nil {
		return nil, err
	}
	if r.Error != "" {
		return nil, fmt.Errorf("%s", r.Error)
	}
	return r.Result, nil
}

func (c *socketCaller) close() { c.conn.Close() } // the extension's mem detaches from the tab
