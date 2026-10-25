package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	home   = memHome() // config + index live here (never inside the notes dir)
	dbPath = filepath.Join(home, "index.db")
	// Set by setupPaths from config: the notes dir (e.g. an Obsidian vault), where new notes go, and trash.
	notesD, inboxD, trashD string
	tagRe                  = regexp.MustCompile(`(?:^|\s)#([\p{L}\p{N}_][\p{L}\p{N}_/-]*)`)
	tokenRe                = regexp.MustCompile(`[\p{L}\p{N}_]+`)
	numericRe              = regexp.MustCompile(`^[0-9]+$`)
)

func memHome() string {
	if h := os.Getenv("MEM_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".mem")
}

type Note struct {
	ID      string   `json:"id"`
	Created string   `json:"created"`
	Tags    []string `json:"tags"`
	Title   string   `json:"title"`
	Body    string   `json:"body"`
	Path    string   `json:"path,omitempty"`
	Private bool     `json:"private,omitempty"` // hidden from AI (frontmatter or ai_exclude)
}

// ---------- files (source of truth) ----------

func setupPaths(cfg Config) {
	notesD = cfg.NotesDir
	inboxD = filepath.Join(notesD, cfg.Inbox)
	trashD = filepath.Join(notesD, ".trash") // same folder Obsidian uses
}

// noteID is the note's path relative to the notes dir, without ".md" (e.g. "Kundix/Crypto/wallets").
func noteID(path string) string {
	rel, err := filepath.Rel(notesD, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	return filepath.ToSlash(strings.TrimSuffix(rel, ".md"))
}

func notePath(id string) string { return filepath.Join(notesD, filepath.FromSlash(id)+".md") }

// parseNote reads a markdown file, with or without YAML frontmatter (mem's own or Obsidian's).
func parseNote(path string) (Note, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Note{}, err
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	meta, tags := map[string]string{}, map[string]bool{}
	if fm, rest, ok := splitFrontmatter(text); ok {
		text = rest
		key := ""
		for _, line := range strings.Split(fm, "\n") {
			if item, ok := strings.CutPrefix(strings.TrimSpace(line), "- "); ok && line != strings.TrimLeft(line, " -") {
				if key == "tags" { // YAML block list: "tags:\n  - a\n  - b"
					addTagList(tags, item)
				}
				continue
			}
			k, v, _ := strings.Cut(line, ":")
			key = strings.TrimSpace(k)
			meta[key] = strings.TrimSpace(v)
		}
		addTagList(tags, meta["tags"])
	}
	body := strings.TrimSpace(text)
	for _, m := range tagRe.FindAllStringSubmatch(body, -1) {
		if !numericRe.MatchString(m[1]) { // "#123" is an issue number, not a tag
			tags[strings.ToLower(m[1])] = true
		}
	}
	created := meta["created"]
	if created == "" {
		created = meta["date"]
	}
	if created == "" { // plain markdown file: fall back to its modification time
		if st, err := os.Stat(path); err == nil {
			created = st.ModTime().Format("2006-01-02T15:04:05")
		}
	}
	id := noteID(path)
	return Note{
		ID: id, Created: strings.Trim(created, `"'`),
		Tags: sortedKeys(tags), Title: titleOf(body, path), Body: body, Path: path,
		Private: isTrue(meta["private"]) || strings.EqualFold(meta["ai"], "false") || aiExcluded(id),
	}, nil
}

func isTrue(v string) bool {
	v = strings.ToLower(strings.Trim(v, `"' `))
	return v == "true" || v == "yes" || v == "1"
}

// aiExcluded reports whether a note id matches the config's ai_exclude patterns:
// "Kundix/Crypto" (a folder or note), "**/secrets*", "Kundix/*/keys" (* = within a folder, ** = any depth).
func aiExcluded(id string) bool {
	for _, re := range aiExcludeRes() {
		if re.MatchString(id) {
			return true
		}
	}
	return false
}

var aiExcludeCache []*regexp.Regexp

func aiExcludeRes() []*regexp.Regexp {
	if aiExcludeCache == nil {
		aiExcludeCache = []*regexp.Regexp{}
		for _, g := range loadConfig().AIExclude {
			g = strings.Trim(strings.TrimSuffix(strings.TrimSpace(g), ".md"), "/")
			if g == "" {
				continue
			}
			var b strings.Builder
			for i := 0; i < len(g); i++ {
				switch {
				case strings.HasPrefix(g[i:], "**/"):
					b.WriteString("(?:.*/)?")
					i += 2
				case strings.HasPrefix(g[i:], "**"):
					b.WriteString(".*")
					i++
				case g[i] == '*':
					b.WriteString("[^/]*")
				case g[i] == '?':
					b.WriteString("[^/]")
				default:
					b.WriteString(regexp.QuoteMeta(g[i : i+1]))
				}
			}
			// A pattern matches the note itself or anything under it (folders).
			aiExcludeCache = append(aiExcludeCache, regexp.MustCompile("(?i)^"+b.String()+"(?:/.*)?$"))
		}
	}
	return aiExcludeCache
}

func splitFrontmatter(text string) (fm, rest string, ok bool) {
	if !strings.HasPrefix(text, "---\n") {
		return "", text, false
	}
	end := strings.Index(text[4:], "\n---")
	if end == -1 {
		return "", text, false
	}
	rest = text[4+end+4:]
	if nl := strings.IndexByte(rest, '\n'); nl != -1 {
		rest = rest[nl+1:]
	} else {
		rest = ""
	}
	return text[4 : 4+end], rest, true
}

// addTagList accepts "a, b", "[a, b]", "a b" and quoted forms.
func addTagList(set map[string]bool, v string) {
	v = strings.Trim(strings.TrimSpace(v), "[]")
	for _, t := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
		if t = strings.ToLower(strings.Trim(t, `"'#`)); t != "" {
			set[t] = true
		}
	}
}

// titleOf is the first non-empty line without markdown heading marks, or the file name.
func titleOf(body, path string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			continue // code fence markers aren't titles
		}
		if line = strings.TrimSpace(strings.TrimLeft(line, "# ")); line != "" {
			if r := []rune(line); len(r) > 120 {
				line = string(r[:120])
			}
			return line
		}
	}
	return strings.TrimSuffix(filepath.Base(path), ".md")
}

func writeNote(body string, tags []string) (string, error) {
	if err := os.MkdirAll(inboxD, 0o755); err != nil {
		return "", err
	}
	now := time.Now()
	base := now.Format("060102-150405")
	set := map[string]bool{}
	for _, t := range tags {
		if t = strings.ToLower(strings.TrimLeft(strings.TrimSpace(t), "#")); t != "" {
			set[t] = true
		}
	}
	content := fmt.Sprintf("---\ncreated: %s\ntags: %s\n---\n%s\n",
		now.Format("2006-01-02T15:04:05"), yamlList(sortedKeys(set)), strings.TrimSpace(body))
	// O_EXCL: never clobber a note another mem process created in the same second; take the next id.
	for n := 1; ; n++ {
		name := base
		if n > 1 {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		path := filepath.Join(inboxD, name+".md")
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		defer f.Close()
		_, err = f.WriteString(content)
		return noteID(path), err
	}
}

// yamlList renders tags as a YAML flow list, which Obsidian reads as tags.
func yamlList(tags []string) string { return "[" + strings.Join(tags, ", ") + "]" }

// addTags rewrites a note's single-line frontmatter "tags:" entry to include extra tags.
func addTags(id string, extra []string) error {
	path := notePath(id)
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(b)
	fm, _, ok := splitFrontmatter(text)
	start := strings.Index(text, "\ntags:")
	if !ok || start == -1 || start > len(fm)+4 {
		return fmt.Errorf("note %s has no frontmatter tags line", id)
	}
	end := start + 1 + strings.Index(text[start+1:], "\n")
	set := map[string]bool{}
	addTagList(set, text[start+7:end])
	for _, t := range extra {
		addTagList(set, t)
	}
	text = text[:start] + "\ntags: " + yamlList(sortedKeys(set)) + text[end:]
	return os.WriteFile(path, []byte(text), 0o644)
}

// resolve finds a note by id or file name, exact match first, then by prefix (case-insensitive).
func resolve(db *sql.DB, q string) (string, error) {
	rows, err := db.Query("SELECT id FROM files")
	if err != nil {
		return "", err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	sort.Strings(ids)
	lq := strings.ToLower(strings.TrimSuffix(q, ".md"))
	base := func(id string) string { return strings.ToLower(id[strings.LastIndex(id, "/")+1:]) }
	tests := []func(string) bool{
		func(id string) bool { return strings.ToLower(id) == lq },
		func(id string) bool { return base(id) == lq },
		func(id string) bool { return strings.HasPrefix(strings.ToLower(id), lq) },
		func(id string) bool { return strings.HasPrefix(base(id), lq) },
	}
	for _, match := range tests {
		var hits []string
		for _, id := range ids {
			if match(id) {
				hits = append(hits, id)
			}
		}
		switch {
		case len(hits) == 1:
			return notePath(hits[0]), nil
		case len(hits) > 1:
			if len(hits) > 8 {
				hits = append(hits[:8], "…")
			}
			return "", fmt.Errorf("'%s' is ambiguous: %s", q, strings.Join(hits, ", "))
		}
	}
	return "", fmt.Errorf("no note matching '%s'", q)
}

// gitCommit commits changes under the notes dir, if it is inside a git repo.
func gitCommit(msg string) {
	if exec.Command("git", "-C", notesD, "rev-parse", "--is-inside-work-tree").Run() != nil {
		return
	}
	exec.Command("git", "-C", notesD, "add", "-A", "--", ".").Run()
	exec.Command("git", "-C", notesD, "commit", "-qm", msg, "--", ".").Run()
}

// ---------- index (disposable cache over the files) ----------

func openIndex() (*sql.DB, error) {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, err
	}
	// WAL + busy_timeout: parallel mem processes wait for each other instead of failing.
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT);
		CREATE TABLE IF NOT EXISTS files(id TEXT PRIMARY KEY, mtime INTEGER);
		CREATE TABLE IF NOT EXISTS private(id TEXT PRIMARY KEY); -- notes hidden from AI
		CREATE VIRTUAL TABLE IF NOT EXISTS fts USING fts5(
			id UNINDEXED, created UNINDEXED, title, body, tags,
			tokenize = 'porter unicode61 remove_diacritics 2');`)
	if err != nil {
		return nil, err
	}
	// The index belongs to one notes dir; if the config points elsewhere now, start over.
	var dir string
	db.QueryRow("SELECT v FROM meta WHERE k='notes_dir'").Scan(&dir)
	// Same for the redaction rules: the index only ever holds redacted text.
	// And for ai_exclude, which decides the private table.
	cfg := loadConfig()
	want := notesD + "|" + redactVersion + "|" + strings.Join(cfg.RedactPatterns, "\x00") + "|" + strings.Join(cfg.AIExclude, "\x00")
	if dir != want {
		db.Exec("DELETE FROM files; DELETE FROM fts; DELETE FROM private;")
		db.Exec("INSERT OR REPLACE INTO meta VALUES('notes_dir', ?)", want)
	}
	ensureVocab(db)
	return db, syncIndex(db)
}

// walkNotes lists every .md file under the notes dir, skipping hidden folders (.obsidian, .trash, .git…).
func walkNotes() map[string]string {
	found := map[string]string{}
	filepath.WalkDir(notesD, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && p != notesD && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") && !strings.HasPrefix(d.Name(), ".") {
			found[noteID(p)] = p
		}
		return nil
	})
	return found
}

// syncIndex brings the index up to date with the files on disk (cheap: mtime check).
func syncIndex(db *sql.DB) error {
	onDisk := walkNotes()
	known := map[string]int64{}
	rows, err := db.Query("SELECT id, mtime FROM files")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var mt int64
		rows.Scan(&id, &mt)
		known[id] = mt
	}
	rows.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	extra := compileExtra(loadConfig().RedactPatterns)
	for id := range known {
		if _, ok := onDisk[id]; !ok {
			tx.Exec("DELETE FROM files WHERE id=?", id)
			tx.Exec("DELETE FROM fts WHERE id=?", id)
			tx.Exec("DELETE FROM private WHERE id=?", id)
		}
	}
	for id, p := range onDisk {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		mt := st.ModTime().UnixNano()
		if old, ok := known[id]; ok && old == mt {
			continue
		}
		n, err := parseNote(p)
		if err != nil {
			continue
		}
		body, _ := redact(n.Body, extra)
		tx.Exec("DELETE FROM fts WHERE id=?", id)
		tx.Exec("INSERT INTO fts VALUES(?,?,?,?,?)", id, n.Created, titleOf(body, p), body, strings.Join(n.Tags, " "))
		tx.Exec("INSERT OR REPLACE INTO files VALUES(?,?)", id, mt)
		if n.Private {
			tx.Exec("INSERT OR IGNORE INTO private VALUES(?)", id)
		} else {
			tx.Exec("DELETE FROM private WHERE id=?", id)
		}
	}
	return tx.Commit()
}

// ftsQuery turns free text into an FTS5 query of prefix terms joined by op ("AND" / "OR").
func ftsQuery(text, op string) string {
	var terms []string
	for _, t := range tokenRe.FindAllString(text, -1) {
		terms = append(terms, `"`+t+`"*`)
	}
	return strings.Join(terms, " "+op+" ")
}

func tagQuery(tag string) string {
	tag = strings.ReplaceAll(strings.ToLower(strings.TrimLeft(tag, "#")), `"`, "")
	return `tags:"` + tag + `"`
}

type Hit struct {
	Note
	Snippet string `json:"-"`
}

// notPrivate is appended to queries that feed the AI: private notes don't exist for it.
const notPrivate = " AND id NOT IN (SELECT id FROM private)"

func aiFilter(ai bool) string {
	if ai {
		return notPrivate
	}
	return ""
}

func search(db *sql.DB, match string, limit int, hlOn, hlOff string, ai bool) ([]Hit, error) {
	rows, err := db.Query(`
		SELECT id, created, title, tags, snippet(fts, 3, ?, ?, '…', 14), body
		FROM fts WHERE fts MATCH ?`+aiFilter(ai)+`
		ORDER BY bm25(fts, 0, 0, 5.0, 1.0, 3.0) LIMIT ?`, hlOn, hlOff, match, limit)
	if err != nil {
		return nil, err
	}
	return scanHits(rows)
}

func recent(db *sql.DB, tag string, limit int, ai bool) ([]Hit, error) {
	q, args := "SELECT id, created, title, tags, '', body FROM fts WHERE 1=1"+aiFilter(ai), []any{}
	if tag != "" {
		q += " AND fts MATCH ?"
		args = append(args, tagQuery(tag))
	}
	rows, err := db.Query(q+" ORDER BY "+idOrder("DESC")+" LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	return scanHits(rows)
}

// allNotes returns every note (optionally one tag), oldest first, and their total size.
// Oldest-first keeps the prompt prefix stable as notes are added, so provider caching works.
func allNotes(db *sql.DB, tag string, ai bool) ([]Hit, int, error) {
	q, args := "SELECT id, created, title, tags, '', body FROM fts WHERE 1=1"+aiFilter(ai), []any{}
	if tag != "" {
		q += " AND fts MATCH ?"
		args = append(args, tagQuery(tag))
	}
	rows, err := db.Query(q+" ORDER BY "+idOrder("ASC"), args...)
	if err != nil {
		return nil, 0, err
	}
	hits, err := scanHits(rows)
	size := 0
	for _, h := range hits {
		size += len(h.Body) + 100
	}
	return hits, size, err
}

func scanHits(rows *sql.Rows) ([]Hit, error) {
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		var tags string
		if err := rows.Scan(&h.ID, &h.Created, &h.Title, &tags, &h.Snippet, &h.Body); err != nil {
			return nil, err
		}
		h.Tags = strings.Fields(tags)
		if h.Tags == nil {
			h.Tags = []string{}
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func isPrivate(db *sql.DB, id string) bool {
	var x string
	return db.QueryRow("SELECT id FROM private WHERE id=?", id).Scan(&x) == nil
}

func tagCounts(db *sql.DB, ai bool) (map[string]int, error) {
	rows, err := db.Query("SELECT tags FROM fts WHERE 1=1" + aiFilter(ai))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var tags string
		rows.Scan(&tags)
		for _, t := range strings.Fields(tags) {
			counts[t]++
		}
	}
	return counts, nil
}

// ---------- helpers ----------

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// idOrder sorts notes chronologically; ties (same second) by id length then id, so "-10" follows "-9".
func idOrder(dir string) string {
	return fmt.Sprintf("created %[1]s, length(id) %[1]s, id %[1]s", dir)
}
