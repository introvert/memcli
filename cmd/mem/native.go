package main

import (
	"embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The Chrome extension (cmd/mem/extension) talks to mem through Chrome's native messaging: Chrome
// starts `mem chrome-extension://<id>/` and exchanges length-prefixed JSON on stdin/stdout. The
// requests are the same as `mem serve` speaks to the menu bar app. Only the extension with this
// id may start it, as the host manifest says; it can't run commands.

const (
	nativeHost  = "com.introvert.mem"
	extensionID = "pjjdngogiihgiedcnpafmapjgebfeagh" // fixed by the "key" in extension/manifest.json
)

//go:embed extension
var extensionFiles embed.FS

func cmdNative([]string) {
	s := newServer(func(b []byte) {
		var n [4]byte
		binary.LittleEndian.PutUint32(n[:], uint32(len(b)))
		os.Stdout.Write(append(n[:], b...))
	})
	for {
		var n [4]byte
		if _, err := io.ReadFull(os.Stdin, n[:]); err != nil {
			return // Chrome closed the port
		}
		size := binary.LittleEndian.Uint32(n[:])
		if size > 64<<20 {
			return
		}
		msg := make([]byte, size)
		if _, err := io.ReadFull(os.Stdin, msg); err != nil {
			return
		}
		s.dispatch(msg)
	}
}

// browserHostDirs are where Chromium-based browsers look for native messaging hosts.
func browserHostDirs() map[string]string {
	h, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		as := filepath.Join(h, "Library", "Application Support")
		return map[string]string{
			"Chrome": filepath.Join(as, "Google", "Chrome"), "Chrome Beta": filepath.Join(as, "Google", "Chrome Beta"),
			"Chromium": filepath.Join(as, "Chromium"), "Brave": filepath.Join(as, "BraveSoftware", "Brave-Browser"),
			"Edge": filepath.Join(as, "Microsoft Edge"), "Arc": filepath.Join(as, "Arc", "User Data"),
		}
	case "linux":
		cfg := filepath.Join(h, ".config")
		return map[string]string{
			"Chrome": filepath.Join(cfg, "google-chrome"), "Chromium": filepath.Join(cfg, "chromium"),
			"Brave": filepath.Join(cfg, "BraveSoftware", "Brave-Browser"), "Edge": filepath.Join(cfg, "microsoft-edge"),
		}
	}
	return nil
}

// cmdExtension unpacks the extension to ~/.mem/extension and registers mem as its native host.
func cmdExtension([]string) {
	dirs := browserHostDirs()
	if dirs == nil {
		die("mem extension supports macOS and Linux for now")
	}
	self, err := os.Executable()
	check(err)
	self, _ = filepath.EvalSymlinks(self)

	dest := filepath.Join(home, "extension")
	os.RemoveAll(dest)
	check(fs.WalkDir(extensionFiles, "extension", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out := filepath.Join(dest, strings.TrimPrefix(p, "extension"))
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := extensionFiles.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	}))

	manifest, _ := json.MarshalIndent(map[string]any{
		"name": nativeHost, "description": "mem — personal memory", "path": self, "type": "stdio",
		"allowed_origins": []string{"chrome-extension://" + extensionID + "/"},
	}, "", "  ")
	var installed []string
	for name, dir := range dirs {
		if !exists(dir) {
			continue // browser not installed
		}
		hostDir := filepath.Join(dir, "NativeMessagingHosts")
		if os.MkdirAll(hostDir, 0o755) == nil &&
			os.WriteFile(filepath.Join(hostDir, nativeHost+".json"), manifest, 0o644) == nil {
			installed = append(installed, name)
		}
	}
	if len(installed) == 0 {
		die("no Chromium-based browser found")
	}
	fmt.Printf("%s registered mem for %s\n\n", c("32", "✓"), strings.Join(installed, ", "))
	fmt.Println("Now load the extension (once):")
	fmt.Println("  1. open chrome://extensions and turn on Developer mode (top right)")
	fmt.Println("  2. click \"Load unpacked\" and choose " + c("36", dest))
	fmt.Println()
	fmt.Println(c("2", "Then: ⌥⇧M (or the toolbar icon) to save, search and ask; right-click → Save to mem."))
	fmt.Println(c("2", "Run `mem extension` again after updating mem; then reload the extension."))
}
