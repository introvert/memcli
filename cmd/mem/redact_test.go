package main

import (
	"regexp"
	"strings"
	"testing"
)

// Fake secrets are assembled at runtime so the repo never contains strings that look like real
// credentials (and secret scanners don't flag the test file).
func rep(s string, n int) string { return strings.Repeat(s, n) }

func TestRedactCatches(t *testing.T) {
	seed := "legal winner thank year wave sausage worth useful legal winner thank yellow"
	cases := []struct{ name, in, secret, kind string }{
		{"password label", "prod db password: hunter2isweak", "hunter2isweak", "secret"},
		{"slovenian label", "geslo za wifi je Kava123!", "Kava123!", "secret"},
		{"table row", "| password | s3cr3tP4ss |", "s3cr3tP4ss", "secret"},
		{"api_key assignment", `API_KEY="abc123def456"`, "abc123def456", "secret"},
		{"words between", "password for gmail: Gm4ilPass", "Gm4ilPass", "secret"},
		{"token is", "the token is qwerty98765", "qwerty98765", "secret"},
		{"url credentials", "postgres://app:Sup3rS3cret@db.internal:5432/x", "Sup3rS3cret", "password"},
		{"aws", "key AKIA" + rep("ABCD", 4) + " here", "AKIA" + rep("ABCD", 4), "aws-key"},
		{"github", "gh" + "p_" + rep("aB3x", 9), "gh" + "p_" + rep("aB3x", 9), "github-token"},
		{"openai-style", "sk-" + "proj-" + rep("Ab1_", 8), "sk-" + "proj-" + rep("Ab1_", 8), "api-key"},
		{"anthropic", "sk-" + "ant-" + rep("x9Y-", 8), "sk-" + "ant-" + rep("x9Y-", 8), "api-key"},
		{"slack", "xo" + "xb-" + rep("1234-", 3) + "abcdEFGH", "xo" + "xb-" + rep("1234-", 3) + "abcdEFGH", "slack-token"},
		{"stripe", "s" + "k_live_" + rep("Zz9", 8), "s" + "k_live_" + rep("Zz9", 8), "stripe-key"},
		{"google", "AI" + "za" + rep("Sy0_x", 7), "AI" + "za" + rep("Sy0_x", 7), "google-key"},
		{"jwt", "eyJ" + rep("hbGci", 3) + ".eyJ" + rep("zdWIi", 3) + "." + rep("SflKx", 3), "eyJ" + rep("hbGci", 3), "jwt"},
		{"bearer", "Authorization: Bearer abcDEF123456ghiJKL789", "abcDEF123456ghiJKL789", ""},
		{"pem", "-----BEGIN RSA PRIVATE KEY-----\nMIIE" + rep("q", 40) + "\n-----END RSA PRIVATE KEY-----", "MIIE", "private-key"},
		{"eth key", "wallet key 0x" + rep("4c0883a69102937d", 4), rep("4c0883a69102937d", 4), "crypto-key"},
		{"labelled seed", "Seed phrase: alpha bravo charlie delta", "alpha bravo charlie delta", "seed-phrase"},
		{"bare seed", "backup:\n" + seed + "\n", seed, "seed-phrase"},
		{"numbered seed", "1. " + strings.ReplaceAll(seed, " ", "\n1. "), "sausage", "seed-phrase"},
		{"high entropy", "export X=" + "q8Ff2LmZ0vT9wKc4Rb7nYp1sHd6Ue3Ja", "q8Ff2LmZ0vT9wKc4Rb7nYp1sHd6Ue3Ja", ""},
		{"key in url param", "https://api.example.com/v1?api_key=Zx81Kq55Lm&x=1", "Zx81Kq55Lm", "secret"},
		{"card", "card 4111 1111 1111 1111 exp 12/29", "4111 1111 1111 1111", "card-number"},
	}
	for _, c := range cases {
		out, found := redact(c.in, nil)
		if strings.Contains(out, c.secret) {
			t.Errorf("%s: secret leaked:\n  in:  %q\n  out: %q", c.name, c.in, out)
		}
		if len(found) == 0 {
			t.Errorf("%s: no finding reported", c.name)
		}
		if c.kind != "" && !strings.Contains(out, "[REDACTED:"+c.kind+"]") {
			t.Errorf("%s: want kind %s, got %q", c.name, c.kind, out)
		}
	}
}

func TestRedactKeepsOrdinaryText(t *testing.T) {
	keep := []string{
		"parked at airport garage level 3, spot C14",
		"meeting with Ana on 2026-03-14 at 10:00, call +386 40 123 456",
		"see https://github.com/introvert/memcli/blob/main/README.md",
		"IBAN SI56 1910 0000 0123 438",
		"invoice 2026-0042 total 1.250,00 EUR",
		"forgot the password again, reset it via the admin panel", // label without a value
		"config file ~/code/notes/Kundix/Accounting/invoices-2026.md",
		"the quick brown fox jumps over the lazy dog every single day of the week",
		"order id 123456789012", // not Luhn-valid as a card, too short anyway
		"kebab-case-identifiers-are-fine-here-too",
		"sheet https://docs.google.com/spreadsheets/d/1aB2cD3eF4gH5iJ6kL7mN8oP9qR0sT1uV2wX3yZ/edit",
		"https://www.amazon.de/dp/B0C1X2Y3Z4?pd_rd_wg=Gh7Zv&pd_rd_r=4f8e2a1b-9c3d-4e5f-8a7b-6c5d4e3f2a1b",
		"blob " + rep("AbC123xYz_", 15), // >100 chars: encoded data, not a key
	}
	for _, s := range keep {
		if out, found := redact(s, nil); out != s || len(found) > 0 {
			t.Errorf("over-redacted:\n  in:  %q\n  out: %q", s, out)
		}
	}
}

func TestRedactExtraPatternsAndLines(t *testing.T) {
	text := "line one\ncustomer PIN-CODE 99887766\nline three password: abcdef"
	out, found := redact(text, []*regexp.Regexp{regexp.MustCompile(`PIN-CODE \d+`)})
	if strings.Contains(out, "99887766") || strings.Contains(out, "abcdef") {
		t.Fatalf("leak: %q", out)
	}
	if len(found) != 2 || found[0].Line != 2 || found[1].Line != 3 {
		t.Fatalf("findings/lines wrong: %+v", found)
	}
	if again, f2 := redact(out, nil); again != out || len(f2) != 0 {
		t.Fatalf("redaction not idempotent: %q / %+v", again, f2)
	}
}
