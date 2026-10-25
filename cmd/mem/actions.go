package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// After `mem ask` answers in a terminal, a one-key menu offers what the answer contains:
// copy a value, copy a secret (filled in locally — the model never saw it), run a command
// (shown first, secrets masked, then confirmed), open the cited note, or ask a follow-up.

const clipClearAfter = 45 * time.Second

type item struct {
	kind string // "value", "secret" or "cmd"
	text string // as the model wrote it, placeholders included
}

var (
	fenceRe  = regexp.MustCompile("(?s)```([A-Za-z0-9_-]*)[^\n]*\n(.*?)```")
	inlineRe = regexp.MustCompile("`([^`\n]+)`")
	shells   = map[string]bool{"": true, "sh": true, "bash": true, "zsh": true, "shell": true, "console": true, "fish": true}
)

// extractItems finds what the user may want to copy or run, in order of appearance (max 9).
func extractItems(answer string) []item {
	type found struct {
		at int
		item
	}
	var all []found
	fences := fenceRe.FindAllStringSubmatchIndex(answer, -1)
	inFence := func(at int) bool {
		for _, f := range fences {
			if at >= f[0] && at < f[1] {
				return true
			}
		}
		return false
	}
	for _, f := range fences {
		lang, body := answer[f[2]:f[3]], answer[f[4]:f[5]]
		if shells[strings.ToLower(lang)] {
			var lines []string
			for _, l := range strings.Split(strings.TrimSpace(body), "\n") {
				lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(l, "$ "), "% "))
			}
			all = append(all, found{f[0], item{"cmd", strings.Join(lines, "\n")}})
		} else {
			all = append(all, found{f[0], item{"value", body}})
		}
	}
	for _, m := range inlineRe.FindAllStringSubmatchIndex(answer, -1) {
		if inFence(m[0]) {
			continue
		}
		text, kind := answer[m[2]:m[3]], "value"
		if looksLikeCommand(text) {
			kind = "cmd"
		}
		all = append(all, found{m[0], item{kind, text}})
	}
	for _, m := range placeholderRe.FindAllStringIndex(answer, -1) {
		if !inFence(m[0]) {
			all = append(all, found{m[0], item{"secret", answer[m[0]:m[1]]}})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at < all[j].at })
	var items []item
	seen := map[string]bool{}
	for _, f := range all {
		if t := strings.TrimSpace(f.text); t != "" && !seen[t] && len(items) < 9 {
			seen[t] = true
			items = append(items, item{f.kind, t})
		}
	}
	return items
}

// looksLikeCommand: an inline span whose first word is a program on PATH, with arguments.
func looksLikeCommand(s string) bool {
	f := strings.Fields(s)
	if len(f) < 2 || strings.ContainsAny(f[0], "/:@") && !strings.HasPrefix(f[0], "./") {
		return false
	}
	_, err := exec.LookPath(f[0])
	return err == nil
}

// masked shows text with placeholders as •••(kind), for display.
func masked(text string) string {
	return placeholderRe.ReplaceAllString(text, "•••($1)")
}

// fill replaces placeholders with the real secrets, read locally from the notes.
func (s *session) fill(text string) (string, bool, error) {
	var ferr error
	had := false
	out := placeholderRe.ReplaceAllStringFunc(text, func(m string) string {
		k, _ := strconv.Atoi(placeholderRe.FindStringSubmatch(m)[2])
		v, _, err := s.secret(k)
		if err != nil {
			ferr = err
			return m
		}
		had = true
		return v
	})
	return out, had, ferr
}

func actionMenu(s *session, res askResult) {
	listed := false // the full item list is printed once (and again after a follow-up)
	for {
		items := extractItems(res.Answer)
		for i, it := range items {
			if listed {
				break
			}
			if i == 0 {
				fmt.Fprintln(os.Stderr)
			}
			label := map[string]string{"value": "copy", "secret": "copy", "cmd": "run "}[it.kind]
			text := masked(it.text)
			if it.kind == "secret" {
				_, note, _ := s.secret(secretNum(it.text))
				text = fmt.Sprintf("%s from %s", placeholderRe.FindStringSubmatch(it.text)[1], note) + c("2", "  (never shown; clipboard clears in 45s)")
			}
			fmt.Fprintf(os.Stderr, "  %s  %s  %s\n", c("36", strconv.Itoa(i+1)), label, firstLine(text))
		}
		listed = true
		keys := []string{c("36", "a") + " copy answer"}
		switch n := len(items); {
		case n == 1:
			keys = append([]string{c("36", "1")}, keys...)
		case n > 1:
			keys = append([]string{c("36", "1-"+strconv.Itoa(n))}, keys...)
		}
		if len(res.Sources) > 0 {
			keys = append(keys, c("36", "o")+" open "+res.Sources[0].ID)
		}
		keys = append(keys, c("36", "f")+" follow-up", c("36", "⏎")+" done")
		fmt.Fprint(os.Stderr, "  "+strings.Join(keys, c("2", " · "))+" ")

		k := readKey()
		fmt.Fprintln(os.Stderr)
		switch {
		case k >= '1' && k <= '9' && int(k-'1') < len(items):
			s.doItem(items[k-'1'])
		case k == 'a':
			report(copyText(res.Answer, false), "copied answer")
		case k == 'o' && len(res.Sources) > 0:
			runEditor(notePath(res.Sources[0].ID))
		case k == 'f':
			q := readLine(c("36", "follow-up › "))
			if q == "" {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			next, err := s.ask(ctx, q, os.Stdout)
			cancel()
			fmt.Println()
			if err != nil {
				warn("%v", err)
				continue
			}
			printSources(next.Sources)
			res, listed = next, false
		default: // Enter, q, Esc, Ctrl-C, anything else
			return
		}
	}
}

func (s *session) doItem(it item) {
	switch it.kind {
	case "value":
		report(copyText(it.text, false), "copied")
	case "secret":
		v, note, err := s.secret(secretNum(it.text))
		if err != nil {
			warn("%v", err)
			return
		}
		report(copyText(v, true), fmt.Sprintf("copied secret from %s (clears in %ds)", note, int(clipClearAfter.Seconds())))
	case "cmd":
		fmt.Fprintf(os.Stderr, "  %s\n  %s run · %s copy · any other key cancels ", c("33", masked(it.text)), c("36", "y"), c("36", "c"))
		k := readKey()
		fmt.Fprintln(os.Stderr)
		cmd, hasSecret, err := s.fill(it.text)
		if err != nil {
			warn("%v", err)
			return
		}
		switch k {
		case 'y':
			runShell(cmd)
		case 'c':
			msg := "copied command"
			if hasSecret {
				msg += fmt.Sprintf(" (contains a secret, clears in %ds)", int(clipClearAfter.Seconds()))
			}
			report(copyText(cmd, hasSecret), msg)
		}
	}
}

func secretNum(placeholder string) int {
	n, _ := strconv.Atoi(placeholderRe.FindStringSubmatch(placeholder)[2])
	return n
}

func runShell(cmd string) {
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.Command("cmd", "/C", cmd)
	} else {
		sh := os.Getenv("SHELL")
		if sh == "" {
			sh = "sh"
		}
		c = exec.Command(sh, "-c", cmd)
	}
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		warn("command exited: %v", err)
	}
}

func printSources(sources []Note) {
	if len(sources) == 0 {
		return
	}
	ids := make([]string, len(sources))
	for i, n := range sources {
		ids[i] = n.ID
	}
	label := "source: "
	if len(ids) > 1 {
		label = "sources: "
	}
	fmt.Println(c("2", label+strings.Join(ids, ", ")))
}

// ---------- terminal input ----------

// readKey reads one keypress without waiting for Enter.
func readKey() byte {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return '\r'
	}
	defer term.Restore(fd, old)
	buf := make([]byte, 8) // arrow keys etc. arrive as multi-byte sequences; only the first byte matters
	n, err := os.Stdin.Read(buf)
	if err != nil || n == 0 {
		return '\r'
	}
	return buf[0]
}

func readLine(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i != -1 {
		return s[:i] + c("2", " …")
	}
	return s
}

func report(err error, ok string) {
	if err != nil {
		warn("%v", err)
	} else {
		fmt.Fprintln(os.Stderr, "  "+c("32", "✓ "+ok))
	}
}

// ---------- clipboard ----------

func clipboardCmds() (copyCmd, pasteCmd []string) {
	switch {
	case runtime.GOOS == "darwin":
		return []string{"pbcopy"}, []string{"pbpaste"}
	case runtime.GOOS == "windows":
		return []string{"clip"}, []string{"powershell", "-NoProfile", "-Command", "Get-Clipboard"}
	case os.Getenv("WAYLAND_DISPLAY") != "" && has("wl-copy"):
		return []string{"wl-copy"}, []string{"wl-paste", "-n"}
	case has("xclip"):
		return []string{"xclip", "-selection", "clipboard"}, []string{"xclip", "-selection", "clipboard", "-o"}
	case has("xsel"):
		return []string{"xsel", "--clipboard", "--input"}, []string{"xsel", "--clipboard", "--output"}
	}
	return nil, nil
}

func has(bin string) bool { _, err := exec.LookPath(bin); return err == nil }

// copyText puts text on the clipboard (via stdin, never argv). Sensitive text is cleared after
// clipClearAfter by a background `mem` that only knows its hash.
func copyText(text string, sensitive bool) error {
	cp, _ := clipboardCmds()
	if cp == nil {
		return fmt.Errorf("no clipboard tool found (install wl-clipboard, xclip or xsel)")
	}
	cmd := exec.Command(cp[0], cp[1:]...)
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("copy failed: %v", err)
	}
	if sensitive {
		if self, err := os.Executable(); err == nil {
			clear := exec.Command(self, "__clipclear")
			sum := sha256.Sum256([]byte(text))
			clear.Env = append(os.Environ(), "MEM_CLIP_HASH="+hex.EncodeToString(sum[:]))
			clear.Start() // detached; outlives this process
		}
	}
	return nil
}

// cmdClipClear (hidden): wait, then empty the clipboard if it still holds the copied secret.
func cmdClipClear([]string) {
	want := os.Getenv("MEM_CLIP_HASH")
	if d := os.Getenv("MEM_CLIP_AFTER"); d != "" { // for tests
		if dur, err := time.ParseDuration(d); err == nil {
			time.Sleep(dur)
		}
	} else {
		time.Sleep(clipClearAfter)
	}
	cp, paste := clipboardCmds()
	if cp == nil || want == "" {
		return
	}
	cur, err := exec.Command(paste[0], paste[1:]...).Output()
	if err != nil {
		return
	}
	sum := sha256.Sum256([]byte(strings.TrimRight(string(cur), "\r\n")))
	sum2 := sha256.Sum256(cur)
	if hex.EncodeToString(sum[:]) == want || hex.EncodeToString(sum2[:]) == want {
		cmd := exec.Command(cp[0], cp[1:]...)
		cmd.Stdin = strings.NewReader("")
		cmd.Run()
	}
}
