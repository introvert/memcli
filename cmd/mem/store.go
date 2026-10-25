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
	home      = memHome()
	notesD    = filepath.Join(home, "notes")
	trashD    = filepath.Join(home, "trash")
	dbPath    = filepath.Join(home, "index.db")
	tagRe     = regexp.MustCompile(`(?:^|\s)#([\p{L}\p{N}_][\p{L}\p{N}_/-]*)`)
	tokenRe   = regexp.MustCompile(`[\p{L}\p{N}_]+`)
	numericRe = regexp.MustCompile(`^[0-9]+$`)
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
}

// ---------- files (source of truth) ----------

func parseNote(path string) (Note, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Note{}, err
	}
	text, meta := string(b), map[string]string{}
	if strings.HasPrefix(text, "---\n") {
		if end := strings.Index(text[4:], "\n---\n"); end != -1 {
			for _, line := range strings.Split(text[4:4+end], "\n") {
				k, v, _ := strings.Cut(line, ":")
				meta[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
			text = text[4+end+5:]
		}
	}
	body := strings.TrimSpace(text)
	tags := map[string]bool{}
	for _, t := range strings.Split(meta["tags"], ",") {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			tags[t] = true
		}
	}
	for _, m := range tagRe.FindAllStringSubmatch(body, -1) {
		if !numericRe.MatchString(m[1]) { // "#123" is an issue number, not a tag
			tags[strings.ToLower(m[1])] = true
		}
	}
	title, _, _ := strings.Cut(body, "\n")
	if r := []rune(title); len(r) > 120 {
		title = string(r[:120])
	}
	return Note{
		ID: strings.TrimSuffix(filepath.Base(path), ".md"), Created: meta["created"],
		Tags: sortedKeys(tags), Title: title, Body: body, Path: path,
	}, nil
}

func writeNote(body string, tags []string) (string, error) {
	if err := os.MkdirAll(notesD, 0o755); err != nil {
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
		now.Format("2006-01-02T15:04:05"), strings.Join(sortedKeys(set), ", "), strings.TrimSpace(body))
	// O_EXCL: never clobber a note another mem process created in the same second; take the next id.
	for n := 1; ; n++ {
		id := base
		if n > 1 {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		f, err := os.OpenFile(filepath.Join(notesD, id+".md"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		defer f.Close()
		_, err = f.WriteString(content)
		return id, err
	}
}

// addTags rewrites a note's frontmatter tags line to include extra tags.
func addTags(id string, extra []string) error {
	path := filepath.Join(notesD, id+".md")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(b)
	start := strings.Index(text, "\ntags:")
	if !strings.HasPrefix(text, "---\n") || start == -1 {
		return fmt.Errorf("note %s has no tags line", id)
	}
	end := start + 1 + strings.Index(text[start+1:], "\n")
	set := map[string]bool{}
	for _, t := range append(strings.Split(text[start+7:end], ","), extra...) {
		if t = strings.ToLower(strings.TrimLeft(strings.TrimSpace(t), "#")); t != "" {
			set[t] = true
		}
	}
	text = text[:start] + "\ntags: " + strings.Join(sortedKeys(set), ", ") + text[end:]
	return os.WriteFile(path, []byte(text), 0o644)
}

func resolve(prefix string) (string, error) {
	hits, _ := filepath.Glob(filepath.Join(notesD, prefix+"*.md"))
	sort.Strings(hits)
	switch {
	case len(hits) == 0:
		return "", fmt.Errorf("no note matching '%s'", prefix)
	case len(hits) > 1:
		ids := []string{}
		for i, h := range hits {
			if i == 8 {
				break
			}
			ids = append(ids, strings.TrimSuffix(filepath.Base(h), ".md"))
		}
		return "", fmt.Errorf("'%s' is ambiguous: %s", prefix, strings.Join(ids, ", "))
	}
	return hits[0], nil
}

func gitCommit(msg string) {
	if exists(filepath.Join(home, ".git")) {
		exec.Command("git", "-C", home, "add", "-A").Run()
		exec.Command("git", "-C", home, "commit", "-qm", msg).Run()
	}
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
		CREATE TABLE IF NOT EXISTS files(id TEXT PRIMARY KEY, mtime INTEGER);
		CREATE VIRTUAL TABLE IF NOT EXISTS fts USING fts5(
			id UNINDEXED, created UNINDEXED, title, body, tags,
			tokenize = 'porter unicode61 remove_diacritics 2');`)
	if err != nil {
		return nil, err
	}
	return db, syncIndex(db)
}

// syncIndex brings the index up to date with the files on disk (cheap: mtime check).
func syncIndex(db *sql.DB) error {
	onDisk := map[string]string{}
	paths, _ := filepath.Glob(filepath.Join(notesD, "*.md"))
	for _, p := range paths {
		onDisk[strings.TrimSuffix(filepath.Base(p), ".md")] = p
	}
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
	for id := range known {
		if _, ok := onDisk[id]; !ok {
			tx.Exec("DELETE FROM files WHERE id=?", id)
			tx.Exec("DELETE FROM fts WHERE id=?", id)
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
		tx.Exec("DELETE FROM fts WHERE id=?", id)
		tx.Exec("INSERT INTO fts VALUES(?,?,?,?,?)", id, n.Created, n.Title, n.Body, strings.Join(n.Tags, " "))
		tx.Exec("INSERT OR REPLACE INTO files VALUES(?,?)", id, mt)
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

func search(db *sql.DB, match string, limit int, hlOn, hlOff string) ([]Hit, error) {
	rows, err := db.Query(`
		SELECT id, created, title, tags, snippet(fts, 3, ?, ?, '…', 14), body
		FROM fts WHERE fts MATCH ?
		ORDER BY bm25(fts, 0, 0, 5.0, 1.0, 3.0) LIMIT ?`, hlOn, hlOff, match, limit)
	if err != nil {
		return nil, err
	}
	return scanHits(rows)
}

func recent(db *sql.DB, tag string, limit int) ([]Hit, error) {
	q, args := "SELECT id, created, title, tags, '', body FROM fts", []any{}
	if tag != "" {
		q += " WHERE fts MATCH ?"
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
func allNotes(db *sql.DB, tag string) ([]Hit, int, error) {
	q, args := "SELECT id, created, title, tags, '', body FROM fts", []any{}
	if tag != "" {
		q += " WHERE fts MATCH ?"
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

func tagCounts(db *sql.DB) (map[string]int, error) {
	rows, err := db.Query("SELECT tags FROM fts")
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

// idOrder sorts ids chronologically: by timestamp, then numerically by the -N suffix (so -10 follows -9).
func idOrder(dir string) string {
	return fmt.Sprintf("substr(id, 1, 13) %[1]s, length(id) %[1]s, id %[1]s", dir)
}
