package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImproveWritingRequiresInPlace(t *testing.T) {
	cmd := &ImproveWritingCmd{Token: "tok", File: []string{"a.md"}}
	if err := cmd.Run(nil, context.Background()); err == nil || !strings.Contains(err.Error(), "--in-place") {
		t.Errorf("missing --in-place must fail, got %v", err)
	}
}

func TestImproveWritingRequiresToken(t *testing.T) {
	cmd := &ImproveWritingCmd{InPlace: true, File: []string{"a.md"}}
	if err := cmd.Run(nil, context.Background()); err == nil || !strings.Contains(err.Error(), "no DeepSeek session") {
		t.Errorf("missing token must fail, got %v", err)
	}
}

func TestImproveWritingRejectsEpub(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "book.epub")
	if err := os.WriteFile(in, []byte("PK\x03\x04epub"), 0644); err != nil {
		t.Fatal(err)
	}
	// epub is rejected before any session is created, so the token need not be valid.
	cmd := &ImproveWritingCmd{InPlace: true, File: []string{in}, Token: "tok"}
	if err := cmd.Run(nil, context.Background()); err == nil || !strings.Contains(err.Error(), "epub") {
		t.Errorf("epub in-place must fail, got %v", err)
	}
}

func TestImproveWritingPlainMD(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "notes.translated.md")
	if err := os.WriteFile(in, []byte("Hello world\n"), 0644); err != nil {
		t.Fatal(err)
	}
	srv, rec := fakeDeepSeekServerWith(t, []string{completionSSE(t, 2, "Bonjour le monde\n")})
	defer srv.Close()
	cmd := &ImproveWritingCmd{NoPersist: true, InPlace: true, File: []string{in}, Token: "tok", clientBase: srv.URL}
	if err := cmd.Run(nil, context.Background()); err != nil {
		t.Fatalf("improve-writing: %v", err)
	}

	// The file is rewritten in place — no separate output path.
	got, err := os.ReadFile(in)
	if err != nil || string(got) != "Bonjour le monde\n" {
		t.Errorf("file not rewritten in place: %q err=%v", got, err)
	}
	prompt, _ := completionBody(t, rec, 0)
	if !strings.Contains(prompt, "Improve the writing") {
		t.Errorf("improve prompt not sent:\n%s", prompt)
	}
	if strings.Contains(prompt, "Translate the following") {
		t.Errorf("improve mode must not use the translation prompt:\n%s", prompt)
	}
}
