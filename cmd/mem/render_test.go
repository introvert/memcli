package main

import (
	"regexp"
	"strings"
	"testing"
)

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

// renderAll streams s through the renderer in chunks of n bytes (n=1 is the hardest case).
func renderAll(s string, n int) string {
	old := color
	color = true
	defer func() { color = old }()
	var b strings.Builder
	w := newRender(&b)
	for i := 0; i < len(s); i += n {
		w.Write([]byte(s[i:min(i+n, len(s))]))
	}
	w.(*render).Flush()
	return b.String()
}

func TestRenderTerminal(t *testing.T) {
	in := "The adidas account ID is `73054` [mem/261026-003214].\n" +
		"Password: [REDACTED:secret#2]\n\n```sh\npsql \"postgres://app:[REDACTED:password#3]@db/app\"\n```\nDone [not a citation]."
	want := "The adidas account ID is 73054 [mem/261026-003214].\n" +
		"Password: ‹secret›\n\n    psql \"postgres://app:[REDACTED:password#3]@db/app\"\nDone [not a citation]."
	for _, n := range []int{1, 3, 7, len(in)} {
		got := ansiRe.ReplaceAllString(renderAll(in, n), "")
		if got != want {
			t.Errorf("chunk %d:\n got: %q\nwant: %q", n, got, want)
		}
	}
	if !strings.Contains(renderAll("a `b` c", 1), "\x1b[36mb\x1b[0m") {
		t.Error("inline code not colored")
	}
}

func TestRenderPassThroughWithoutColor(t *testing.T) {
	var b strings.Builder
	old := color
	color = false
	defer func() { color = old }()
	w := newRender(&b)
	w.Write([]byte("`x` [a/b] ```"))
	if b.String() != "`x` [a/b] ```" {
		t.Errorf("plain output changed: %q", b.String())
	}
}

func TestClosest(t *testing.T) {
	vocab := []vterm{{"adida", 3}, {"account", 9}, {"adapt", 1}, {"parking", 4}}
	cases := map[string]string{"addidas": "adida", "acount": "account", "parkign": "parking"}
	for in, want := range cases {
		if got := closest(in, vocab, 1); len(got) != 1 || got[0] != want {
			t.Errorf("closest(%q) = %v, want %s", in, got, want)
		}
	}
	if got := closest("zzzzzz", vocab, 2); len(got) != 0 {
		t.Errorf("unrelated word corrected to %v", got)
	}
}
