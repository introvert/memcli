package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// LLM is the one thing mem needs from a model: system + user text in, text out.
// If out is non-nil, text is streamed to it as it arrives.
type LLM interface {
	Complete(ctx context.Context, system, user string, small bool, out io.Writer) (string, error)
}

func newLLM(cfg Config, spec string) (LLM, error) {
	p, model, err := cfg.resolveModel(spec)
	if err != nil {
		return nil, err
	}
	switch p.API {
	case "anthropic":
		return &claudeLLM{client: anthropic.NewClient(), model: model}, nil
	case "openai":
		key := ""
		if p.APIKeyEnv != "" {
			if key = os.Getenv(p.APIKeyEnv); key == "" {
				return nil, fmt.Errorf("%s is not set (needed for %s)", p.APIKeyEnv, spec)
			}
		}
		return &openaiLLM{baseURL: strings.TrimRight(p.BaseURL, "/"), key: key, model: model,
			options: p.Options, smallOptions: p.SmallOptions}, nil
	}
	return nil, fmt.Errorf("provider api %q not supported (use \"anthropic\" or \"openai\")", p.API)
}

// ---------- Claude (native Anthropic SDK) ----------

type claudeLLM struct {
	client anthropic.Client
	model  string
}

// Effort is accepted by Opus/Sonnet 4.6+ and Fable/Mythos; older models and Haiku reject it.
var effortModels = regexp.MustCompile(`^claude-(fable|mythos|opus|sonnet)-([5-9]|4-[6-9])`)

// Models that support server-side refusal fallbacks ("default" routing).
var fallbackModels = map[string]bool{
	"claude-fable-5-1": true, "claude-opus-5-5": true, "claude-opus-5": true, "claude-sonnet-5-5": true,
}

func (c *claudeLLM) Complete(ctx context.Context, system, user string, small bool, out io.Writer) (string, error) {
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: 16000,
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(user))},
	}
	if effortModels.MatchString(c.model) {
		effort := anthropic.BetaOutputConfigEffortMedium
		if small {
			effort = anthropic.BetaOutputConfigEffortLow
		}
		params.OutputConfig = anthropic.BetaOutputConfigParam{Effort: effort}
	}
	if fallbackModels[c.model] {
		// If a safety classifier declines, the API re-serves the request on a suitable model.
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}

	stream := c.client.Beta.Messages.NewStreaming(ctx, params)
	msg := anthropic.BetaMessage{}
	for stream.Next() {
		ev := stream.Current()
		msg.Accumulate(ev)
		if out == nil {
			continue
		}
		if d, ok := ev.AsAny().(anthropic.BetaRawContentBlockDeltaEvent); ok {
			if t, ok := d.Delta.AsAny().(anthropic.BetaTextDelta); ok {
				fmt.Fprint(out, t.Text)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return "", err
	}
	if msg.StopReason == anthropic.BetaStopReasonRefusal {
		return "", fmt.Errorf("model declined the request (%s)", msg.StopDetails.Category)
	}
	var sb strings.Builder
	for _, b := range msg.Content {
		if t, ok := b.AsAny().(anthropic.BetaTextBlock); ok {
			sb.WriteString(t.Text)
		}
	}
	return sb.String(), nil
}

// ---------- everything else (OpenAI-compatible chat completions) ----------

type openaiLLM struct {
	baseURL, key, model   string
	options, smallOptions map[string]any
}

func (o *openaiLLM) Complete(ctx context.Context, system, user string, small bool, out io.Writer) (string, error) {
	req := map[string]any{}
	merge(req, o.options)
	if small {
		merge(req, o.smallOptions)
	}
	req["model"], req["stream"] = o.model, true
	req["messages"] = []map[string]string{
		{"role": "system", "content": system},
		{"role": "user", "content": user},
	}
	body, _ := json.Marshal(req)
	hreq, err := http.NewRequestWithContext(ctx, "POST", o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if o.key != "" {
		hreq.Header.Set("Authorization", "Bearer "+o.key)
	}
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Error != nil {
			return "", fmt.Errorf("%s", chunk.Error.Message)
		}
		for _, ch := range chunk.Choices {
			sb.WriteString(ch.Delta.Content)
			if out != nil {
				fmt.Fprint(out, ch.Delta.Content)
			}
		}
	}
	return sb.String(), sc.Err()
}

// merge copies src into dst; a nil value deletes the key.
func merge(dst, src map[string]any) {
	for k, v := range src {
		if v == nil {
			delete(dst, k)
		} else {
			dst[k] = v
		}
	}
}

// ---------- prompts ----------

const askSystem = `You answer questions using the user's personal notes, provided in <note> tags
(either all of their notes, or the ones a search found).
Answer directly and concisely in plain text suitable for a terminal (no markdown headings).
After each fact you use, cite its note id exactly as given, in square brackets, e.g. [mem/261025-143012]
or [Projects/Ideas].
If the notes don't contain the answer, say so plainly instead of guessing.`

const expandSystem = `You turn a question into search keywords for a full-text search over personal notes.
Reply with 5-12 single-word keywords: the important words from the question plus likely synonyms and
related terms someone might have used when writing the note. Space-separated, lowercase, nothing else.`

const tagSystem = `You pick tags for a personal note. Reply with 1-3 short lowercase tags, comma-separated,
nothing else. Strongly prefer tags from the existing list when one fits; invent a new one only if none do.`

var citeRe = regexp.MustCompile(`\[([^\[\]\n]+)\]`)

// cited returns the notes whose ids appear in the answer, in the order first cited.
func cited(answer string, hits []Hit) []Note {
	byID := map[string]Note{}
	for _, h := range hits {
		byID[h.ID] = h.Note
	}
	out, seen := []Note{}, map[string]bool{}
	for _, m := range citeRe.FindAllStringSubmatch(answer, -1) {
		for _, id := range strings.Split(m[1], ",") { // "[a, b]" cites two notes
			id = strings.TrimSpace(id)
			if n, ok := byID[id]; ok && !seen[id] {
				seen[id] = true
				out = append(out, n)
			}
		}
	}
	return out
}

func notesContext(hits []Hit) string {
	const perNote = 6000 // chars; very long notes are cut with a visible marker
	var sb strings.Builder
	for _, h := range hits {
		body := h.Body
		if r := []rune(body); len(r) > perNote {
			body = string(r[:perNote]) + "\n[…note truncated]"
		}
		fmt.Fprintf(&sb, "<note id=%q created=%q tags=%q>\n%s\n</note>\n", h.ID, h.Created, strings.Join(h.Tags, " "), body)
	}
	return sb.String()
}

func parseTags(s string) []string {
	var tags []string
	for _, t := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
		t = strings.ToLower(strings.Trim(strings.TrimSpace(t), "#`\"'."))
		if t != "" && !strings.ContainsAny(t, " \t") && len(t) <= 30 {
			tags = append(tags, t)
		}
	}
	if len(tags) > 3 {
		tags = tags[:3]
	}
	return tags
}
