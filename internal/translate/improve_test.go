package translate

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dat267/dscli/internal/deepseek"
)

func TestImprovePrompt(t *testing.T) {
	p := improvePrompt("text", false, "")
	for _, want := range []string{"Improve the writing", "Do not translate", "Plain text.", "Reply with ONLY the improved content"} {
		if !strings.Contains(p, want) {
			t.Errorf("improve prompt missing %q:\n%s", want, p)
		}
	}
	// Structural formats keep their preserve rules so timestamps/markup survive.
	if !strings.Contains(improvePrompt("lrc", false, ""), "timestamp") {
		t.Error("improve prompt should keep structural rules for lrc")
	}
	// The reminder variant names the improvement verification failure.
	if !strings.Contains(improvePrompt("text", true, ""), "IMPROVEMENT VERIFICATION FAILED") {
		t.Error("improve prompt missing reminder warning")
	}
	// Style is appended before the closing instruction.
	p2 := improvePrompt("text", false, "CUSTOM STYLE BLOCK")
	if i, j := strings.Index(p2, "CUSTOM STYLE BLOCK"), strings.Index(p2, "Reply with ONLY"); i < 0 || j < 0 || i > j {
		t.Error("style must come before the closing instruction")
	}
}

func TestResolveImproveStyle(t *testing.T) {
	t.Run("default fallback", func(t *testing.T) {
		style, err := ResolveImproveStyle("")
		if err != nil {
			t.Fatalf("ResolveImproveStyle: %v", err)
		}
		for _, want := range []string{"IMPROVE WRITING", "grammar", "active voice"} {
			if !strings.Contains(style, want) {
				t.Errorf("default improve style missing %q", want)
			}
		}
	})

	t.Run("explicit file", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "style.md")
		if err := os.WriteFile(p, []byte("CUSTOM IMPROVE RULES"), 0644); err != nil {
			t.Fatal(err)
		}
		got, err := ResolveImproveStyle(p)
		if err != nil || !strings.Contains(got, "CUSTOM IMPROVE RULES") {
			t.Errorf("explicit style = %q err=%v", got, err)
		}
		if _, err := ResolveImproveStyle(filepath.Join(dir, "missing.md")); err == nil {
			t.Error("missing explicit file must error")
		}
	})

	t.Run("improve-writing/default.md sidecar", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "iw")
		if err := os.MkdirAll(sub, 0755); err != nil {
			t.Fatal(err)
		}
		orig := improveStyleDirs
		t.Cleanup(func() { improveStyleDirs = orig })
		improveStyleDirs = []string{sub}
		if err := os.WriteFile(filepath.Join(sub, "default.md"), []byte("IMPROVE DEFAULT RULES"), 0644); err != nil {
			t.Fatal(err)
		}
		got, err := ResolveImproveStyle("")
		if err != nil || !strings.Contains(got, "IMPROVE DEFAULT RULES") {
			t.Errorf("improve-writing/default.md = %q err=%v", got, err)
		}
	})
}

func TestTranslateImprovePromptWiring(t *testing.T) {
	// Capture the prompt sent to the model and assert improve mode swaps the
	// instruction for a writing-improvement one (and translate mode keeps the
	// translation instruction).
	var mu sync.Mutex
	var lastPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/chat_session/create":
			_, _ = io.WriteString(w, `{"code":0,"data":{"biz_data":{"chat_session":{"id":"sess-1"}}}}`)
		case "/api/v0/chat/create_pow_challenge":
			_, _ = io.WriteString(w, `{"code":0,"data":{"biz_data":{"challenge":{
				"algorithm":"DeepSeekHashV1",
				"challenge":"9099d8ee62c210152bb06cf47a6071e24785ab2e6c413d4cab9bbb9f849f5a58",
				"salt":"450f343f44a1e9e6",
				"signature":"signed-request",
				"target_path":"/api/v0/chat/completion",
				"difficulty":2000,
				"expire_at":1752033600
			}}}}`)
		case "/api/v0/chat/completion":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			lastPrompt = string(b)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sseReply(t, "ok\n"))
		case "/api/v0/chat_session/delete":
			_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)

	_, _, err := Translate(context.Background(), client, "sess-1", []byte("hello"), "text", Options{Improve: true})
	if err != nil {
		t.Fatalf("Translate improve: %v", err)
	}
	if !strings.Contains(lastPrompt, "Improve the writing") {
		t.Errorf("improve prompt not sent:\n%s", lastPrompt)
	}
	if strings.Contains(lastPrompt, "Translate the following") {
		t.Errorf("improve mode must not use the translation prompt:\n%s", lastPrompt)
	}

	_, _, err = Translate(context.Background(), client, "sess-1", []byte("hello"), "text", Options{To: "English"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if !strings.Contains(lastPrompt, "Translate the following") {
		t.Errorf("translation prompt not sent:\n%s", lastPrompt)
	}
}
