package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dat267/dscli/internal/deepseek"
	"github.com/dat267/dscli/internal/translate"
)

func TestChunkText(t *testing.T) {
	// Line boundaries preserved even when a single line exceeds the budget.
	text := "aaaaa\n" + strings.Repeat("b", 100) + "\nccccc\n" + strings.Repeat("d", 100)
	chunks := translate.ChunkText(text, 20)
	if len(chunks) != 4 { // each oversized line becomes its own chunk
		t.Fatalf("chunks = %d, want 4", len(chunks))
	}
	if strings.Contains(chunks[0], "ccccc") || strings.Contains(chunks[1], "aaaaa") {
		t.Errorf("lines split across chunks:\n%q\n%q", chunks[0], chunks[1])
	}
	if strings.Join(chunks, "") != text {
		t.Errorf("round trip lost data")
	}

	empty := translate.ChunkText("", 10)
	if len(empty) != 1 || empty[0] != "" {
		t.Errorf("empty text must yield one empty chunk: %v", empty)
	}
}

func TestDefaultOutput(t *testing.T) {
	for _, tc := range []struct{ in, to, want string }{
		{"a.md", "English", "a.translated.en.md"},
		{"a.md", "en", "a.translated.en.md"},
		{"dir/song.lrc", "Chinese", "dir/song.translated.zh.lrc"},
		{"book.epub", "English", "book.translated.en.txt"},
		{"movie.vtt", "Japanese", "movie.translated.ja.vtt"},
		{"noext", "English", "noext.translated.en"},
		{"x.translated.en.md", "Chinese", "x.translated.zh.md"}, // never stacks suffixes
		{"x.translated.md", "Chinese", "x.translated.zh.md"},
		{"song.lrc", "Klingon", "song.translated.klingon.lrc"}, // unknown label → normalized token
	} {
		if got := translate.DefaultOutput(tc.in, tc.to); got != tc.want {
			t.Errorf("DefaultOutput(%s, %s) = %s, want %s", tc.in, tc.to, got, tc.want)
		}
	}
}

func TestLangCode(t *testing.T) {
	for label, want := range map[string]string{
		"English": "en", "english": "en", "Japanese": "ja", "简体中文": "",
		"Portuguese": "pt", "Klingon": "klingon",
	} {
		if got := translate.LangCode(label); got != want {
			t.Errorf("LangCode(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestTranslatePromptFormats(t *testing.T) {
	for _, format := range []string{"text", "lrc", "srt", "vtt", "ass", "ttml", "markdown"} {
		p := translate.Prompt(format, "auto", "Chinese", false, "")
		for _, want := range []string{"Chinese", "Reply with ONLY the translated content"} {
			if !strings.Contains(p, want) {
				t.Errorf("%s prompt missing %q", format, want)
			}
		}
	}
	p := translate.Prompt("lrc", "auto", "Chinese", true, "")
	if !strings.Contains(p, "VERIFICATION FAILED") {
		t.Errorf("reminder prompt missing the verification warning")
	}
}

func TestTranslatePlainMD(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(in, []byte("Hello world\n"), 0644); err != nil {
		t.Fatal(err)
	}
	style := filepath.Join(dir, "style.md")
	if err := os.WriteFile(style, []byte("USE FORMAL REGISTER"), 0644); err != nil {
		t.Fatal(err)
	}
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, "Bonjour le monde\n"),
	})
	defer srv.Close()
	cmd := &TranslateCmd{
		NoPersist: true, File: in, From: "English", To: "French", Token: "tok", clientBase: srv.URL, Instructions: style}
	if err := cmd.Run(nil, context.Background()); err != nil {
		t.Fatalf("translate: %v", err)
	}

	out := filepath.Join(dir, "notes.translated.fr.md")
	got, err := os.ReadFile(out)
	if err != nil || string(got) != "Bonjour le monde\n" {
		t.Errorf("output = %q err=%v", got, err)
	}
	// The session we created was deleted afterwards.
	rec.mu.Lock()
	deleted := append([]string(nil), rec.deleted...)
	rec.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "sess-1" {
		t.Errorf("session not cleaned up: %v", deleted)
	}
	prompt, _ := completionBody(t, rec, 0)
	if !strings.Contains(prompt, "Markdown document") || !strings.Contains(prompt, "French") {
		t.Errorf("first chunk prompt wrong:\n%s", prompt)
	}
	if !strings.Contains(prompt, "USE FORMAL REGISTER") {
		t.Errorf("custom instructions missing from chunk prompt:\n%s", prompt)
	}
}

func TestTranslateOutputExistsRefused(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "a.md")
	out := filepath.Join(dir, "b.md")
	if err := os.WriteFile(in, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("y"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := &TranslateCmd{
		NoPersist: true, File: in, Output: out, Token: "tok"}
	if err := cmd.Run(nil, context.Background()); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("overwriting existing output without -f must fail, got %v", err)
	}
	// With -f the write proceeds.
	srv, _ := fakeDeepSeekServerWith(t, []string{completionSSE(t, 2, "z\n")})
	defer srv.Close()
	cmd = &TranslateCmd{
		NoPersist: true, File: in, Output: out, Force: true, Token: "tok", clientBase: srv.URL}
	if err := cmd.Run(nil, context.Background()); err != nil {
		t.Fatalf("translate with -f: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "z\n" {
		t.Errorf("output = %q", got)
	}
}

func TestTranslateLRCVerificationRetry(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "song.lrc")
	src := "[00:01.00]hello\n[00:05.50]world\n"
	if err := os.WriteFile(in, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	// First attempt corrupts a timestamp; the retry must fix it.
	broken := "[99:99.99]hola\n[00:05.50]mundo\n"
	good := "[00:01.00]bonjour\n[00:05.50]le monde\n"
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, broken),
		completionSSE(t, 3, good),
	})
	defer srv.Close()
	cmd := &TranslateCmd{
		NoPersist: true, File: in, To: "French", Token: "tok", clientBase: srv.URL}
	if err := cmd.Run(nil, context.Background()); err != nil {
		t.Fatalf("translate: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "song.translated.fr.lrc"))
	if string(got) != good {
		t.Errorf("output = %q, want %q", got, good)
	}
	rec.mu.Lock()
	n := len(rec.completionBodies)
	rec.mu.Unlock()
	if n != 2 {
		t.Errorf("completions = %d, want 2 (bad + strict retry)", n)
	}
	prompt2, _ := completionBody(t, rec, 1)
	if !strings.Contains(prompt2, "VERIFICATION FAILED") {
		t.Errorf("retry prompt missing the verification warning:\n%s", prompt2)
	}
}

func TestTranslateLRCVerificationGivesUp(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "song.lrc")
	if err := os.WriteFile(in, []byte("[00:01.00]hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	bad := "[99:99.99]hola\n"
	srv, _ := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, bad),
		completionSSE(t, 3, bad),
	})
	defer srv.Close()
	cmd := &TranslateCmd{
		NoPersist: true, File: in, To: "French", Token: "tok", clientBase: srv.URL}
	err := cmd.Run(nil, context.Background())
	if err == nil || !strings.Contains(err.Error(), "protected line") {
		t.Errorf("persistent corruption must fail loudly, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "song.translated.fr.lrc")); !os.IsNotExist(statErr) {
		t.Error("no output must be written on failure")
	}
}

var _ = deepseek.Session{} // keep import if assertions change

func TestTranslateASSVerificationRetry(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "sub.ass")
	src := "[Script Info]\nPlayResX: 1920\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Hello\n"
	if err := os.WriteFile(in, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	broken := "[Script Info]\nPlayResX: 1920\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,9:99:01.00,0:00:04.00,Default,,0,0,0,,Hola\n"
	good := "[Script Info]\nPlayResX: 1920\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Bonjour\n"
	srv, _ := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, broken),
		completionSSE(t, 3, good),
	})
	defer srv.Close()
	cmd := &TranslateCmd{
		NoPersist: true, File: in, To: "French", Token: "tok", clientBase: srv.URL}
	if err := cmd.Run(nil, context.Background()); err != nil {
		t.Fatalf("translate: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "sub.translated.fr.ass"))
	if string(got) != good {
		t.Errorf("output = %q, want %q", got, good)
	}
}
