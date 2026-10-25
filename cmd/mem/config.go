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
	// Shell command that prints the key, e.g. "op read op://Private/DeepSeek/credential".
	APIKeyCmd string `json:"api_key_cmd,omitempty"`
	Name      string `json:"-"` // provider name, filled in by resolveModel
	// Extra fields merged into every request body (openai api), e.g. {"reasoning_effort": "low"}.
	Options map[string]any `json:"options,omitempty"`
	// Merged on top of Options for quick jobs (tagging) / for `mem ask -d`; null removes a key.
	SmallOptions map[string]any `json:"small_options,omitempty"`
	DeepOptions  map[string]any `json:"deep_options,omitempty"`
}

type Config struct {
	NotesDir  string `json:"notes_dir"`  // where notes live (any folder of .md files, e.g. an Obsidian vault)
	Inbox     string `json:"inbox"`      // subfolder of notes_dir for notes created by mem ("" = top level)
	Model     string `json:"model"`      // "provider/model", used for `mem ask`
	FastModel string `json:"fast_model"` // small jobs (tagging, query expansion); defaults to Model
	AutoTag   bool   `json:"auto_tag"`   // AI-suggest tags on every `mem add`
	MaxSteps  int    `json:"max_steps"`  // tool-call rounds per `mem ask` before it must answer
	// Notes the AI must never see, as folder/note paths or globs ("Kundix/Crypto", "**/keys*").
	// A note can also opt out itself with `private: true` (or `ai: false`) in its frontmatter.
	AIExclude []string `json:"ai_exclude"`
	// Show the copy/run/follow-up menu after `mem ask` answers in a terminal (default true).
	Actions *bool `json:"actions,omitempty"`
	// Extra regexes to redact (on top of the built-in secret detection), e.g. "INTERNAL-\\d+".
	RedactPatterns []string            `json:"redact_patterns"`
	Providers      map[string]Provider `json:"providers"` // extra providers, or overrides of built-in ones
}

var builtinProviders = map[string]Provider{
	"anthropic":  {API: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY"},
	"openai":     {API: "openai", BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
	"openrouter": {API: "openai", BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY"},
	"gemini":     {API: "openai", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", APIKeyEnv: "GEMINI_API_KEY"},
	"groq":       {API: "openai", BaseURL: "https://api.groq.com/openai/v1", APIKeyEnv: "GROQ_API_KEY"},
	"mistral":    {API: "openai", BaseURL: "https://api.mistral.ai/v1", APIKeyEnv: "MISTRAL_API_KEY"},
	// deepseek-flash thinks by default (high effort), which costs seconds per call. Note lookups
	// don't need it: thinking off by default, on (high) for `mem ask -d`.
	"deepseek": {API: "openai", BaseURL: "https://api.deepseek.com", APIKeyEnv: "DEEPSEEK_API_KEY",
		Options:     map[string]any{"thinking": map[string]any{"type": "disabled"}},
		DeepOptions: map[string]any{"thinking": map[string]any{"type": "enabled"}, "reasoning_effort": "high"}},
	"xai":      {API: "openai", BaseURL: "https://api.x.ai/v1", APIKeyEnv: "XAI_API_KEY"},
	"ollama":   {API: "openai", BaseURL: "http://localhost:11434/v1"},
	"lmstudio": {API: "openai", BaseURL: "http://localhost:1234/v1"},
}

const (
	defaultModel    = "deepseek/deepseek-flash"
	defaultMaxSteps = 8
)

var configPath = filepath.Join(home, "config.json")

var loaded *Config

func loadConfig() Config {
	if loaded != nil {
		return *loaded
	}
	cfg := Config{NotesDir: filepath.Join(home, "notes"), Model: defaultModel, MaxSteps: defaultMaxSteps}
	if b, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			die("bad %s: %v", configPath, err)
		}
	}
	if m := os.Getenv("MEM_MODEL"); m != "" {
		cfg.Model = m
	}
	if d := os.Getenv("MEM_NOTES"); d != "" {
		cfg.NotesDir = d
	}
	if rest, ok := strings.CutPrefix(cfg.NotesDir, "~/"); ok {
		h, _ := os.UserHomeDir()
		cfg.NotesDir = filepath.Join(h, rest)
	}
	if abs, err := filepath.Abs(cfg.NotesDir); err == nil {
		cfg.NotesDir = abs
	}
	if cfg.FastModel == "" {
		cfg.FastModel = cfg.Model
	}
	loaded = &cfg
	return cfg
}

func writeDefaultConfig() error {
	if exists(configPath) {
		return nil
	}
	os.MkdirAll(home, 0o755)
	b, _ := json.MarshalIndent(Config{
		NotesDir:       filepath.Join(home, "notes"),
		Inbox:          "",
		Model:          defaultModel,
		FastModel:      "",
		AutoTag:        false,
		MaxSteps:       defaultMaxSteps,
		RedactPatterns: []string{},
		AIExclude:      []string{},
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
		if user.APIKeyCmd != "" {
			p.APIKeyCmd = user.APIKeyCmd
		}
		if user.Options != nil {
			p.Options = user.Options
		}
		if user.SmallOptions != nil {
			p.SmallOptions = user.SmallOptions
		}
		if user.DeepOptions != nil {
			p.DeepOptions = user.DeepOptions
		}
	}
	if p.API == "" {
		p.API = "openai"
	}
	p.Name = name
	return p, model, nil
}
