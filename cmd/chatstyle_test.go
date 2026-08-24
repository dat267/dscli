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

// TestChatStylePrepended: the resolved style is prepended to the prompt that
// reaches the model in a general chat turn.
func TestChatStylePrepended(t *testing.T) {
	srv, rec := fakeDeepSeekServer(t)
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{}
	withStdin(t, "hi\n/exit\n", func() {
		captureStdout(t, func() {
			captureStderr(t, func() {
				_ = cmd.replLoop(context.Background(), client, "sess-1", nil, false)
			})
		})
	})
	prompt, _ := completionBody(t, rec, 0)
	if !strings.HasPrefix(prompt, DefaultChatStyle()+"\n\nhi") {
		t.Errorf("prompt = %q, want the chat style prepended before the message", prompt)
	}
}

// TestAskChatStylePrepended: `ask` prepends the style too.
func TestAskChatStylePrepended(t *testing.T) {
	srv, rec := fakeDeepSeekServer(t)
	defer srv.Close()
	cmd := &AskCmd{Prompt: []string{"hello"}, Token: "tok", clientBase: srv.URL}
	captureStdout(t, func() {
		if err := cmd.Run(&App{cfgPath: filepath.Join(t.TempDir(), "dscli.json")}, context.Background()); err != nil {
			t.Fatalf("ask: %v", err)
		}
	})
	prompt, _ := completionBody(t, rec, 0)
	if !strings.HasPrefix(prompt, DefaultChatStyle()+"\n\nhello") {
		t.Errorf("prompt = %q, want the chat style prepended", prompt)
	}
}
