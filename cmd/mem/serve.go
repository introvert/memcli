package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// `mem serve` speaks JSON lines on stdin/stdout for GUI front ends (the macOS menu bar app).
// One request per line, {"id":1,"op":"search","q":"..."}; every reply carries the request id.
// The process stays up, so the index is open, the API connection warm and an ask session (with
// its secret map) lives across follow-ups. Secrets never leave this process except into the
// clipboard (auto-cleared) or a command the user confirmed.

type request struct {
	ID     int    `json:"id"`
	Op     string `json:"op"` // search, add, ask, cancel, reset, item, copy_note, copy_text; extension: bridge, do, cdp_result, do_reply
	Q      string `json:"q,omitempty"`
	Text   string `json:"text,omitempty"`
	Deep   bool   `json:"deep,omitempty"`
	New    bool   `json:"new,omitempty"` // ask: start a new conversation
	N      int    `json:"n,omitempty"`
	Action string `json:"action,omitempty"` // item: copy | run
	Limit  int    `json:"limit,omitempty"`
	// replies from the extension: n is the cid of the event answered
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	OK     bool            `json:"ok,omitempty"`
}

type event map[string]any

type uiNote struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Created string   `json:"created"`
	Tags    []string `json:"tags"`
	Snippet string   `json:"snippet,omitempty"`
	Body    string   `json:"body"` // redacted: the panel can be seen over your shoulder
	Path    string   `json:"path"`
}

type uiItem struct {
	N     int    `json:"n"`
	Kind  string `json:"kind"`  // value, secret, cmd
	Label string `json:"label"` // safe to display: secrets masked
}

type server struct {
	db       *sql.DB
	cfg      Config
	write    func([]byte) // one encoded event to the client
	wmu      sync.Mutex   // serializes writes
	allowRun bool         // whether "item run" may run commands (not for the browser extension)
	bmu      sync.Mutex
	br       *bridge    // the extension's browser relay (native host only)
	imu      sync.Mutex // serializes index syncs

	run    sync.Mutex // one ask at a time
	amu    sync.Mutex // guards the fields below
	sess   *session
	items  []item
	cancel context.CancelFunc
}

func cmdServe([]string) {
	s := newServer(func(b []byte) { os.Stdout.Write(append(b, '\n')) })
	s.allowRun = true
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		s.dispatch(sc.Bytes())
	}
}

func newServer(write func([]byte)) *server {
	color = false
	cfg := loadConfig()
	db, err := openIndex()
	check(err)
	s := &server{db: db, cfg: cfg, write: write}
	warmUp(cfg, cfg.Model)
	s.send(0, event{"ev": "ready", "version": version, "notes_dir": notesD, "model": cfg.Model})
	return s
}

func (s *server) dispatch(line []byte) {
	var r request
	if err := json.Unmarshal(line, &r); err != nil {
		s.send(0, event{"ev": "error", "text": "bad request: " + err.Error()})
		return
	}
	if r.Op == "cancel" || r.Op == "reset" {
		s.handle(r) // in order: must not overtake the request sent after it
	} else {
		go s.handle(r)
	}
}

func (s *server) send(id int, e event) {
	e["id"] = id
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(e)
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.write(bytes.TrimRight(buf.Bytes(), "\n"))
}

func (s *server) fail(id int, err error) { s.send(id, event{"ev": "error", "text": err.Error()}) }

func (s *server) sync() {
	s.imu.Lock()
	defer s.imu.Unlock()
	syncIndex(s.db)
}

func (s *server) handle(r request) {
	switch r.Op {
	case "search":
		s.search(r)
	case "add":
		s.add(r)
	case "ask":
		s.ask(r)
	case "cancel":
		s.amu.Lock()
		if s.cancel != nil {
			s.cancel()
		}
		s.amu.Unlock()
		s.send(r.ID, event{"ev": "ok"})
	case "reset":
		s.amu.Lock()
		if s.cancel != nil {
			s.cancel()
		}
		s.sess, s.items = nil, nil
		s.amu.Unlock()
		s.send(r.ID, event{"ev": "ok"})
	case "item":
		s.item(r)
	case "copy_note":
		s.copyNote(r)
	case "bridge": // the extension's background: relay browser commands, serve `mem do` in terminals
		go s.serveSocket()
		s.send(r.ID, event{"ev": "ok"})
	case "do":
		s.doInBrowser(r)
	case "cdp_result", "do_reply":
		s.bridge().reply(r)
	case "copy_text":
		s.report(r.ID, copyText(r.Text, false), "copied")
	default:
		s.send(r.ID, event{"ev": "error", "text": "unknown op " + r.Op})
	}
}

// search: ranked results (with the same typo fallback as `mem s`), or recent notes for an empty query.
func (s *server) search(r request) {
	s.sync()
	limit := clamp(r.Limit, 30, 100)
	var hits []Hit
	var err error
	corrected := false
	if q := ftsQuery(r.Q, "AND"); q == "" {
		hits, err = recent(s.db, "", limit, false)
	} else {
		hits, err = search(s.db, q, limit, "", "", false)
		if err == nil && len(hits) == 0 {
			if fixed, ok := correct(s.db, r.Q); ok {
				hits, err = search(s.db, ftsQuery(fixed, "OR"), limit, "", "", false)
				corrected = len(hits) > 0
			}
		}
	}
	if err != nil {
		s.fail(r.ID, err)
		return
	}
	notes := make([]uiNote, 0, len(hits))
	for _, h := range hits {
		notes = append(notes, uiNote{h.ID, h.Title, h.Created, h.Tags, strings.ReplaceAll(h.Snippet, "\n", " "), clip(h.Body, 8000), notePath(h.ID)})
	}
	s.send(r.ID, event{"ev": "notes", "notes": notes, "corrected": corrected})
}

func (s *server) add(r request) {
	body := strings.TrimSpace(r.Text)
	if body == "" {
		s.send(r.ID, event{"ev": "error", "text": "empty note"})
		return
	}
	id, err := writeNote(body, nil)
	if err != nil {
		s.fail(r.ID, err)
		return
	}
	if s.cfg.AutoTag {
		if tags, err := suggestTags(s.cfg, s.cfg.FastModel, body); err == nil {
			addTags(id, tags)
		}
	}
	gitCommit("add " + id)
	s.sync()
	s.send(r.ID, event{"ev": "saved", "note": id})
}

// ask streams status lines and answer text, then the result with the actions it offers.
// A follow-up continues the current session; "reset" starts a new one.
func (s *server) ask(r request) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s.amu.Lock()
	if s.cancel != nil {
		s.cancel() // a new question replaces one still running
	}
	s.cancel = cancel
	if r.New {
		s.sess, s.items = nil, nil
	}
	if s.sess == nil { // -d only picks the level of a new conversation, like `mem ask -d`
		level := Normal
		if r.Deep {
			level = Deep
		}
		s.sync()
		s.sess = newSession(s.db, s.cfg, s.cfg.Model, level, "", nil)
	}
	sess := s.sess
	s.amu.Unlock()
	s.run.Lock() // a replaced question finishes cancelling before the session is reused
	defer s.run.Unlock()
	if ctx.Err() != nil {
		s.fail(r.ID, fmt.Errorf("cancelled"))
		return
	}

	sess.progress = writerFunc(func(p []byte) (int, error) {
		if line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(p)), "·")); line != "" {
			s.send(r.ID, event{"ev": "status", "text": line})
		}
		return len(p), nil
	})
	text := writerFunc(func(p []byte) (int, error) {
		s.send(r.ID, event{"ev": "text", "text": string(p)})
		return len(p), nil
	})
	res, err := sess.ask(ctx, r.Q, text)
	if err != nil {
		if ctx.Err() == context.Canceled {
			err = fmt.Errorf("cancelled")
		}
		s.fail(r.ID, err)
		return
	}
	items := extractItems(res.Answer)
	ui := make([]uiItem, len(items))
	for i, it := range items {
		label := masked(it.text)
		if it.kind == "secret" {
			_, note, _ := sess.secret(secretNum(it.text))
			label = placeholderRe.FindStringSubmatch(it.text)[1] + " from " + note
		}
		ui[i] = uiItem{i + 1, it.kind, label}
	}
	s.amu.Lock()
	if s.sess == sess {
		s.items = items
	}
	s.amu.Unlock()
	sources := make([]uiNote, len(res.Sources))
	for i, n := range res.Sources {
		sources[i] = uiNote{ID: n.ID, Title: n.Title, Created: n.Created, Tags: n.Tags, Path: n.Path}
	}
	s.send(r.ID, event{"ev": "done", "answer": res.Answer, "sources": sources, "items": ui, "steps": res.Steps})
}

// item copies or runs action n of the last answer, filling in secrets locally.
func (s *server) item(r request) {
	s.amu.Lock()
	sess, items := s.sess, s.items
	s.amu.Unlock()
	if sess == nil || r.N < 1 || r.N > len(items) {
		s.send(r.ID, event{"ev": "error", "text": "no such item"})
		return
	}
	it := items[r.N-1]
	switch {
	case it.kind == "value":
		s.report(r.ID, copyText(it.text, false), "copied")
	case it.kind == "secret":
		v, note, err := sess.secret(secretNum(it.text))
		if err != nil {
			s.fail(r.ID, err)
			return
		}
		s.report(r.ID, copyText(v, true), fmt.Sprintf("copied secret from %s · clears in %ds", note, int(clipClearAfter.Seconds())))
	case r.Action == "run" && !s.allowRun:
		s.send(r.ID, event{"ev": "error", "text": "running commands is not available here"})
	case r.Action == "run":
		cmd, _, err := sess.fill(it.text)
		if err != nil {
			s.fail(r.ID, err)
			return
		}
		out, code := runCaptured(cmd)
		// Output goes back through the redactor, and the secrets just filled in are masked, so a
		// command that echoes a secret doesn't put it on screen.
		for _, m := range placeholderRe.FindAllStringSubmatch(it.text, -1) {
			if v, _, err := sess.secret(secretNum(m[0])); err == nil && v != "" {
				out = strings.ReplaceAll(out, v, "‹"+m[1]+"›")
			}
		}
		red, _ := redact(out, compileExtra(s.cfg.RedactPatterns))
		s.send(r.ID, event{"ev": "output", "text": red, "code": code})
	default: // copy a command
		cmd, hasSecret, err := sess.fill(it.text)
		if err != nil {
			s.fail(r.ID, err)
			return
		}
		msg := "copied command"
		if hasSecret {
			msg += fmt.Sprintf(" (contains a secret · clears in %ds)", int(clipClearAfter.Seconds()))
		}
		s.report(r.ID, copyText(cmd, hasSecret), msg)
	}
}

// copyNote copies a note's real text (secrets included, so the clipboard is cleared afterwards).
func (s *server) copyNote(r request) {
	path, err := resolve(s.db, r.Q)
	if err != nil {
		s.fail(r.ID, err)
		return
	}
	n, err := parseNote(path)
	if err != nil {
		s.fail(r.ID, err)
		return
	}
	_, found := redact(n.Body, compileExtra(s.cfg.RedactPatterns))
	msg := "copied " + n.ID
	if len(found) > 0 {
		msg += fmt.Sprintf(" (has secrets · clears in %ds)", int(clipClearAfter.Seconds()))
	}
	s.report(r.ID, copyText(strings.TrimSpace(n.Body), len(found) > 0), msg)
}

func (s *server) report(id int, err error, ok string) {
	if err != nil {
		s.fail(id, err)
	} else {
		s.send(id, event{"ev": "ok", "text": ok})
	}
}

// runCaptured runs a confirmed command in the user's shell (no terminal), returning its output.
func runCaptured(cmd string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "cmd", "/C", cmd)
	} else {
		sh := os.Getenv("SHELL")
		if sh == "" {
			sh = "sh"
		}
		c = exec.CommandContext(ctx, sh, "-c", cmd)
	}
	c.Dir, _ = os.UserHomeDir()
	c.Stdin = strings.NewReader("")
	var buf limitedBuffer
	c.Stdout, c.Stderr = &buf, &buf
	err := c.Run()
	code := 0
	if err != nil {
		code = -1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		if buf.Len() == 0 {
			buf.WriteString(err.Error())
		}
	}
	return buf.String(), code
}

// limitedBuffer keeps the first 64 KB of output.
type limitedBuffer struct{ strings.Builder }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := 64<<10 - b.Len(); room > 0 {
		b.Builder.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

var _ io.Writer = (*limitedBuffer)(nil)
