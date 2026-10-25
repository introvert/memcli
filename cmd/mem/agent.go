package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// `mem ask` runs a small agent: the model explores the notes with search/grep/list/read tools
// and answers with citations. Every tool reads the index, whose note text is already redacted,
// so secrets never reach the model and can't be probed by searching for guesses.

const agentSystem = `You are mem, the user's personal-notes assistant. You answer questions from the user's
notes, which you can only see through tools:
- search: ranked full-text search (stemmed, prefix matching). Usually the best first step.
- grep: exact text or regex over all notes; returns matching lines. Use for names, numbers, code, exact phrases.
- list: browse folders, tags and the most recent notes.
- read: read a note with line numbers.

How to work:
- Try a few phrasings: synonyms, related terms, and any other language the notes may be written in.
- Read the most promising notes before answering; snippets alone can mislead.
- Be efficient: stop as soon as you can answer. Several independent lookups can go in one turn.

Answer in the language of the question, concisely, in plain text for a terminal (no markdown headings).
After each fact, cite the note id in square brackets exactly as the tools show it, e.g. [mem/261025-143012]
or [Projects/ideas]. If the notes don't contain the answer, say so plainly instead of guessing.

Privacy: values shown as [REDACTED:kind] were removed by the user's privacy filter before reaching you.
Never guess or try to reconstruct them. If the user asks for one, name the note that holds it and tell them
to run: mem show <id>`

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

func runAgent(ctx context.Context, db *sql.DB, cfg Config, spec, question, tag string, progress io.Writer) (askResult, error) {
	user := fmt.Sprintf("Today is %s.\n\nQuestion: %s", time.Now().Format("2006-01-02 (Monday)"), question)
	if tag != "" {
		user += fmt.Sprintf("\n\n(The user is asking about notes tagged #%s.)", tag)
	}
	chat, err := newChat(cfg, spec, agentSystem, user, agentTools, false)
	if err != nil {
		return askResult{}, err
	}
	res := askResult{Steps: []string{}}
	for step := 0; ; step++ {
		last := step >= cfg.MaxSteps // out of budget: answer with what we have
		text, calls, err := chat.Next(ctx, !last)
		if err != nil {
			return res, err
		}
		if len(calls) == 0 || last {
			res.Answer = strings.TrimSpace(text)
			break
		}
		results := make([]string, len(calls))
		for i, call := range calls {
			out, summary := runTool(db, call)
			results[i] = out
			line := call.Name + " " + summary
			res.Steps = append(res.Steps, line)
			if progress != nil {
				fmt.Fprintln(progress, c("2", "· "+line))
			}
		}
		chat.AddResults(calls, results)
	}
	res.Sources = cited(res.Answer, db)
	return res, nil
}

// runTool executes one tool call. Errors go back to the model as text so it can correct itself.
func runTool(db *sql.DB, call ToolCall) (out, summary string) {
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
		return toolSearch(db, a.Query, a.Tag, clamp(a.Limit, 10, 30))
	case "grep":
		return toolGrep(db, a.Pattern, a.Regex, a.Folder)
	case "list":
		return toolList(db, a.Folder, a.Tag, clamp(a.Limit, 30, 100))
	case "read":
		return toolRead(db, a.ID, max(a.Offset, 1), clamp(a.Limit, 300, 1000))
	}
	return "error: unknown tool " + call.Name, "(unknown tool)"
}

func toolSearch(db *sql.DB, query, tag string, limit int) (string, string) {
	sum := fmt.Sprintf("%q", query)
	run := func(op string) ([]Hit, error) {
		q := ftsQuery(query, op)
		if q == "" {
			return nil, nil
		}
		if tag != "" {
			q = "(" + q + ") AND " + tagQuery(tag)
		}
		return search(db, q, limit, "", "")
	}
	hits, err := run("AND")
	if err == nil && len(hits) == 0 { // nothing has every word: rank by any of them
		hits, err = run("OR")
	}
	if err != nil {
		return "error: " + err.Error(), sum
	}
	if len(hits) == 0 {
		return "no matches", sum + " → 0"
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "%s | %s | %s | %s\n  %s\n", h.ID, day(h.Created), h.Title, hashTags(h.Tags),
			strings.ReplaceAll(h.Snippet, "\n", " "))
	}
	return b.String(), fmt.Sprintf("%s → %d", sum, len(hits))
}

func toolGrep(db *sql.DB, pattern string, isRegex bool, folder string) (string, string) {
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
	hits, _, err := allNotes(db, "")
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
				fmt.Fprintf(&b, "%s:%d: %s\n", h.ID, i+1, clip(strings.TrimSpace(line), 240))
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

func toolList(db *sql.DB, folder, tag string, limit int) (string, string) {
	if tag != "" {
		hits, err := recent(db, tag, limit)
		if err != nil {
			return "error: " + err.Error(), "#" + tag
		}
		return noteLines(hits), fmt.Sprintf("#%s → %d", tag, len(hits))
	}
	all, _, err := allNotes(db, "")
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
		hits, _ := recent(db, "", min(limit, 15))
		b.WriteString("Most recent notes:\n" + noteLines(hits))
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
	b.WriteString(noteLines(direct))
	return b.String(), fmt.Sprintf("%s → %d notes, %d folders", folder, len(direct), len(sub))
}

func toolRead(db *sql.DB, id string, offset, limit int) (string, string) {
	p, err := resolve(db, id)
	if err != nil {
		return "error: " + err.Error(), id
	}
	nid := noteID(p)
	var created, tags, body string
	if err := db.QueryRow("SELECT created, tags, body FROM fts WHERE id=?", nid).Scan(&created, &tags, &body); err != nil {
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
		fmt.Fprintf(&b, "%d\t%s\n", i+1, clip(lines[i], 2000))
		if b.Len() > 40_000 {
			fmt.Fprintf(&b, "… stopped at line %d (size limit); continue with offset=%d\n", i+1, i+2)
			break
		}
	}
	return b.String(), nid
}

var citeRe = regexp.MustCompile(`\[([^\[\]\n]+)\]`)

// cited returns the existing notes whose ids the answer cites, in order of first citation.
func cited(answer string, db *sql.DB) []Note {
	out, seen := []Note{}, map[string]bool{}
	for _, m := range citeRe.FindAllStringSubmatch(answer, -1) {
		for _, id := range strings.Split(m[1], ",") { // "[a, b]" cites two notes
			id = strings.TrimSpace(id)
			if seen[id] || strings.HasPrefix(id, "REDACTED") {
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

func noteLines(hits []Hit) string {
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "  %s | %s | %s | %s\n", h.ID, day(h.Created), h.Title, hashTags(h.Tags))
	}
	return b.String()
}

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
