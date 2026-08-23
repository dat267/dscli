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

// searchSSE builds a snapshot carrying a response fragment plus a
// TOOL_SEARCH fragment with citations, so [citation:N] has sources.
func searchSSE(t *testing.T, seq int, content string, refs []map[string]string) string {
	t.Helper()
	var refsAny []any
	for _, r := range refs {
		m := map[string]any{}
		for k, v := range r {
			m[k] = v
		}
		refsAny = append(refsAny, m)
	}
	frags := []any{
		map[string]any{"type": "response", "content": content},
		map[string]any{"type": "tool_search", "references": refsAny},
	}
	line, err := json.Marshal(map[string]any{
		"v": map[string]any{
			"response":   map[string]any{"fragments": frags, "message_id": seq},
			"message_id": seq,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(line) + "\n\n"
}

func TestAskSearchSources(t *testing.T) {
	srv, _ := fakeDeepSeekServerWith(t, []string{
		searchSSE(t, 2, "Gold is high [citation:1]", []map[string]string{
			{"url": "https://ex.com/gold", "title": "Gold Prices"},
		}),
	})
	defer srv.Close()
	cmd := &AskCmd{Prompt: []string{"gold price"}, Search: true, Token: "tok", clientBase: srv.URL}

	var stdout, stderr string
	stdout = captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if err := cmd.Run(nil, context.Background()); err != nil {
				t.Fatalf("ask: %v", err)
			}
		})
	})
	if !strings.Contains(stdout, "Gold is high [citation:1]") {
		t.Errorf("stdout = %q", stdout)
	}
	for _, want := range []string{"Sources:", "[1] Gold Prices — https://ex.com/gold"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

func TestAskSearchSourcesJSON(t *testing.T) {
	srv, _ := fakeDeepSeekServerWith(t, []string{
		searchSSE(t, 2, "hi [citation:1]", []map[string]string{{"url": "https://ex.com/a", "title": "A"}}),
	})
	defer srv.Close()
	cmd := &AskCmd{Prompt: []string{"x"}, Search: true, JSONOut: true, Token: "tok", clientBase: srv.URL}

	stdout := captureStdout(t, func() {
		if err := cmd.Run(nil, context.Background()); err != nil {
			t.Fatalf("ask: %v", err)
		}
	})
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var final map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &final); err != nil {
		t.Fatalf("final line not JSON: %q: %v", lines[len(lines)-1], err)
	}
	srcs, ok := final["sources"].([]any)
	if !ok || len(srcs) != 1 {
		t.Errorf("sources line = %v", final)
	}
}
