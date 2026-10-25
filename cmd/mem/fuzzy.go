package main

import (
	"database/sql"
	"sort"
	"strings"
	"unicode/utf8"
)

// Typo tolerance: a query word that matches nothing is swapped for the closest words that do
// exist in the notes ("addidas" → "adidas"), using the index's own vocabulary. The vocabulary
// comes from the redacted index, so it never contains secrets.

func ensureVocab(db *sql.DB) {
	db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS vocab USING fts5vocab(fts, 'row')`)
}

// correct returns text with every word that has no match replaced by up to two close matches.
func correct(db *sql.DB, text string) (string, bool) {
	words := tokenRe.FindAllString(strings.ToLower(text), -1)
	changed := false
	var out []string
	var vocab []vterm
	for _, w := range words {
		if utf8.RuneCountInString(w) < 4 || hasMatch(db, w) {
			out = append(out, w)
			continue
		}
		if vocab == nil {
			vocab = loadVocab(db)
		}
		if fixes := closest(w, vocab, 2); len(fixes) > 0 {
			out = append(out, fixes...)
			changed = true
		} else {
			out = append(out, w)
		}
	}
	return strings.Join(out, " "), changed
}

func hasMatch(db *sql.DB, w string) bool {
	var n int
	db.QueryRow(`SELECT count(*) FROM (SELECT 1 FROM fts WHERE fts MATCH ? LIMIT 1)`, `"`+w+`"*`).Scan(&n)
	return n > 0
}

type vterm struct {
	term string
	docs int
}

func loadVocab(db *sql.DB) []vterm {
	ensureVocab(db)
	rows, err := db.Query(`SELECT term, doc FROM vocab WHERE length(term) >= 3`)
	if err != nil {
		return []vterm{}
	}
	defer rows.Close()
	out := []vterm{}
	for rows.Next() {
		var t vterm
		if rows.Scan(&t.term, &t.docs) == nil {
			out = append(out, t)
		}
	}
	return out
}

// closest returns up to n vocabulary terms within a small edit distance, most common first.
// Index terms are stemmed ("adidas" is stored as "adida"), so a stem may also be a prefix of w.
func closest(w string, vocab []vterm, n int) []string {
	maxDist := 1
	if utf8.RuneCountInString(w) >= 6 {
		maxDist = 2
	}
	type cand struct {
		term       string
		dist, docs int
	}
	var cands []cand
	for _, v := range vocab {
		if d := abs(utf8.RuneCountInString(v.term) - utf8.RuneCountInString(w)); d > maxDist+1 {
			continue
		}
		d := levenshtein(w, v.term)
		if strings.HasPrefix(w, v.term) && len(v.term) >= 4 { // stem of a misspelling
			d = min(d, levenshtein(w[:len(v.term)], v.term))
		}
		if d <= maxDist {
			cands = append(cands, cand{v.term, d, v.docs})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].dist != cands[j].dist {
			return cands[i].dist < cands[j].dist
		}
		return cands[i].docs > cands[j].docs
	})
	var out []string
	for i := 0; i < len(cands) && i < n; i++ {
		out = append(out, cands[i].term)
	}
	return out
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
