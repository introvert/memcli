package main

import (
	"io"
	"strings"
)

// render formats a streamed markdown answer for the terminal as it arrives:
// `values` in color without backticks, ``` blocks indented, [note-id] citations dimmed and
// [REDACTED:kind#n] placeholders as ‹kind›. Without color (pipes, NO_COLOR) text passes through
// unchanged, so `mem ask ... | something` still gets plain markdown.
type render struct {
	w         io.Writer
	pending   string // text held back until it can be classified
	lineStart bool
	inFence   bool
	inCode    bool
}

func newRender(w io.Writer) io.Writer {
	if w == nil || !color {
		return w
	}
	return &render{w: w, lineStart: true}
}

func (r *render) Write(p []byte) (int, error) {
	r.pending += string(p)
	r.drain(false)
	return len(p), nil
}

// Flush writes whatever is still held back (end of a model turn).
func (r *render) Flush() {
	r.drain(true)
	if r.inCode {
		io.WriteString(r.w, "\x1b[0m")
		r.inCode = false
	}
}

func (r *render) drain(final bool) {
	for r.pending != "" {
		s := r.pending
		// Fenced code: handled a whole line at a time.
		if r.inFence || (r.lineStart && strings.HasPrefix(s, "```")) {
			nl := strings.IndexByte(s, '\n')
			if nl == -1 {
				if !final {
					return
				}
				nl = len(s) - 1
			}
			line := strings.TrimRight(s[:nl+1], "\n")
			r.pending = s[nl+1:]
			r.lineStart = true
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				r.inFence = !r.inFence
				continue
			}
			io.WriteString(r.w, "    "+c("36", line)+"\n")
			continue
		}
		if r.lineStart && len(s) < 3 && strings.Trim(s, "`") == "" && !final {
			return // could be the start of a ``` fence
		}
		ch := s[0]
		switch {
		case ch == '`':
			r.inCode = !r.inCode
			if r.inCode {
				io.WriteString(r.w, "\x1b[36m")
			} else {
				io.WriteString(r.w, "\x1b[0m")
			}
			r.pending = s[1:]
		case ch == '[':
			end := strings.IndexAny(s, "]\n")
			if end == -1 && len(s) < 120 && !final {
				return // wait for the closing bracket
			}
			if end == -1 || s[end] != ']' {
				io.WriteString(r.w, "[")
				r.pending = s[1:]
				break
			}
			inner := s[1:end]
			if m := placeholderRe.FindStringSubmatch(s[:end+1]); m != nil {
				io.WriteString(r.w, c("33", "‹"+m[1]+"›"))
			} else if !strings.ContainsAny(inner, " \t") || strings.Contains(inner, "/") {
				io.WriteString(r.w, c("2", "["+inner+"]"))
			} else {
				io.WriteString(r.w, "["+inner+"]")
			}
			r.pending = s[end+1:]
		default:
			// Plain text up to the next character that needs attention.
			i := strings.IndexAny(s, "`[\n")
			if i == -1 {
				i = len(s)
			}
			if s[0] == '\n' {
				i = 1
			}
			io.WriteString(r.w, s[:i])
			r.lineStart = strings.HasSuffix(s[:i], "\n")
			r.pending = s[i:]
			continue
		}
		r.lineStart = false
	}
}
