// mem — tiny personal memory. Plain markdown files + a disposable SQLite FTS5 index,
// plus optional AI (any model: Claude natively, everything else via OpenAI-compatible APIs).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const usage = `mem — tiny personal memory

  mem buy oat milk #todo          add a note (bare text = add; #hashtags become tags)
  echo "..." | mem                add from stdin
  mem -e                          add in $EDITOR
  mem add -a "..."                add and let AI pick tags
  mem s oat milk [-t todo]        search (ranked, prefix-matching)
  mem ask "when does the pg password rotate?"   AI agent searches your notes and answers, citing sources
  mem ask -d "..."                same, with deep thinking for hard questions (slower)
                                  after an answer: 1-9 copy/run · a copy answer · o open note · f follow-up
                                  (-q: no menu)
  mem secrets                     what the privacy filter hides from AI (values masked)
  mem key set deepseek            store an API key in the OS keychain (mem key ls / rm)
  mem ls [-n 20] [-t todo]        recent notes
  mem show ID                     print a note (ID prefix is enough)
  mem edit ID                     open in $EDITOR
  mem rm ID                       move to trash
  mem tags                        tag counts
  mem config                      show/create config (model, providers)
  mem path | reindex              storage dir | rebuild index

Flags: --json (s/ls/show/ask) machine-readable output · -m provider/model (ask/add) pick a model
Notes: notes_dir in config (or $MEM_NOTES), any folder of .md files incl. Obsidian vaults.
Config + index: $MEM_HOME (default ~/.mem). If the notes dir is in a git repo, changes are auto-committed.
Models: deepseek/deepseek-flash (default), anthropic/claude-opus-5-5, openai/<m>, gemini/<m>, ollama/<m>, openrouter/<m>, groq/<m>, ...`

var version = "dev" // set at release build time: -ldflags "-X main.version=v1.2.3"

var color = isTTY(os.Stdout) && os.Getenv("NO_COLOR") == ""

func main() {
	setupPaths(loadConfig())
	args := os.Args[1:]
	if len(args) == 0 {
		if piped() {
			cmdAdd(nil)
		} else {
			cmdLs(nil)
		}
		return
	}
	cmds := map[string]func([]string){
		"add": cmdAdd, "s": cmdSearch, "search": cmdSearch, "ask": cmdAsk,
		"ls": cmdLs, "show": cmdShow, "cat": cmdShow, "edit": cmdEdit, "rm": cmdRm,
		"tags": cmdTags, "config": cmdConfig, "reindex": cmdReindex, "secrets": cmdSecrets, "key": cmdKey, "serve": cmdServe, "__clipclear": cmdClipClear,
		"path": func([]string) { fmt.Println(notesD) },
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Println(usage)
	case "-v", "--version", "version":
		fmt.Println("mem " + version)
	default:
		if f, ok := cmds[args[0]]; ok {
			f(args[1:])
		} else {
			cmdAdd(args) // anything else is a note
		}
	}
}

// ---------- flags ----------

type opts struct {
	pos   []string
	kv    map[string]string
	flags map[string]bool
}

// parseOpts: valued flags (-t, -n, -m) take the next arg; bool flags are listed explicitly.
func parseOpts(args []string, bools ...string) opts {
	o := opts{kv: map[string]string{}, flags: map[string]bool{}}
	isBool := map[string]bool{"--json": true}
	for _, b := range bools {
		isBool[b] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case isBool[a]:
			o.flags[a] = true
		case a == "-t" || a == "-n" || a == "-m":
			if i+1 >= len(args) {
				die("%s needs a value", a)
			}
			o.kv[a] = args[i+1]
			i++
		default:
			o.pos = append(o.pos, a)
		}
	}
	return o
}

func (o opts) limit(def int) int {
	if v, ok := o.kv["-n"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			die("-n needs a positive number")
		}
		return n
	}
	return def
}

// ---------- commands ----------

func cmdAdd(args []string) {
	o := parseOpts(args, "-e", "-a")
	body := strings.Join(o.pos, " ")
	if body == "" && piped() {
		b, _ := io.ReadAll(os.Stdin)
		body = string(b)
	}
	if body == "" || o.flags["-e"] {
		body = editTemp(body)
	}
	if strings.TrimSpace(body) == "" {
		die("empty note, nothing saved")
	}
	var tags []string
	if t, ok := o.kv["-t"]; ok {
		tags = strings.Split(t, ",")
	}
	// Save first: a slow or failing model must never cost you the note.
	id, err := writeNote(body, tags)
	check(err)
	cfg := loadConfig()
	if o.flags["-a"] || cfg.AutoTag {
		if ai, err := suggestTags(cfg, pick(o, cfg.FastModel), body); err != nil {
			warn("auto-tag skipped: %v", err)
		} else if err := addTags(id, ai); err != nil {
			warn("auto-tag skipped: %v", err)
		} else {
			tags = append(tags, ai...)
		}
	}
	gitCommit("add " + id)
	fmt.Println(c("32", "saved ") + id + tagSuffix(tags))
}

func cmdSearch(args []string) {
	o := parseOpts(args)
	var parts []string
	if q := ftsQuery(strings.Join(o.pos, " "), "AND"); q != "" {
		parts = append(parts, q)
	}
	if t, ok := o.kv["-t"]; ok {
		parts = append(parts, tagQuery(t))
	}
	if len(parts) == 0 {
		die("usage: mem s WORDS [-t tag]")
	}
	db, err := openIndex()
	check(err)
	on, off := "", ""
	if color && !o.flags["--json"] {
		on, off = "\x1b[1;33m", "\x1b[0m"
	}
	hits, err := search(db, strings.Join(parts, " AND "), o.limit(20), on, off, false)
	check(err)
	if len(hits) == 0 && len(o.pos) > 0 { // maybe a typo: retry with the closest real words
		if fixed, ok := correct(db, strings.Join(o.pos, " ")); ok {
			q := ftsQuery(fixed, "OR")
			if t, ok := o.kv["-t"]; ok {
				q = "(" + q + ") AND " + tagQuery(t)
			}
			if hits, err = search(db, q, o.limit(20), on, off, false); err == nil && len(hits) > 0 && !o.flags["--json"] {
				fmt.Println(c("2", fmt.Sprintf("no exact matches for %q; showing close matches", strings.Join(o.pos, " "))))
			}
		}
	}
	if o.flags["--json"] {
		printJSON(notesOf(hits))
		return
	}
	if len(hits) == 0 {
		fmt.Println(c("2", "no matches"))
	}
	for _, h := range hits {
		snip := ""
		if strings.TrimSpace(h.Body) != h.Title {
			snip = strings.ReplaceAll(h.Snippet, "\n", " ")
		}
		printRow(h.Note, snip)
	}
}

func cmdAsk(args []string) {
	o := parseOpts(args, "-d", "-q")
	question := strings.TrimSpace(strings.Join(o.pos, " "))
	if question == "" {
		die("usage: mem ask QUESTION [-d] [-q] [-t tag] [-m provider/model]")
	}
	cfg := loadConfig()
	spec := pick(o, cfg.Model)
	warmUp(cfg, spec) // connect to the API while we search locally
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := openIndex()
	check(err)

	level := Normal
	if o.flags["-d"] {
		level = Deep
	}
	// Steps go to stderr and the answer streams to stdout, so `mem ask ... > file` stays clean.
	var progress, out io.Writer = os.Stderr, os.Stdout
	if o.flags["--json"] {
		progress, out = nil, nil
	}
	sess := newSession(db, cfg, spec, level, o.kv["-t"], progress)
	res, err := sess.ask(ctx, question, out)
	if out != nil {
		fmt.Println()
	}
	check(err)
	if o.flags["--json"] {
		printJSON(res)
		return
	}
	interactive := isTTY(os.Stdout) && isTTY(os.Stdin) && !o.flags["-q"] && (cfg.Actions == nil || *cfg.Actions)
	if !interactive || len(res.Sources) > 1 { // with one source, the menu's "o open <id>" names it
		printSources(res.Sources)
	}
	if interactive {
		actionMenu(sess, res)
	}
}

// cmdSecrets lists what the privacy filter redacts, so you can check it (values masked).
func cmdSecrets(args []string) {
	o := parseOpts(args)
	extra := compileExtra(loadConfig().RedactPatterns)
	ids := walkNotes()
	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	type row struct {
		ID   string `json:"id"`
		Line int    `json:"line"`
		Kind string `json:"kind"`
		Mask string `json:"masked"`
	}
	rows := []row{}
	for _, id := range keys {
		n, err := parseNote(ids[id])
		if err != nil {
			continue
		}
		_, found := redact(n.Body, extra)
		for _, f := range found {
			rows = append(rows, row{id, f.Line, f.Kind, f.Preview})
		}
	}
	if o.flags["--json"] {
		printJSON(rows)
		return
	}
	notes := map[string]bool{}
	for _, r := range rows {
		notes[r.ID] = true
		fmt.Printf("%s:%d  %s  %s\n", c("36", r.ID), r.Line, c("33", r.Kind), c("2", r.Mask))
	}
	fmt.Println(c("2", fmt.Sprintf("%d redactions in %d notes · AI only ever sees [REDACTED:kind]", len(rows), len(notes))))
}

func cmdLs(args []string) {
	o := parseOpts(args)
	db, err := openIndex()
	check(err)
	hits, err := recent(db, o.kv["-t"], o.limit(20), false)
	check(err)
	if o.flags["--json"] {
		printJSON(notesOf(hits))
		return
	}
	for _, h := range hits {
		printRow(h.Note, "")
	}
}

func cmdShow(args []string) {
	o := parseOpts(args)
	if len(o.pos) == 0 {
		die("usage: mem show ID")
	}
	db, err := openIndex()
	check(err)
	for _, p := range o.pos {
		path, err := resolve(db, p)
		check(err)
		n, err := parseNote(path)
		check(err)
		if o.flags["--json"] {
			printJSON(n)
			continue
		}
		fmt.Println(c("36", n.ID), c("2", n.Created), c("35", hashTags(n.Tags)))
		fmt.Println(n.Body + "\n")
	}
}

func cmdEdit(args []string) {
	if len(args) == 0 {
		die("usage: mem edit ID")
	}
	db, err := openIndex()
	check(err)
	path, err := resolve(db, args[0])
	check(err)
	runEditor(path)
	gitCommit("edit " + noteID(path))
}

func cmdRm(args []string) {
	if len(args) == 0 {
		die("usage: mem rm ID")
	}
	check(os.MkdirAll(trashD, 0o755))
	db, err := openIndex()
	check(err)
	for _, a := range args {
		path, err := resolve(db, a)
		check(err)
		id := noteID(path)
		dest := filepath.Join(trashD, strings.ReplaceAll(id, "/", "__")+".md")
		if exists(dest) {
			dest = strings.TrimSuffix(dest, ".md") + time.Now().Format("-060102-150405") + ".md"
		}
		check(os.Rename(path, dest))
		fmt.Println(c("31", "trashed ") + id)
	}
	gitCommit("rm " + strings.Join(args, " "))
}

func cmdTags([]string) {
	db, err := openIndex()
	check(err)
	counts, err := tagCounts(db, false)
	check(err)
	for _, t := range sortedByCount(counts) {
		fmt.Printf("%5d  #%s\n", counts[t], t)
	}
}

func cmdConfig([]string) {
	check(writeDefaultConfig())
	cfg := loadConfig()
	fmt.Println("config:     " + configPath)
	fmt.Println("notes_dir:  " + notesD + c("2", "   (new notes → "+inboxD+")"))
	fmt.Println("model:      " + cfg.Model + c("2", "   (mem ask)"))
	fmt.Println("fast_model: " + cfg.FastModel + c("2", "   (tagging)"))
	fmt.Printf("max_steps:  %d%s\n", cfg.MaxSteps, c("2", "   (tool calls per ask)"))
	fmt.Printf("auto_tag:   %v\n", cfg.AutoTag)
	names := providerNames(cfg)
	fmt.Println("providers:  " + strings.Join(names, ", "))
	fmt.Println(c("2", "override per run: mem ask -m ollama/llama3.2 ... · or MEM_MODEL=gemini/gemini-2.5-flash"))
}

func cmdReindex([]string) {
	os.Remove(dbPath)
	db, err := openIndex()
	check(err)
	var n int
	db.QueryRow("SELECT count(*) FROM files").Scan(&n)
	fmt.Printf("indexed %d notes\n", n)
}

// ---------- AI helpers ----------

func suggestTags(cfg Config, spec, body string) ([]string, error) {
	existing := ""
	if db, err := openIndex(); err == nil {
		if counts, err := tagCounts(db, true); err == nil { // the tag list goes to the model
			top := sortedByCount(counts)
			if len(top) > 60 {
				top = top[:60]
			}
			existing = strings.Join(top, ", ")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	safe, _ := redact(body, compileExtra(cfg.RedactPatterns))
	out, err := complete(ctx, cfg, spec, tagSystem,
		fmt.Sprintf("Existing tags: %s\n\n<note>\n%s\n</note>", orNone(existing), safe), Quick)
	if err != nil {
		return nil, err
	}
	return parseTags(out), nil
}

func pick(o opts, def string) string {
	if m, ok := o.kv["-m"]; ok {
		return m
	}
	return def
}

// ---------- output & misc ----------

func printRow(n Note, extra string) {
	date := n.Created
	if len(date) > 10 {
		date = date[:10]
	}
	title := strings.TrimSpace(tagRe.ReplaceAllString(n.Title, "")) // tags are shown once, at the end
	fmt.Println(strings.TrimRight(fmt.Sprintf("%s  %s  %s  %s", c("36", n.ID), c("2", date), title, c("35", hashTags(n.Tags))), " "))
	if extra != "" {
		fmt.Println("    " + extra)
	}
}

func notesOf(hits []Hit) []Note {
	out := make([]Note, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Note)
	}
	return out
}

func hashTags(tags []string) string {
	var s []string
	for _, t := range tags {
		s = append(s, "#"+t)
	}
	return strings.Join(s, " ")
}

func tagSuffix(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	return "  " + c("35", hashTags(tags))
}

func sortedByCount(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

func editTemp(initial string) string {
	f, err := os.CreateTemp("", "mem-*.md")
	check(err)
	f.WriteString(initial)
	f.Close()
	defer os.Remove(f.Name())
	runEditor(f.Name())
	b, _ := os.ReadFile(f.Name())
	return string(b)
}

func runEditor(path string) {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		ed = "vi"
	}
	cmd := exec.Command("sh", "-c", ed+` "$1"`, "sh", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Run()
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	enc.Encode(v)
}

func c(code, s string) string {
	if !color || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func orNone(s string) string {
	if s == "" {
		return "(none yet)"
	}
	return s
}

// piped is true only when stdin is a real pipe or redirected file (never blocks on a terminal).
func piped() bool {
	st, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeNamedPipe != 0 || st.Mode().IsRegular()
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func warn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, c("33", "mem: ")+format+"\n", a...)
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "mem: "+format+"\n", a...)
	os.Exit(1)
}

func check(err error) {
	if err != nil {
		die("%v", err)
	}
}
