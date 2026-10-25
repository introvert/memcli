package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Provider describes how to reach a model host. API is "anthropic" (native Claude API)
// or "openai" (the OpenAI-compatible chat completions API that almost every other host speaks).
type Provider struct {
	API       string `json:"api"`
	BaseURL   string `json:"base_url,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// Extra fields merged into every request body (openai api), e.g. {"reasoning_effort": "low"}.
	Options map[string]any `json:"options,omitempty"`
	// Merged on top of Options for small jobs (tagging, query expansion); null removes a key.
	SmallOptions map[string]any `json:"small_options,omitempty"`
}

type Config struct {
	Model     string `json:"model"`      // "provider/model", used for `mem ask`
	FastModel string `json:"fast_model"` // small jobs (tagging, query expansion); defaults to Model
	AutoTag   bool   `json:"auto_tag"`   // AI-suggest tags on every `mem add`
	// `mem ask` sends ALL notes (best recall, one call) while their total size is under this
	// many characters; above it, it falls back to search. Lower it for small local models.
	ContextBudget int                 `json:"context_budget"`
	Providers     map[string]Provider `json:"providers"` // extra providers, or overrides of built-in ones
}

var builtinProviders = map[string]Provider{
	"anthropic":  {API: "anthropic"},
	"openai":     {API: "openai", BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
	"openrouter": {API: "openai", BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY"},
	"gemini":     {API: "openai", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", APIKeyEnv: "GEMINI_API_KEY"},
	"groq":       {API: "openai", BaseURL: "https://api.groq.com/openai/v1", APIKeyEnv: "GROQ_API_KEY"},
	"mistral":    {API: "openai", BaseURL: "https://api.mistral.ai/v1", APIKeyEnv: "MISTRAL_API_KEY"},
	// deepseek-flash thinks by default (high effort): keep it low for answers, off for small jobs.
	"deepseek": {API: "openai", BaseURL: "https://api.deepseek.com", APIKeyEnv: "DEEPSEEK_API_KEY",
		Options:      map[string]any{"reasoning_effort": "low"},
		SmallOptions: map[string]any{"reasoning_effort": nil, "thinking": map[string]any{"type": "disabled"}}},
	"xai":      {API: "openai", BaseURL: "https://api.x.ai/v1", APIKeyEnv: "XAI_API_KEY"},
	"ollama":   {API: "openai", BaseURL: "http://localhost:11434/v1"},
	"lmstudio": {API: "openai", BaseURL: "http://localhost:1234/v1"},
}

const (
	defaultModel  = "deepseek/deepseek-flash"
	defaultBudget = 400_000 // chars, roughly 100k tokens
)

var configPath = filepath.Join(home, "config.json")

func loadConfig() Config {
	cfg := Config{Model: defaultModel, ContextBudget: defaultBudget}
	if b, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			die("bad %s: %v", configPath, err)
		}
	}
	if m := os.Getenv("MEM_MODEL"); m != "" {
		cfg.Model = m
	}
	if cfg.FastModel == "" {
		cfg.FastModel = cfg.Model
	}
	return cfg
}

func writeDefaultConfig() error {
	if exists(configPath) {
		return nil
	}
	os.MkdirAll(home, 0o755)
	b, _ := json.MarshalIndent(Config{
		Model:         defaultModel,
		FastModel:     "",
		AutoTag:       false,
		ContextBudget: defaultBudget,
		Providers: map[string]Provider{
			"example-local": {API: "openai", BaseURL: "http://localhost:8080/v1"},
		},
	}, "", "  ")
	return os.WriteFile(configPath, append(b, '\n'), 0o644)
}

// resolveModel splits "provider/model" (the model part may itself contain slashes,
// e.g. "openrouter/meta-llama/llama-3.3-70b") and finds the provider.
func (c Config) resolveModel(spec string) (Provider, string, error) {
	name, model, ok := strings.Cut(spec, "/")
	if !ok {
		switch {
		case strings.HasPrefix(spec, "claude"):
			name, model = "anthropic", spec
		case strings.HasPrefix(spec, "gpt"), strings.HasPrefix(spec, "o1"), strings.HasPrefix(spec, "o3"), strings.HasPrefix(spec, "o4"):
			name, model = "openai", spec
		default:
			return Provider{}, "", fmt.Errorf("model %q: use provider/model, e.g. ollama/%s", spec, spec)
		}
	}
	p, builtin := builtinProviders[name]
	user, custom := c.Providers[name]
	if !builtin && !custom {
		return Provider{}, "", fmt.Errorf("unknown provider %q (add it under \"providers\" in %s)", name, configPath)
	}
	if custom { // overlay only the fields the user set, keeping built-in defaults
		if user.API != "" {
			p.API = user.API
		}
		if user.BaseURL != "" {
			p.BaseURL = user.BaseURL
		}
		if user.APIKeyEnv != "" {
			p.APIKeyEnv = user.APIKeyEnv
		}
		if user.Options != nil {
			p.Options = user.Options
		}
		if user.SmallOptions != nil {
			p.SmallOptions = user.SmallOptions
		}
	}
	if p.API == "" {
		p.API = "openai"
	}
	return p, model, nil
}
