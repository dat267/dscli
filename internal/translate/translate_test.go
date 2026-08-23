package translate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveStyle(t *testing.T) {
	t.Run("default fallback", func(t *testing.T) {
		style, err := ResolveStyle("", "ja", "en")
		if err != nil {
			t.Fatalf("ResolveStyle: %v", err)
		}
		for _, want := range []string{"STYLE: GENERAL", "pronouns", "active voice", "register"} {
			if !strings.Contains(style, want) {
				t.Errorf("default style missing %q", want)
			}
		}
	})

	t.Run("explicit file", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "style.md")
		if err := os.WriteFile(p, []byte("CUSTOM RULES HERE"), 0644); err != nil {
			t.Fatal(err)
		}
		got, err := ResolveStyle(p, "ja", "en")
		if err != nil || !strings.Contains(got, "CUSTOM RULES HERE") {
			t.Errorf("explicit style = %q err=%v", got, err)
		}
		if _, err := ResolveStyle(filepath.Join(dir, "missing.md"), "ja", "en"); err == nil {
			t.Error("missing explicit file must error")
		}
	})

	t.Run("sidecar per-pair file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "ja-en.md-parent"), 0755); err != nil {
			t.Fatal(err)
		}
		// styleDirs lookup uses translate/ subdirs; emulate by overriding.
		sub := filepath.Join(dir, "tr")
		if err := os.MkdirAll(sub, 0755); err != nil {
			t.Fatal(err)
		}
		// Newer Go: no t.Setenv-based dir override; we patch the package var.
		orig := styleDirs
		t.Cleanup(func() { styleDirs = orig })
		styleDirs = []string{sub}
		if err := os.WriteFile(filepath.Join(sub, "ja-en.md"), []byte("JA EN PAIR RULES"), 0644); err != nil {
			t.Fatal(err)
		}
		got, err := ResolveStyle("", "ja", "en")
		if err != nil || !strings.Contains(got, "JA EN PAIR RULES") {
			t.Errorf("sidecar style = %q err=%v", got, err)
		}
		// default.md fallback
		if err := os.WriteFile(filepath.Join(sub, "default.md"), []byte("DEFAULT RULES"), 0644); err != nil {
			t.Fatal(err)
		}
		got, err = ResolveStyle("", "de", "fr")
		if err != nil || !strings.Contains(got, "DEFAULT RULES") {
			t.Errorf("default.md fallback = %q err=%v", got, err)
		}
	})
}

func TestPromptIncludesStyle(t *testing.T) {
	p := Prompt("text", "ja", "en", false, "SPECIAL STYLE BLOCK")
	if !strings.Contains(p, "SPECIAL STYLE BLOCK") {
		t.Errorf("prompt missing style:\n%s", p)
	}
	// The style is appended before the final output-only instruction.
	if i, j := strings.Index(p, "SPECIAL STYLE BLOCK"), strings.Index(p, "Reply with ONLY"); i < 0 || j < 0 || i > j {
		t.Error("style must come before the closing instruction")
	}
}
