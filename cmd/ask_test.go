package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dat267/dscli/internal/deepseek"
)

func TestAskPlain(t *testing.T) {
	srv, rec := fakeDeepSeekServerWith(t, []string{completionSSE(t, 2, "Hello back")})
	defer srv.Close()
	cmd := &AskCmd{Prompt: []string{"hi"}, Token: "tok", clientBase: srv.URL}

	stdout := captureStdout(t, func() {
		if err := cmd.Run(nil, context.Background()); err != nil {
			t.Fatalf("ask: %v", err)
		}
	})
	// Reply ends with a newline even though the model text does not.
	if stdout != "Hello back\n" {
		t.Errorf("stdout = %q, want %q", stdout, "Hello back\n")
	}
	// The one-shot session was deleted afterwards.
	rec.mu.Lock()
	deleted := append([]string(nil), rec.deleted...)
	rec.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "sess-1" {
		t.Errorf("session not cleaned up: %v", deleted)
	}
	// First turn carries model_type.
	prompt, parent := completionBody(t, rec, 0)
	if !strings.Contains(prompt, "hi") {
		t.Errorf("prompt = %q", prompt)
	}
	if parent != nil {
		t.Errorf("first turn parent = %v, want null", parent)
	}
}

func TestAskFromStdin(t *testing.T) {
	srv, _ := fakeDeepSeekServerWith(t, []string{completionSSE(t, 2, "sum is 3")})
	defer srv.Close()
	cmd := &AskCmd{Token: "tok", clientBase: srv.URL}

	stdout := captureStdout(t, func() {
		withStdin(t, "1 + 2\n", func() {
			if err := cmd.Run(nil, context.Background()); err != nil {
				t.Fatalf("ask (stdin): %v", err)
			}
		})
	})
	if stdout != "sum is 3\n" {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestAskJSONOut(t *testing.T) {
	srv, _ := fakeDeepSeekServerWith(t, []string{completionSSE(t, 2, "Hi")})
	defer srv.Close()
	cmd := &AskCmd{Prompt: []string{"hi"}, JSONOut: true, Token: "tok", clientBase: srv.URL}

	stdout := captureStdout(t, func() {
		if err := cmd.Run(nil, context.Background()); err != nil {
			t.Fatalf("ask: %v", err)
		}
	})
	var line map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &line); err != nil {
		t.Fatalf("expected one NDJSON delta line, got %q: %v", stdout, err)
	}
	if line["delta"] != "Hi" {
		t.Errorf("delta = %q", line["delta"])
	}
}

func TestAskRequiresInput(t *testing.T) {
	cmd := &AskCmd{Token: "tok"}
	err := cmd.Run(nil, context.Background())
	if err == nil || !strings.Contains(err.Error(), "nothing to ask") {
		t.Errorf("empty input must error, got %v", err)
	}
}

var _ = deepseek.Session{} // keep import if assertions change
