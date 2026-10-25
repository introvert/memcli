package main

import (
	_ "embed"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Everything a model sees (tool output, notes for tagging) and everything in the search index
// passes through redact first, so secrets never leave the machine and can't be probed by search.
// `mem show` / `mem edit` read the files directly and are never redacted.

// Bump when the rules change: the index is rebuilt with the new rules.
const redactVersion = "6"

//go:embed bip39.txt
var bip39Raw string

var bip39 = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(bip39Raw) {
		m[w] = true
	}
	return m
}()

type rule struct {
	kind string
	re   *regexp.Regexp
	// group is the submatch to replace (0 = whole match); the rest of the match is kept.
	group int
}

var rules = []rule{
	{"private-key", regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----.*?-----END [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----`), 0},
	// scheme://user:password@host — keep the user, drop the password
	{"password", regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:([^/\s@]+)@`), 1},
	// well-known token formats
	{"aws-key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), 0},
	{"github-token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})\b`), 0},
	{"gitlab-token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`), 0},
	{"slack-token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`), 0},
	{"slack-webhook", regexp.MustCompile(`https://hooks\.slack\.com/services/[A-Za-z0-9/]+`), 0},
	{"stripe-key", regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}\b|\bwhsec_[A-Za-z0-9]{20,}\b`), 0},
	{"api-key", regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}\b`), 0},
	{"google-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b|\bya29\.[0-9A-Za-z_-]{20,}`), 0},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), 0},
	{"sendgrid-key", regexp.MustCompile(`\bSG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`), 0},
	{"package-token", regexp.MustCompile(`\b(?:npm_[A-Za-z0-9]{36}|pypi-[A-Za-z0-9_-]{50,}|hf_[A-Za-z0-9]{30,})\b`), 0},
	{"telegram-token", regexp.MustCompile(`\b\d{8,10}:AA[A-Za-z0-9_-]{33}\b`), 0},
	{"bearer-token", regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/-]{16,}=*)`), 1},
	// crypto: hex private keys (also hits 64-hex hashes — acceptable) and Bitcoin WIF keys
	{"crypto-key", regexp.MustCompile(`\b(?:0x)?[a-fA-F0-9]{64}\b`), 0},
	{"crypto-key", regexp.MustCompile(`\b[5KL][1-9A-HJ-NP-Za-km-z]{50,51}\b`), 0},
	// seed phrase after a label: the rest of the line
	{"seed-phrase", regexp.MustCompile(`(?im)\b(?:seed(?:\s*phrase)?|mnemonic|recovery\s*(?:phrase|words?)|backup\s*phrase)\b\s*(?:[:=|-]|is|je)\s*(\S.*)$`), 1},
	// "password: x", "api_key=x", "| geslo | x |", "token is x" — the value after a secret-ish label
	{"secret", regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|pass|passphrase|passcode|pin|secret|client[_ -]?secret|api[_ -]?key|apikey|access[_ -]?key|secret[_ -]?key|private[_ -]?key|auth[_ -]?token|access[_ -]?token|refresh[_ -]?token|token|credentials?|geslo|gesla|žeton)\b(?:\s+[\p{L}\p{N}_.-]{1,24}){0,3}?["']?\s*(?:[:=|]|=>|\bis\b|\bje\b)\s*["']?([^\s"'|,;]{3,})`), 1},
}

type Finding struct {
	Kind, Preview string
	Line          int
	Value         string // the secret itself: only ever used locally (copy/run), never printed or sent
}

// placeholderRe matches what redact leaves behind: [REDACTED:kind#n], n = nth secret in the note.
var placeholderRe = regexp.MustCompile(`\[REDACTED:([a-z-]+)#(\d+)\]`)

// redact returns text with secrets replaced by numbered placeholders, plus what it found
// (findings[n-1] is placeholder #n).
func redact(text string, extra []*regexp.Regexp) (string, []Finding) {
	var found []Finding
	mark := func(kind, secret string, at, n int) string {
		found = append(found, Finding{Kind: kind, Preview: mask(secret), Line: 1 + strings.Count(text[:at], "\n"), Value: secret})
		return fmt.Sprintf("[REDACTED:%s#%d]", kind, n)
	}
	// Collect spans on the original text, then replace right-to-left so offsets stay valid.
	var spans []span
	for _, r := range rules {
		for _, m := range r.re.FindAllStringSubmatchIndex(text, -1) {
			s, e := m[2*r.group], m[2*r.group+1]
			if s < 0 || strings.HasPrefix(text[s:e], "[REDACTED") {
				continue
			}
			spans = append(spans, span{s, e, r.kind})
		}
	}
	for _, re := range extra {
		for _, m := range re.FindAllStringIndex(text, -1) {
			spans = append(spans, span{m[0], m[1], "custom"})
		}
	}
	spans = append(spans, seedSpans(text)...)
	spans = append(spans, entropySpans(text)...)
	spans = append(spans, cardSpans(text)...)

	// Merge overlaps (keep the earliest-starting, longest span).
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end > spans[j].end
	})
	var merged []span
	for _, s := range spans {
		if n := len(merged); n > 0 && s.start < merged[n-1].end {
			if s.end > merged[n-1].end {
				merged[n-1].end = s.end
			}
			continue
		}
		merged = append(merged, s)
	}
	out := text
	for i := len(merged) - 1; i >= 0; i-- {
		s := merged[i]
		out = out[:s.start] + mark(s.kind, text[s.start:s.end], s.start, i+1) + out[s.end:]
	}
	// found was filled right-to-left
	for i, j := 0, len(found)-1; i < j; i, j = i+1, j-1 {
		found[i], found[j] = found[j], found[i]
	}
	return out, found
}

type span struct {
	start, end int
	kind       string
}

var seedWordRe = regexp.MustCompile(`[a-z]+`)

// seedSpans finds BIP-39 seed phrases written without a label: 12+ wordlist words in a row,
// on one line or one word per line (optionally numbered "1. word").
func seedSpans(text string) []span {
	var out []span
	words := seedWordRe.FindAllStringIndex(text, -1)
	start := -1 // first word of the current run of wordlist words
	flush := func(end int) {
		if start >= 0 && end-start+1 >= 12 {
			out = append(out, span{words[start][0], words[end][1], "seed-phrase"})
		}
	}
	for i, w := range words {
		inList := bip39[text[w[0]:w[1]]]
		if inList && start >= 0 && onlySeparators(text[words[i-1][1]:w[0]]) {
			continue // run goes on
		}
		flush(i - 1)
		start = -1
		if inList {
			start = i
		}
	}
	flush(len(words) - 1)
	return out
}

func onlySeparators(s string) bool {
	for _, r := range s {
		if !(unicode.IsSpace(r) || unicode.IsDigit(r) || r == '.' || r == ')' || r == ',' || r == '-') {
			return false
		}
	}
	return true
}

var (
	tokenCandRe = regexp.MustCompile(`[A-Za-z0-9_\-+=]{24,}`)
	urlRe       = regexp.MustCompile(`(?i)\b(?:https?|ftp)://[^\s<>"'\x60)\]]+`)
)

// entropySpans catches unknown-format keys: 24-100 char tokens that mix letters and digits randomly.
// Tokens inside URLs (doc ids, tracking params) are skipped; secrets in URLs are caught by the
// labelled rules (?api_key=..., ?token=..., user:pass@).
func entropySpans(text string) []span {
	var out []span
	urls := urlRe.FindAllStringIndex(text, -1)
	inURL := func(s, e int) bool {
		for _, u := range urls {
			if s < u[1] && e > u[0] {
				return true
			}
		}
		return false
	}
	for _, m := range tokenCandRe.FindAllStringIndex(text, -1) {
		tok := text[m[0]:m[1]]
		if len(tok) > 100 || inURL(m[0], m[1]) {
			continue
		}
		var lower, upper, digit bool
		for _, r := range tok {
			lower = lower || unicode.IsLower(r)
			upper = upper || unicode.IsUpper(r)
			digit = digit || unicode.IsDigit(r)
		}
		if digit && (lower || upper) && (lower && upper || len(tok) >= 32) && entropy(tok) >= 3.6 {
			out = append(out, span{m[0], m[1], "token"})
		}
	}
	return out
}

func entropy(s string) float64 {
	counts := map[rune]float64{}
	for _, r := range s {
		counts[r]++
	}
	h, n := 0.0, float64(len(s))
	for _, c := range counts {
		h -= c / n * math.Log2(c/n)
	}
	return h
}

var cardRe = regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`)

// cardSpans finds payment card numbers (13-19 digits passing the Luhn check).
func cardSpans(text string) []span {
	var out []span
	for _, m := range cardRe.FindAllStringIndex(text, -1) {
		digits := strings.Map(func(r rune) rune {
			if unicode.IsDigit(r) {
				return r
			}
			return -1
		}, text[m[0]:m[1]])
		if len(digits) >= 13 && luhn(digits) {
			out = append(out, span{m[0], m[1], "card-number"})
		}
	}
	return out
}

func luhn(d string) bool {
	sum := 0
	for i := range d {
		n := int(d[len(d)-1-i] - '0')
		if i%2 == 1 {
			if n *= 2; n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return sum%10 == 0
}

func mask(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= 4 {
		return strings.Repeat("•", len(r))
	}
	return string(r[:2]) + strings.Repeat("•", min(len(r)-2, 8)) + fmt.Sprintf(" (%d chars)", len(r))
}

// compileExtra compiles the user's extra redaction patterns from config.
func compileExtra(patterns []string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			die("bad redact pattern %q: %v", p, err)
		}
		out = append(out, re)
	}
	return out
}
