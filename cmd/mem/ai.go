package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// Tool is a function the model may call; Params is a JSON Schema object.
type Tool struct {
	Name, Description string
	Params            map[string]any
}

type ToolCall struct{ ID, Name, Args string }

// Level is how hard the model should think: speed vs depth.
type Level int

const (
	Quick  Level = iota // tagging and other one-shot jobs
	Normal              // `mem ask`: lookups in notes don't need deep reasoning, so keep it fast
	Deep                // `mem ask -d`: hard questions
)

// Chat is one conversation with a model, kept in the provider's native message format.
type Chat interface {
	// Next sends the conversation so far and streams the model's text to out (if non-nil).
	// It returns the text and any tool calls to run; no calls means the model is done.
	// allowTools=false makes the model answer now.
	Next(ctx context.Context, allowTools bool, out io.Writer) (text string, calls []ToolCall, err error)
	// AddResults appends the outputs of the calls from the last Next, in the same order.
	AddResults(calls []ToolCall, results []string)
	// AddUser appends a follow-up message from the user.
	AddUser(text string)
}

// compactor is implemented by chats that can shrink old tool results, so long tool loops (the
// browser agent sees a whole page per step) don't outgrow the context window.
type compactor interface {
	// Compact cuts every tool result except the last keep to its first head bytes plus a note.
	Compact(keep, head int)
}

const compactNote = "\n[older page state omitted]"

func cut(s string, head int) string {
	if len(s) <= head+len(compactNote) || strings.HasSuffix(s, compactNote) {
		return s
	}
	return clip(s, head) + compactNote
}

func newChat(cfg Config, spec, system, user string, tools []Tool, level Level) (Chat, error) {
	p, model, err := cfg.resolveModel(spec)
	if err != nil {
		return nil, err
	}
	key, _, err := apiKey(p)
	if err != nil {
		return nil, err
	}
	switch p.API {
	case "anthropic":
		// No key found: let the SDK use its own sources (e.g. an `ant auth login` profile).
		return newClaudeChat(key, model, system, user, tools, level), nil
	case "openai":
		if key == "" && (p.APIKeyEnv != "" || p.APIKeyCmd != "") {
			return nil, fmt.Errorf("no API key for %s: run `mem key set %s` (or set %s)", p.Name, p.Name, p.APIKeyEnv)
		}
		opts := map[string]any{}
		merge(opts, p.Options)
		switch level {
		case Quick:
			merge(opts, p.SmallOptions)
		case Deep:
			merge(opts, p.DeepOptions)
		}
		return &openaiChat{
			url: strings.TrimRight(p.BaseURL, "/") + "/chat/completions", key: key, model: model,
			opts: opts, tools: tools,
			msgs: []map[string]any{{"role": "system", "content": system}, {"role": "user", "content": user}},
		}, nil
	}
	return nil, fmt.Errorf("provider api %q not supported (use \"anthropic\" or \"openai\")", p.API)
}

// complete is a single tool-less request.
func complete(ctx context.Context, cfg Config, spec, system, user string, level Level) (string, error) {
	chat, err := newChat(cfg, spec, system, user, nil, level)
	if err != nil {
		return "", err
	}
	text, _, err := chat.Next(ctx, false, nil)
	return text, err
}

// ---------- Claude (native Anthropic SDK) ----------

type claudeChat struct {
	client anthropic.Client
	params anthropic.BetaMessageNewParams
}

// Effort is accepted by Opus/Sonnet 4.6+ and Fable/Mythos; older models and Haiku reject it.
var effortModels = regexp.MustCompile(`^claude-(fable|mythos|opus|sonnet)-([5-9]|4-[6-9])`)

// Models that support server-side refusal fallbacks ("default" routing).
var fallbackModels = map[string]bool{
	"claude-fable-5-1": true, "claude-opus-5-5": true, "claude-opus-5": true, "claude-sonnet-5-5": true,
}

func newClaudeChat(key, model, system, user string, tools []Tool, level Level) *claudeChat {
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: 32000,
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(user))},
	}
	for _, t := range tools {
		props, _ := t.Params["properties"].(map[string]any)
		req, _ := t.Params["required"].([]string)
		params.Tools = append(params.Tools, anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.BetaToolInputSchemaParam{Properties: props, Required: req},
		}})
	}
	if effortModels.MatchString(model) {
		effort := anthropic.BetaOutputConfigEffortLow
		if level == Deep {
			effort = anthropic.BetaOutputConfigEffortHigh
		}
		params.OutputConfig = anthropic.BetaOutputConfigParam{Effort: effort}
	}
	if fallbackModels[model] {
		// If a safety classifier declines, the API re-serves the request on a suitable model.
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}
	var opts []option.RequestOption
	if key != "" {
		opts = append(opts, option.WithAPIKey(key))
	}
	return &claudeChat{client: anthropic.NewClient(opts...), params: params}
}

func (c *claudeChat) Next(ctx context.Context, allowTools bool, out io.Writer) (string, []ToolCall, error) {
	p := c.params
	if !allowTools && len(p.Tools) > 0 {
		p.ToolChoice = anthropic.BetaToolChoiceUnionParam{OfNone: &anthropic.BetaToolChoiceNoneParam{}}
	}
	stream := c.client.Beta.Messages.NewStreaming(ctx, p)
	msg := anthropic.BetaMessage{}
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			return "", nil, err
		}
		if out != nil {
			if d, ok := ev.AsAny().(anthropic.BetaRawContentBlockDeltaEvent); ok {
				if t, ok := d.Delta.AsAny().(anthropic.BetaTextDelta); ok {
					fmt.Fprint(out, t.Text)
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return "", nil, err
	}
	if msg.StopReason == anthropic.BetaStopReasonRefusal {
		return "", nil, fmt.Errorf("model declined the request (%s)", msg.StopDetails.Category)
	}
	// Keep the full assistant turn (incl. thinking blocks) in history, unchanged.
	c.params.Messages = append(c.params.Messages, msg.ToParam())
	var text strings.Builder
	var calls []ToolCall
	for _, b := range msg.Content {
		switch v := b.AsAny().(type) {
		case anthropic.BetaTextBlock:
			text.WriteString(v.Text)
		case anthropic.BetaToolUseBlock:
			calls = append(calls, ToolCall{ID: v.ID, Name: v.Name, Args: v.JSON.Input.Raw()})
		}
	}
	return text.String(), calls, nil
}

func (c *claudeChat) AddUser(text string) {
	c.params.Messages = append(c.params.Messages, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(text)))
}

func (c *claudeChat) AddResults(calls []ToolCall, results []string) {
	var blocks []anthropic.BetaContentBlockParamUnion
	for i, call := range calls {
		blocks = append(blocks, anthropic.NewBetaToolResultBlock(call.ID, results[i], false))
	}
	// All results of one turn go back in a single user message.
	c.params.Messages = append(c.params.Messages, anthropic.NewBetaUserMessage(blocks...))
}

func (c *claudeChat) Compact(keep, head int) {
	seen := 0
	for i := len(c.params.Messages) - 1; i >= 0; i-- {
		for _, b := range c.params.Messages[i].Content {
			if tr := b.OfToolResult; tr != nil {
				if seen++; seen > keep {
					for _, part := range tr.Content {
						if part.OfText != nil {
							part.OfText.Text = cut(part.OfText.Text, head)
						}
					}
				}
			}
		}
	}
}

// ---------- everything else (OpenAI-compatible chat completions) ----------

type openaiChat struct {
	url, key, model string
	opts            map[string]any
	tools           []Tool
	msgs            []map[string]any
}

func (o *openaiChat) Next(ctx context.Context, allowTools bool, out io.Writer) (string, []ToolCall, error) {
	req := map[string]any{}
	merge(req, o.opts)
	req["model"], req["messages"], req["stream"] = o.model, o.msgs, true
	if len(o.tools) > 0 {
		var tools []map[string]any
		for _, t := range o.tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": t.Params,
			}})
		}
		req["tools"] = tools
		if !allowTools {
			req["tool_choice"] = "none"
		}
	}
	body, _ := json.Marshal(req)
	hreq, err := http.NewRequestWithContext(ctx, "POST", o.url, bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if o.key != "" {
		hreq.Header.Set("Authorization", "Bearer "+o.key)
	}
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return "", nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}

	// Server-sent events: text arrives in delta.content, tool calls in pieces keyed by index.
	type callAcc struct{ id, name, args strings.Builder }
	var text, reasoning strings.Builder
	acc := map[int]*callAcc{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		if data = strings.TrimSpace(data); data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
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
			return "", nil, fmt.Errorf("%s", chunk.Error.Message)
		}
		for _, ch := range chunk.Choices {
			d := ch.Delta
			text.WriteString(d.Content)
			reasoning.WriteString(d.ReasoningContent)
			if out != nil && d.Content != "" {
				fmt.Fprint(out, d.Content)
			}
			for _, tc := range d.ToolCalls {
				a := acc[tc.Index]
				if a == nil {
					a = &callAcc{}
					acc[tc.Index] = a
				}
				a.id.WriteString(tc.ID)
				a.name.WriteString(tc.Function.Name)
				a.args.WriteString(tc.Function.Arguments)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", nil, err
	}

	idx := make([]int, 0, len(acc))
	for i := range acc {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	var calls []ToolCall
	var wire []map[string]any
	for _, i := range idx {
		a := acc[i]
		call := ToolCall{ID: a.id.String(), Name: a.name.String(), Args: a.args.String()}
		calls = append(calls, call)
		wire = append(wire, map[string]any{"id": call.ID, "type": "function",
			"function": map[string]any{"name": call.Name, "arguments": call.Args}})
	}
	msg := map[string]any{"role": "assistant", "content": text.String()}
	if len(wire) > 0 {
		msg["tool_calls"] = wire
	}
	if reasoning.Len() > 0 { // DeepSeek requires it back on later turns when tools are used
		msg["reasoning_content"] = reasoning.String()
	}
	o.msgs = append(o.msgs, msg)
	return text.String(), calls, nil
}

func (o *openaiChat) AddUser(text string) {
	o.msgs = append(o.msgs, map[string]any{"role": "user", "content": text})
}

func (o *openaiChat) AddResults(calls []ToolCall, results []string) {
	for i, call := range calls {
		o.msgs = append(o.msgs, map[string]any{"role": "tool", "tool_call_id": call.ID, "content": results[i]})
	}
}

func (o *openaiChat) Compact(keep, head int) {
	seen := 0
	for i := len(o.msgs) - 1; i >= 0; i-- {
		if o.msgs[i]["role"] == "tool" {
			if seen++; seen > keep {
				if c, ok := o.msgs[i]["content"].(string); ok {
					o.msgs[i]["content"] = cut(c, head)
				}
			}
		}
	}
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

// ---------- tagging ----------

const tagSystem = `You pick tags for a personal note. Reply with 1-3 short lowercase tags, comma-separated,
nothing else. Strongly prefer tags from the existing list when one fits; invent a new one only if none do.`

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
