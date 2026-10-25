package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/term"
)

// API keys are looked up, in order: the provider's env var, its api_key_cmd (e.g. a password
// manager), the OS keychain (macOS Keychain / Linux Secret Service), then ~/.mem/keys.json (0600).
// Keys are only ever passed to helper tools via stdin, never as command-line arguments.

var (
	keychainService = envOr("MEM_KEYCHAIN_SERVICE", "mem")
	keysFile        = filepath.Join(home, "keys.json")
	keyCharsRe      = regexp.MustCompile(`^[A-Za-z0-9._~+/=:-]{8,512}$`)
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type keyResult struct{ key, source string }

var keyCache = map[string]keyResult{} // one keychain lookup per provider per run

// apiKey finds the key for a provider; source says where it came from ("" = not found).
func apiKey(p Provider) (key, source string, err error) {
	if r, ok := keyCache[p.Name]; ok {
		return r.key, r.source, nil
	}
	key, source, err = lookupKey(p)
	if err == nil {
		keyCache[p.Name] = keyResult{key, source}
	}
	return key, source, err
}

func lookupKey(p Provider) (key, source string, err error) {
	if p.APIKeyEnv != "" {
		if v := os.Getenv(p.APIKeyEnv); v != "" {
			return v, "env " + p.APIKeyEnv, nil
		}
	}
	if p.APIKeyCmd != "" {
		out, err := exec.Command("sh", "-c", p.APIKeyCmd).Output()
		if err != nil {
			return "", "", fmt.Errorf("api_key_cmd for %s failed: %v", p.Name, err)
		}
		return strings.TrimSpace(string(out)), "api_key_cmd", nil
	}
	if v := keychainGet(p.Name); v != "" {
		return v, keychainName(), nil
	}
	if v := fileKeys()[p.Name]; v != "" {
		return v, keysFile, nil
	}
	return "", "", nil
}

func keychainName() string {
	if runtime.GOOS == "darwin" {
		return "keychain"
	}
	return "keyring"
}

func hasSecretTool() bool { _, err := exec.LookPath("secret-tool"); return err == nil }

func keychainGet(provider string) string {
	var cmd *exec.Cmd
	switch {
	case runtime.GOOS == "darwin":
		cmd = exec.Command("security", "find-generic-password", "-s", keychainService, "-a", provider, "-w")
	case hasSecretTool():
		cmd = exec.Command("secret-tool", "lookup", "service", keychainService, "account", provider)
	default:
		return ""
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// keychainSet stores a key; ok=false means no keychain is available on this system.
func keychainSet(provider, key string) (ok bool, err error) {
	switch {
	case runtime.GOOS == "darwin":
		// `security -i` reads the command from stdin, so the key never shows up in `ps`.
		cmd := exec.Command("security", "-i")
		cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -l \"mem: %s API key\" -w %s\n",
			keychainService, provider, provider, key))
		out, err := cmd.CombinedOutput()
		if err != nil || strings.Contains(string(out), "rror") {
			return true, fmt.Errorf("keychain: %s %v", strings.TrimSpace(string(out)), err)
		}
		return true, nil
	case hasSecretTool():
		cmd := exec.Command("secret-tool", "store", "--label=mem: "+provider+" API key",
			"service", keychainService, "account", provider)
		cmd.Stdin = strings.NewReader(key)
		if out, err := cmd.CombinedOutput(); err != nil {
			return true, fmt.Errorf("keyring: %s %v", strings.TrimSpace(string(out)), err)
		}
		return true, nil
	}
	return false, nil
}

func keychainDelete(provider string) bool {
	switch {
	case runtime.GOOS == "darwin":
		return exec.Command("security", "delete-generic-password", "-s", keychainService, "-a", provider).Run() == nil
	case hasSecretTool():
		return exec.Command("secret-tool", "clear", "service", keychainService, "account", provider).Run() == nil
	}
	return false
}

func fileKeys() map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(keysFile); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func writeFileKeys(m map[string]string) error {
	os.MkdirAll(home, 0o700)
	if len(m) == 0 {
		os.Remove(keysFile)
		return nil
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(keysFile, b, 0o600)
}

// ---------- mem key ----------

func cmdKey(args []string) {
	usage := "usage: mem key set|rm PROVIDER · mem key ls"
	if len(args) == 0 {
		die(usage)
	}
	cfg := loadConfig()
	switch args[0] {
	case "ls", "list":
		names := providerNames(cfg)
		for _, n := range names {
			p, _, _ := cfg.resolveModel(n + "/x")
			_, src, err := apiKey(p)
			switch {
			case err != nil:
				fmt.Printf("%-11s %s\n", n, c("31", err.Error()))
			case src != "":
				fmt.Printf("%-11s %s\n", n, c("32", "✓ "+src))
			case p.APIKeyEnv == "" && p.APIKeyCmd == "" && p.API == "openai":
				fmt.Printf("%-11s %s\n", n, c("2", "no key needed (local)"))
			default:
				fmt.Printf("%-11s %s\n", n, c("2", "—"))
			}
		}
	case "set":
		if len(args) != 2 {
			die("usage: mem key set PROVIDER   (prompts for the key, or reads it from stdin)")
		}
		name := args[1]
		if _, _, err := cfg.resolveModel(name + "/x"); err != nil {
			die("%v", err)
		}
		key := readSecret(fmt.Sprintf("%s API key: ", name))
		if !keyCharsRe.MatchString(key) {
			die("that doesn't look like an API key (8-512 chars, no spaces or quotes)")
		}
		ok, err := keychainSet(name, key)
		check(err)
		where := keychainName()
		if !ok { // no keychain on this system: private file
			m := fileKeys()
			m[name] = key
			check(writeFileKeys(m))
			where = keysFile + " (mode 600)"
		}
		fmt.Println(c("32", "saved ") + name + " key to " + where)
	case "rm", "delete":
		if len(args) != 2 {
			die("usage: mem key rm PROVIDER")
		}
		name, removed := args[1], false
		if keychainDelete(name) {
			removed = true
		}
		if m := fileKeys(); m[name] != "" {
			delete(m, name)
			check(writeFileKeys(m))
			removed = true
		}
		if !removed {
			die("no stored key for %s", name)
		}
		fmt.Println(c("31", "removed ") + name + " key")
	default:
		die(usage)
	}
}

// readSecret prompts without echo on a terminal, or reads the first line of piped stdin.
func readSecret(prompt string) string {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		check(err)
		return strings.TrimSpace(string(b))
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		check(err)
	}
	return strings.TrimSpace(line)
}

func providerNames(cfg Config) []string {
	set := map[string]bool{}
	for n := range builtinProviders {
		set[n] = true
	}
	for n := range cfg.Providers {
		set[n] = true
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
