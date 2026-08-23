package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPairs(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"a.md":                          "orig",
		"a.translated.en.md":            "en",
		"a.translated.ja.md":            "ja",
		"b.md":                          "no translation, not listed",
		"c.translated.zh.txt":           "orphan (no original)",
		"notes/readme.md":               "orig",
		"notes/readme.translated.fr.md": "fr",
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	out := captureStdout(t, func() {
		if err := (&PairsCmd{Dir: dir}).Run(nil, context.Background()); err != nil {
			t.Fatalf("pairs: %v", err)
		}
	})

	// TSV: base, lang, path. Originals close their pairs; orphans listed.
	for _, want := range []string{
		"a\t-\t" + filepath.Join(dir, "a.md") + "\n",
		"a\ten\t" + filepath.Join(dir, "a.translated.en.md") + "\n",
		"a\tja\t" + filepath.Join(dir, "a.translated.ja.md") + "\n",
		"c\tzh\t" + filepath.Join(dir, "c.translated.zh.txt") + "\n",
		"readme\t-\t" + filepath.Join(dir, "notes", "readme.md") + "\n",
		"readme\tfr\t" + filepath.Join(dir, "notes", "readme.translated.fr.md") + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pairs output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "b.md") {
		t.Errorf("file without a translation must not be listed:\n%s", out)
	}
}
