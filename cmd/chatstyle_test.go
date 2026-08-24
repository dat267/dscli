package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dat267/dscli/internal/deepseek"
)

func TestResolveChatStyle(t *testing.T) {
	// Default fallback.
	got, err := ResolveChatStyle("")
	if err != nil || got != DefaultChatStyle() {
		t.Errorf("default = %q err=%v", got, err)
	}

	// Explicit file.
	dir := t.TempDir()
	p := filepath.Join(dir, "style.md")
	if err := os.WriteFile(p, []byte("MY CHAT STYLE"), 0644); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveChatStyle(p); err != nil || got != "MY CHAT STYLE" {
		t.Errorf("explicit = %q err=%v", got, err)
	}
	if _, err := ResolveChatStyle(filepath.Join(dir, "missing.md")); err == nil {
		t.Error("missing explicit file must error")
	}

	// Sidecar override (chat/chat.md beats the built-in default).
	sub := filepath.Join(dir, "ch")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	orig := chatStyleDirs
	t.Cleanup(func() { chatStyleDirs = orig })
	chatStyleDirs = []string{sub}
	if err := os.WriteFile(filepath.Join(sub, "chat.md"), []byte("SIDECAR STYLE"), 0644); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveChatStyle(""); err != nil || got != "SIDECAR STYLE" {
		t.Errorf("sidecar = %q err=%v", got, err)
	}
}

// TestChatStyleRetryOnRejection: the style is NOT sent on the first attempt;
// only when the reply is content-filtered / cut off is it retried once with
// the style prepended.
func TestChatStyleRetryOnRejection(t *testing.T) {
	rejected := completionSSE(t, 2, "") + "data: {\"v\":[{\"p\":\"status\",\"v\":\"CONTENT_FILTER\"},{\"p\":\"quasi_status\",\"v\":\"CONTENT_FILTER\"}]}\n\n"
	srv, rec := fakeDeepSeekServerWith(t, []string{rejected, completionSSE(t, 3, "answer")})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{}
	withStdin(t, "hi\n/exit\n", func() {
		captureStdout(t, func() {
			captureStderr(t, func() {
				_ = cmd.replLoop(context.Background(), client, "sess-1", nil, false)
			})
		})
	})
	// First attempt: no style.
	first, _ := completionBody(t, rec, 0)
	if first != "hi" {
		t.Errorf("first prompt = %q, want %q (no style on the initial attempt)", first, "hi")
	}
	// Retry: style prepended, continuing from the filtered reply.
	second, parent := completionBody(t, rec, 1)
	if want := DefaultChatStyle() + "\n\nhi"; second != want {
		t.Errorf("retry prompt = %q, want %q", second, want)
	}
	if parent != float64(2) {
		t.Errorf("retry parent_message_id = %v, want 2 (the filtered reply)", parent)
	}
}

// TestChatStyleNotRetriedWhenAccepted: a normal reply does not trigger a
// styled retry.
func TestChatStyleNotRetriedWhenAccepted(t *testing.T) {
	srv, rec := fakeDeepSeekServerWith(t, []string{completionSSE(t, 2, "fine")})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{}
	withStdin(t, "hi\n/exit\n", func() {
		captureStdout(t, func() {
			captureStderr(t, func() {
				_ = cmd.replLoop(context.Background(), client, "sess-1", nil, false)
			})
		})
	})
	if got := rec.remaining; len(got) != 0 {
		t.Errorf("unused completions = %d, want 0 (no retry on an accepted reply)", len(got))
	}
	prompt, _ := completionBody(t, rec, 0)
	if prompt != "hi" {
		t.Errorf("prompt = %q, want %q (no style prepended)", prompt, "hi")
	}
}

// TestAskChatStyleRetryOnRejection: `ask` retries once with the style when the
// reply is content-filtered.
func TestAskChatStyleRetryOnRejection(t *testing.T) {
	rejected := completionSSE(t, 2, "") + "data: {\"v\":[{\"p\":\"status\",\"v\":\"CONTENT_FILTER\"},{\"p\":\"quasi_status\",\"v\":\"CONTENT_FILTER\"}]}\n\n"
	srv, rec := fakeDeepSeekServerWith(t, []string{rejected, completionSSE(t, 3, "answer")})
	defer srv.Close()
	cmd := &AskCmd{Prompt: []string{"hello"}, Token: "tok", clientBase: srv.URL}
	var stdout, stderr string
	stdout = captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if err := cmd.Run(&App{cfgPath: filepath.Join(t.TempDir(), "dscli.json")}, context.Background()); err != nil {
				t.Fatalf("ask: %v", err)
			}
		})
	})
	first, _ := completionBody(t, rec, 0)
	if first != "hello" {
		t.Errorf("first prompt = %q, want %q (no style)", first, "hello")
	}
	second, _ := completionBody(t, rec, 1)
	if want := DefaultChatStyle() + "\n\nhello"; second != want {
		t.Errorf("retry prompt = %q, want %q", second, want)
	}
	if !strings.Contains(stdout, "answer") {
		t.Errorf("stdout = %q (stderr=%q), want the retried reply", stdout, stderr)
	}
}
