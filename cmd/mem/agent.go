package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// `mem ask` runs a small agent: the model explores the notes with search/grep/list/read tools
// and answers with citations. Every tool reads the index, whose note text is already redacted,
// and skips private notes, so secrets and excluded notes never reach the model.

const agentSystem = `You are mem, the user's personal-notes assistant. You answer questions from the user's
notes, which you can only see through tools:
- search: ranked full-text search (stemmed, prefix matching).
- grep: exact text or regex over all notes; returns matching lines. Use for names, numbers, code, exact phrases.
- list: browse folders, tags and the most recent notes.
- read: read a note with line numbers.

How to work:
- Speed matters. Each user message already contains search results for it, with the best notes in
  full: if they answer the question, answer right away without calling tools. Otherwise make several
  independent lookups in the same turn (try synonyms, related terms, other languages the notes may be
  written in), read the promising notes, and stop as soon as you can answer.
- Don't write any text in a turn where you call tools; write only the final answer.

Answer in the language of the question: just the answer, as short as it can be, for a terminal (no
markdown headings or tables). Don't remark on what the notes don't say unless they lack the answer.
After each fact, cite the note id in square brackets exactly as the tools show it, e.g. [mem/261025-143012]
or [Projects/ideas]. If the notes don't contain the answer, say so plainly instead of guessing.
Put shell commands the user may run in a fenced block (` + "```sh" + `), one command per block, and values they
may want to copy (hosts, IPs, IDs, URLs, names) in single backticks. The user can copy or run these.

Privacy: secrets in notes are replaced by placeholders like [REDACTED:password#3] before you see them.
Never guess or reconstruct a secret. When the user needs one, write its placeholder exactly as shown,
also inside commands (e.g. psql "postgres://app:[REDACTED:password#3]@db/app"): the user's machine fills
in the real value locally when they copy or run it, without it ever being shown to you.`

var agentTools = []Tool{
	{"search", "Ranked full-text search over all notes. Returns note ids, titles, tags, dates and a snippet.",
		obj(props{"query": str("Words to search for"), "tag": str("Only notes with this tag (optional)"),
			"limit": integer("Max results, default 10, max 30")}, "query")},
	{"grep", "Find lines containing exact text (or a regex) across all notes. Returns id:line: text.",
		obj(props{"pattern": str("Text to find (case-insensitive) or a regex when regex=true"),
			"regex":  boolean("Treat pattern as a regular expression (RE2 syntax)"),
			"folder": str("Only notes under this folder, e.g. Projects/2026 (optional)")}, "pattern")},
	{"list", "Browse notes: subfolders and notes in a folder, notes with a tag, or the most recent notes.",
		obj(props{"folder": str("Folder to list, '' for the top level"), "tag": str("List notes with this tag"),
			"limit": integer("Max notes, default 30")})},
	{"read", "Read a note with line numbers. Long notes come in pages; use offset to continue.",
		obj(props{"id": str("Note id as shown by other tools (a file name prefix also works)"),
			"offset": integer("First line to return, default 1"), "limit": integer("Lines to return, default 300")}, "id")},
}

type askResult struct {
	Answer  string   `json:"answer"`
	Sources []Note   `json:"sources"`
	Steps   []string `json:"steps"`
}

// secretRef points a session placeholder back to the nth secret of a note.
type secretRef struct {
	Note string
	N    int
}

// session is one `mem ask` conversation, follow-ups included.
type session struct {
	db       *sql.DB
	cfg      Config
	spec     string
	level    Level
	tag      string
	chat     Chat
	progress io.Writer
	// Placeholders are numbered per note in the index ([REDACTED:password#2] = 2nd secret of that
	// note). Before text reaches the model they are renumbered session-wide, so every placeholder
	// the model sees maps to exactly one (note, n) — that's how copy/run fill in real values locally.
	refs    []secretRef
	refByID map[secretRef]int
}

func newSession(db *sql.DB, cfg Config, spec string, level Level, tag string, progress io.Writer) *session {
	return &session{db: db, cfg: cfg, spec: spec, level: level, tag: tag, progress: progress, refByID: map[secretRef]int{}}
}

// expose renumbers a note's placeholders to session-wide ones.
func (s *session) expose(noteID, text string) string {
	return placeholderRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := placeholderRe.FindStringSubmatch(m)
		n, _ := strconv.Atoi(sub[2])
		ref := secretRef{noteID, n}
		k, ok := s.refByID[ref]
		if !ok {
			s.refs = append(s.refs, ref)
			k = len(s.refs)
			s.refByID[ref] = k
		}
		return fmt.Sprintf("[REDACTED:%s#%d]", sub[1], k)
	})
}

// secret returns the real value behind session placeholder #k, read from the note on disk.
func (s *session) secret(k int) (value, noteID string, err error) {
	if k < 1 || k > len(s.refs) {
		return "", "", fmt.Errorf("unknown placeholder #%d", k)
	}
	ref := s.refs[k-1]
	n, err := parseNote(notePath(ref.Note))
	if err != nil {
		return "", ref.Note, err
	}
	_, found := redact(n.Body, compileExtra(s.cfg.RedactPatterns))
	if ref.N > len(found) {
		return "", ref.Note, fmt.Errorf("note %s changed since it was read", ref.Note)
	}
	return found[ref.N-1].Value, ref.Note, nil
}

// ask sends a question (the first one or a follow-up) and runs the tool loop until the model answers.
func (s *session) ask(ctx context.Context, question string, out io.Writer) (askResult, error) {
	user := fmt.Sprintf("Question: %s", question)
	if s.chat == nil {
		user = fmt.Sprintf("Today is %s.\n\n%s", time.Now().Format("2006-01-02 (Monday)"), user)
		if s.tag != "" {
			user += fmt.Sprintf("\n\n(The user is asking about notes tagged #%s.)", s.tag)
		}
	}
	// Run the obvious search locally (milliseconds) and hand over the results up front, so most
	// questions are answered in a single model call instead of search → read → answer round trips.
	st := newStatus(s.progress, out)
	st.set("searching notes…")
	pre, hits, fixed := s.prefetch(question)
	user += "\n\n" + pre
	note := ""
	if fixed {
		note = " (corrected typos)"
	}
	_, model, _ := s.cfg.resolveModel(s.spec)
	if len(hits) == 0 {
		st.set("no direct matches" + note + " · asking " + model + "…")
	} else {
		st.set(fmt.Sprintf("%d notes matched%s · asking %s…", len(hits), note, model))
	}
	defer st.clear()
	rw := newRender(st.wrap(out))
	flush := func() {
		if r, ok := rw.(*render); ok {
			r.Flush()
		}
	}
	if s.chat == nil {
		chat, err := newChat(s.cfg, s.spec, agentSystem, user, agentTools, s.level)
		if err != nil {
			return askResult{}, err
		}
		s.chat = chat
	} else {
		s.chat.AddUser(user)
	}
	res := askResult{Steps: []string{}}
	for step := 0; ; step++ {
		last := step >= s.cfg.MaxSteps // out of budget: answer with what we have
		text, calls, err := s.chat.Next(ctx, !last, rw)
		flush()
		if err != nil {
			return res, err
		}
		if len(calls) == 0 || last {
			res.Answer = strings.TrimSpace(text)
			break
		}
		results := make([]string, len(calls))
		for i, call := range calls {
			o, summary := s.runTool(call)
			results[i] = o
			line := call.Name + " " + summary
			res.Steps = append(res.Steps, line)
			st.set(line)
		}
		s.chat.AddResults(calls, results)
	}
	res.Sources = cited(res.Answer, s.db)
	return res, nil
}

// prefetch searches for the question's words and returns a results list plus the full (redacted)
// text of the top notes, ready to put in the message.
func (s *session) prefetch(question string) (string, []Hit, bool) {
	const list, full, perNote = 8, 3, 6000
	words, fixed := correct(s.db, stripStopwords(question))
	q := ftsQuery(words, "OR")
	if q == "" {
		return "Search results for it: none (no searchable words).", nil, false
	}
	if s.tag != "" {
		q = "(" + q + ") AND " + tagQuery(s.tag)
	}
	hits, err := search(s.db, q, list, "", "", true)
	if err != nil || len(hits) == 0 {
		return "Search results for it: no matches. Try other words with the tools.", nil, fixed
	}
	var b strings.Builder
	b.WriteString("Search results for it (best first):\n")
	for _, h := range hits {
		fmt.Fprintf(&b, "%s | %s | %s | %s\n  %s\n", h.ID, day(h.Created), s.expose(h.ID, h.Title), hashTags(h.Tags),
			s.expose(h.ID, strings.ReplaceAll(h.Snippet, "\n", " ")))
	}
	b.WriteString("\nFull text of the top results:\n")
	for i, h := range hits {
		if i == full {
			break
		}
		fmt.Fprintf(&b, "<note id=%q created=%q>\n%s\n</note>\n", h.ID, day(h.Created), s.expose(h.ID, clip(h.Body, perNote)))
	}
	return b.String(), hits, fixed
}

// runTool executes one tool call. Errors go back to the model as text so it can correct itself.
func (s *session) runTool(call ToolCall) (out, summary string) {
	var a struct {
		Query, Tag, Pattern, Folder, ID string
		Regex                           bool
		Limit, Offset                   int
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(call.Args)), &a); err != nil {
		return "error: arguments are not valid JSON: " + err.Error(), "(bad arguments)"
	}
	switch call.Name {
	case "search":
		return s.toolSearch(a.Query, a.Tag, clamp(a.Limit, 10, 30))
	case "grep":
		return s.toolGrep(a.Pattern, a.Regex, a.Folder)
	case "list":
		return s.toolList(a.Folder, a.Tag, clamp(a.Limit, 30, 100))
	case "read":
		return s.toolRead(a.ID, max(a.Offset, 1), clamp(a.Limit, 300, 1000))
	}
	return "error: unknown tool " + call.Name, "(unknown tool)"
}

func (s *session) toolSearch(query, tag string, limit int) (string, string) {
	sum := fmt.Sprintf("%q", query)
	run := func(op string) ([]Hit, error) {
		q := ftsQuery(query, op)
		if q == "" {
			return nil, nil
		}
		if tag != "" {
			q = "(" + q + ") AND " + tagQuery(tag)
		}
		return search(s.db, q, limit, "", "", true)
	}
	hits, err := run("AND")
	if err == nil && len(hits) == 0 { // nothing has every word: rank by any of them
		hits, err = run("OR")
	}
	if err == nil && len(hits) == 0 { // maybe a typo: retry with the closest real words
		if fixedQ, ok := correct(s.db, query); ok {
			query = fixedQ
			hits, err = run("OR")
			sum += " (corrected typos)"
		}
	}
	if err != nil {
		return "error: " + err.Error(), sum
	}
	if len(hits) == 0 {
		return "no matches", sum + " → 0"
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "%s | %s | %s | %s\n  %s\n", h.ID, day(h.Created), s.expose(h.ID, h.Title), hashTags(h.Tags),
			s.expose(h.ID, strings.ReplaceAll(h.Snippet, "\n", " ")))
	}
	return b.String(), fmt.Sprintf("%s → %d", sum, len(hits))
}

func (s *session) toolGrep(pattern string, isRegex bool, folder string) (string, string) {
	sum := fmt.Sprintf("%q", pattern)
	if pattern == "" {
		return "error: empty pattern", sum
	}
	expr := "(?i)" + regexp.QuoteMeta(pattern)
	if isRegex {
		expr = "(?i)" + pattern
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "error: bad regex: " + err.Error(), sum
	}
	hits, _, err := allNotes(s.db, "", true)
	if err != nil {
		return "error: " + err.Error(), sum
	}
	const maxLines = 60
	var b strings.Builder
	n, notes := 0, 0
	for _, h := range hits {
		if folder != "" && !strings.HasPrefix(h.ID, strings.Trim(folder, "/")+"/") {
			continue
		}
		matched := false
		for i, line := range strings.Split(h.Body, "\n") {
			if !re.MatchString(line) {
				continue
			}
			if n++; n <= maxLines {
				fmt.Fprintf(&b, "%s:%d: %s\n", h.ID, i+1, s.expose(h.ID, clip(strings.TrimSpace(line), 240)))
			}
			matched = true
		}
		if matched {
			notes++
		}
	}
	if n == 0 {
		return "no matches", sum + " → 0"
	}
	if n > maxLines {
		fmt.Fprintf(&b, "… %d more matching lines; narrow the pattern or add a folder\n", n-maxLines)
	}
	return b.String(), fmt.Sprintf("%s → %d lines in %d notes", sum, n, notes)
}

func (s *session) toolList(folder, tag string, limit int) (string, string) {
	if tag != "" {
		hits, err := recent(s.db, tag, limit, true)
		if err != nil {
			return "error: " + err.Error(), "#" + tag
		}
		return s.noteLines(hits), fmt.Sprintf("#%s → %d", tag, len(hits))
	}
	all, _, err := allNotes(s.db, "", true)
	if err != nil {
		return "error: " + err.Error(), folder
	}
	folder = strings.Trim(folder, "/")
	if folder == "" {
		// Top level: folder overview + most recent notes anywhere.
		counts := map[string]int{}
		for _, h := range all {
			if top, _, ok := strings.Cut(h.ID, "/"); ok {
				counts[top]++
			}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d notes in total.\nFolders:\n", len(all))
		for _, f := range sortedByCount(counts) {
			fmt.Fprintf(&b, "  %s/ (%d notes)\n", f, counts[f])
		}
		hits, _ := recent(s.db, "", min(limit, 15), true)
		b.WriteString("Most recent notes:\n" + s.noteLines(hits))
		return b.String(), "(top level)"
	}
	sub := map[string]int{}
	var direct []Hit
	for _, h := range all {
		rest, ok := strings.CutPrefix(h.ID, folder+"/")
		if !ok {
			continue
		}
		if d, _, nested := strings.Cut(rest, "/"); nested {
			sub[d]++
		} else {
			direct = append(direct, h)
		}
	}
	if len(sub) == 0 && len(direct) == 0 {
		return "no such folder: " + folder, folder + " → 0"
	}
	sort.Slice(direct, func(i, j int) bool { return direct[i].Created > direct[j].Created })
	var b strings.Builder
	for _, f := range sortedByCount(sub) {
		fmt.Fprintf(&b, "  %s/ (%d notes)\n", path.Join(folder, f), sub[f])
	}
	if len(direct) > limit {
		fmt.Fprintf(&b, "(%d notes, showing the %d most recent)\n", len(direct), limit)
		direct = direct[:limit]
	}
	b.WriteString(s.noteLines(direct))
	return b.String(), fmt.Sprintf("%s → %d notes, %d folders", folder, len(direct), len(sub))
}

func (s *session) toolRead(id string, offset, limit int) (string, string) {
	p, err := resolve(s.db, id)
	if err == nil && isPrivate(s.db, noteID(p)) {
		err = fmt.Errorf("no note matching '%s'", id) // private notes don't exist for the model
	}
	if err != nil {
		return "error: " + err.Error(), id
	}
	nid := noteID(p)
	var created, tags, body string
	if err := s.db.QueryRow("SELECT created, tags, body FROM fts WHERE id=?", nid).Scan(&created, &tags, &body); err != nil {
		return "error: " + err.Error(), nid
	}
	lines := strings.Split(body, "\n")
	if offset > len(lines) {
		return fmt.Sprintf("note %s has only %d lines", nid, len(lines)), nid
	}
	end := min(offset-1+limit, len(lines))
	var b strings.Builder
	fmt.Fprintf(&b, "id: %s\ncreated: %s\ntags: %s\nlines %d-%d of %d\n---\n", nid, created, hashTags(strings.Fields(tags)), offset, end, len(lines))
	for i := offset - 1; i < end; i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, s.expose(nid, clip(lines[i], 2000)))
		if b.Len() > 40_000 {
			fmt.Fprintf(&b, "… stopped at line %d (size limit); continue with offset=%d\n", i+1, i+2)
			break
		}
	}
	return b.String(), nid
}

func (s *session) noteLines(hits []Hit) string {
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "  %s | %s | %s | %s\n", h.ID, day(h.Created), s.expose(h.ID, h.Title), hashTags(h.Tags))
	}
	return b.String()
}

// warmUp opens the connection to an OpenAI-compatible API in the background (DNS, TCP, TLS)
// while mem searches locally, so the first model call doesn't pay for the handshake.
// (The Anthropic SDK keeps its own connection pool, so this only helps the openai path.)
func warmUp(cfg Config, spec string) {
	p, _, err := cfg.resolveModel(spec)
	if err != nil || p.API != "openai" || p.BaseURL == "" {
		return
	}
	go func() {
		req, err := http.NewRequest("HEAD", strings.TrimRight(p.BaseURL, "/")+"/models", nil)
		if err != nil {
			return
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close() // the connection stays in the pool for the real request
		}
	}()
}

// Common words that only add noise to an OR search (English and Slovenian).
var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a an the and or but if of to in on at by for with from about as is are was were be been
		do does did have has had i me my we our you your it its this that these those what which who whom when where why how
		can could should would will may might must not no so than then there here any some all just get got tell show find
		je na za v z s pa ali kaj kje kdaj kako kdo zakaj ki se si sem so bo bi da ne to ta te tega moj moja moje mi me`) {
		m[w] = true
	}
	return m
}()

func stripStopwords(s string) string {
	var keep []string
	for _, t := range tokenRe.FindAllString(strings.ToLower(s), -1) {
		if !stopwords[t] {
			keep = append(keep, t)
		}
	}
	return strings.Join(keep, " ")
}

var citeRe = regexp.MustCompile(`\[([^\[\]\n]+)\]`)

// cited returns the existing, non-private notes whose ids the answer cites, in order of first citation.
func cited(answer string, db *sql.DB) []Note {
	out, seen := []Note{}, map[string]bool{}
	for _, m := range citeRe.FindAllStringSubmatch(answer, -1) {
		for _, id := range strings.Split(m[1], ",") { // "[a, b]" cites two notes
			id = strings.TrimSpace(id)
			if seen[id] || strings.HasPrefix(id, "REDACTED") || isPrivate(db, id) {
				continue
			}
			var n Note
			var tags string
			if db.QueryRow("SELECT id, created, title, tags FROM fts WHERE id=?", id).Scan(&n.ID, &n.Created, &n.Title, &tags) == nil {
				seen[id] = true
				n.Tags = strings.Fields(tags)
				n.Path = notePath(n.ID)
				out = append(out, n)
			}
		}
	}
	return out
}

// ---------- helpers ----------

type props map[string]any

func obj(p props, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": map[string]any(p), "required": required}
}
func str(d string) map[string]any     { return map[string]any{"type": "string", "description": d} }
func integer(d string) map[string]any { return map[string]any{"type": "integer", "description": d} }
func boolean(d string) map[string]any { return map[string]any{"type": "boolean", "description": d} }

func day(created string) string { return created[:min(len(created), 10)] }

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func clamp(v, def, hi int) int {
	if v <= 0 {
		return def
	}
	return min(v, hi)
}

func orEmptyJSON(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

// status shows progress on one line of stderr that updates in place and disappears as soon as the
// answer starts, so the screen ends up showing just the answer. When stderr or stdout isn't a
// terminal (logs, pipes), each step is printed on its own line instead.
type status struct {
	w     io.Writer
	live  bool
	shown bool
}

func newStatus(progress, out io.Writer) *status {
	if progress == nil {
		return &status{}
	}
	return &status{w: progress, live: isTTY(os.Stderr) && out == os.Stdout && isTTY(os.Stdout)}
}

func (st *status) set(line string) {
	switch {
	case st.w == nil:
	case st.live:
		fmt.Fprint(st.w, "\r\x1b[K"+c("2", "· "+clip(line, 100)))
		st.shown = true
	default:
		fmt.Fprintln(st.w, c("2", "· "+line))
	}
}

func (st *status) clear() {
	if st.live && st.shown {
		fmt.Fprint(st.w, "\r\x1b[K")
		st.shown = false
	}
}

// wrap returns a writer that clears the status line before the first answer text appears.
func (st *status) wrap(w io.Writer) io.Writer {
	if w == nil || !st.live {
		return w
	}
	return writerFunc(func(p []byte) (int, error) {
		st.clear()
		return w.Write(p)
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
