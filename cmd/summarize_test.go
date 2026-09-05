package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSummarizeRequiresToken(t *testing.T) {
	cmd := &SummarizeCmd{File: []string{"a.md"}}
	if err := cmd.Run(nil, context.Background()); err == nil || !strings.Contains(err.Error(), "no DeepSeek session") {
		t.Errorf("missing token must fail, got %v", err)
	}
}

func TestSummarizeOutputRefusedWithoutForce(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "a.md")
	out := filepath.Join(dir, "b.md")
	if err := os.WriteFile(in, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("y"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := &SummarizeCmd{File: []string{in}, Output: out, Token: "tok"}
	if err := cmd.Run(nil, context.Background()); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("overwriting existing output without -f must fail, got %v", err)
	}
}

func TestSummarizePrintsToStdout(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(in, []byte("Hello world\n"), 0644); err != nil {
		t.Fatal(err)
	}
	srv, _ := fakeDeepSeekServerWith(t, []string{completionSSE(t, 2, "A summary of greetings.\n")})
	defer srv.Close()
	cmd := &SummarizeCmd{NoPersist: true, File: []string{in}, Token: "tok", clientBase: srv.URL}
	out := captureStdout(t, func() {
		if err := cmd.Run(nil, context.Background()); err != nil {
			t.Errorf("summarize: %v", err)
		}
	})
	if !strings.Contains(out, "A summary of greetings.") {
		t.Errorf("summary not printed to stdout: %q", out)
	}
}

func TestSummarizeOutputFile(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "notes.md")
	out := filepath.Join(dir, "notes.summary.md")
	if err := os.WriteFile(in, []byte("Hello world\n"), 0644); err != nil {
		t.Fatal(err)
	}
	srv, rec := fakeDeepSeekServerWith(t, []string{completionSSE(t, 2, "A summary of greetings.\n")})
	defer srv.Close()
	cmd := &SummarizeCmd{NoPersist: true, File: []string{in}, Output: out, Force: true, Token: "tok", clientBase: srv.URL}
	if err := cmd.Run(nil, context.Background()); err != nil {
		t.Fatalf("summarize -o: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil || string(got) != "A summary of greetings.\n" {
		t.Errorf("output file = %q err=%v", got, err)
	}
	prompt, _ := completionBody(t, rec, 0)
	if !strings.Contains(prompt, "Summarize the following") {
		t.Errorf("summarize prompt not sent:\n%s", prompt)
	}
}
