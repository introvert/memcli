package main

import (
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

// Tool is a function the model may call; Params is a JSON Schema object.
type Tool struct {
	Name, Description string
	Params            map[string]any
}

type ToolCall struct{ ID, Name, Args string }

// Chat is one conversation with a model, kept in the provider's native message format.
type Chat interface {
	// Next sends the conversation so far. It returns the model's text and any tool calls it wants
	// run; no calls means the model is done. allowTools=false makes the model answer now.
	Next(ctx context.Context, allowTools bool) (text string, calls []ToolCall, err error)
	// AddResults appends the outputs of the calls from the last Next, in the same order.
	AddResults(calls []ToolCall, results []string)
}

// newChat starts a conversation. small=true is for quick jobs (tagging): less model effort.
func newChat(cfg Config, spec, system, user string, tools []Tool, small bool) (Chat, error) {
	p, model, err := cfg.resolveModel(spec)
	if err != nil {
		return nil, err
	}
	switch p.API {
	case "anthropic":
		return newClaudeChat(model, system, user, tools, small), nil
	case "openai":
		key := ""
		if p.APIKeyEnv != "" {
			if key = os.Getenv(p.APIKeyEnv); key == "" {
				return nil, fmt.Errorf("%s is not set (needed for %s)", p.APIKeyEnv, spec)
			}
		}
		opts := map[string]any{}
		merge(opts, p.Options)
		if small {
			merge(opts, p.SmallOptions)
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
func complete(ctx context.Context, cfg Config, spec, system, user string, small bool) (string, error) {
	chat, err := newChat(cfg, spec, system, user, nil, small)
	if err != nil {
		return "", err
	}
	text, _, err := chat.Next(ctx, false)
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

func newClaudeChat(model, system, user string, tools []Tool, small bool) *claudeChat {
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: 16000,
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
		effort := anthropic.BetaOutputConfigEffortMedium
		if small {
			effort = anthropic.BetaOutputConfigEffortLow
		}
		params.OutputConfig = anthropic.BetaOutputConfigParam{Effort: effort}
	}
	if fallbackModels[model] {
		// If a safety classifier declines, the API re-serves the request on a suitable model.
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}
	return &claudeChat{client: anthropic.NewClient(), params: params}
}

func (c *claudeChat) Next(ctx context.Context, allowTools bool) (string, []ToolCall, error) {
	p := c.params
	if !allowTools && len(p.Tools) > 0 {
		p.ToolChoice = anthropic.BetaToolChoiceUnionParam{OfNone: &anthropic.BetaToolChoiceNoneParam{}}
	}
	resp, err := c.client.Beta.Messages.New(ctx, p)
	if err != nil {
		return "", nil, err
	}
	if resp.StopReason == anthropic.BetaStopReasonRefusal {
		return "", nil, fmt.Errorf("model declined the request (%s)", resp.StopDetails.Category)
	}
	// Keep the full assistant turn (incl. thinking blocks) in history, unchanged.
	c.params.Messages = append(c.params.Messages, resp.ToParam())
	var text strings.Builder
	var calls []ToolCall
	for _, b := range resp.Content {
		switch v := b.AsAny().(type) {
		case anthropic.BetaTextBlock:
			text.WriteString(v.Text)
		case anthropic.BetaToolUseBlock:
			calls = append(calls, ToolCall{ID: v.ID, Name: v.Name, Args: v.JSON.Input.Raw()})
		}
	}
	return text.String(), calls, nil
}

func (c *claudeChat) AddResults(calls []ToolCall, results []string) {
	var blocks []anthropic.BetaContentBlockParamUnion
	for i, call := range calls {
		blocks = append(blocks, anthropic.NewBetaToolResultBlock(call.ID, results[i], false))
	}
	// All results of one turn go back in a single user message.
	c.params.Messages = append(c.params.Messages, anthropic.NewBetaUserMessage(blocks...))
}

// ---------- everything else (OpenAI-compatible chat completions) ----------

type openaiChat struct {
	url, key, model string
	opts            map[string]any
	tools           []Tool
	msgs            []map[string]any
}

func (o *openaiChat) Next(ctx context.Context, allowTools bool) (string, []ToolCall, error) {
	req := map[string]any{}
	merge(req, o.opts)
	req["model"], req["messages"] = o.model, o.msgs
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
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw[:min(len(raw), 2000)])))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content          string          `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", nil, fmt.Errorf("bad response: %v", err)
	}
	if out.Error != nil {
		return "", nil, fmt.Errorf("%s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", nil, fmt.Errorf("empty response")
	}
	m := out.Choices[0].Message
	var calls []ToolCall
	var parsed []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if len(m.ToolCalls) > 0 && string(m.ToolCalls) != "null" {
		json.Unmarshal(m.ToolCalls, &parsed)
	}
	for _, p := range parsed {
		calls = append(calls, ToolCall{ID: p.ID, Name: p.Function.Name, Args: p.Function.Arguments})
	}
	msg := map[string]any{"role": "assistant", "content": m.Content}
	if len(parsed) > 0 {
		msg["tool_calls"] = m.ToolCalls
	}
	if m.ReasoningContent != "" { // DeepSeek requires it back on later turns when tools are used
		msg["reasoning_content"] = m.ReasoningContent
	}
	o.msgs = append(o.msgs, msg)
	return m.Content, calls, nil
}

func (o *openaiChat) AddResults(calls []ToolCall, results []string) {
	for i, call := range calls {
		o.msgs = append(o.msgs, map[string]any{"role": "tool", "tool_call_id": call.ID, "content": results[i]})
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
