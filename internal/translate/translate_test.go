package translate

import (
	"context"
	"encoding/json"
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

func TestTranslateDefaultUsesMaxContext(t *testing.T) {
	// A 300 KiB file (≈75K tokens) must translate in ONE request with the
	// default context-sized chunk, not be sliced into tiny pieces.
	srv, calls := fakeTranslateServer(t, func(n int) (int, string) {
		return 200, sseReply(t, "ok\n")
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	content := strings.Repeat("a", 300*1024)
	text, err := Translate(context.Background(), client, "sess-1", []byte(content), "text", Options{To: "English"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if text != "ok\n" {
		t.Errorf("text = %q", text)
	}
	if got := *calls; got != 1 {
		t.Errorf("completions = %d, want 1 (whole file in one context-sized chunk)", got)
	}
}

func TestTranslateBisectsOnOverflow(t *testing.T) {
	// The first request overflows the context; the engine must bisect the
	// chunk and translate the halves instead of failing.
	srv, calls := fakeTranslateServer(t, func(n int) (int, string) {
		if n == 1 {
			return 400, `{"code":40004,"msg":"context length exceeded"}`
		}
		if n == 2 {
			return 200, sseReply(t, "A\n")
		}
		return 200, sseReply(t, "B\n")
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	content := strings.Repeat("a", 30*1024) // > minChunkBytes, bisectable
	text, err := Translate(context.Background(), client, "sess-1", []byte(content), "text", Options{To: "English"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if text != "A\nB\n" {
		t.Errorf("text = %q, want %q", text, "A\nB\n")
	}
	if got := *calls; got != 3 {
		t.Errorf("completions = %d, want 3 (overflow + two halves)", got)
	}
}

// fakeTranslateServer is a minimal chat.deepseek.com stand-in for the
// engine: session create, PoW challenge, and a per-call completion handler.
func fakeTranslateServer(t *testing.T, completions func(n int) (int, string)) (*httptest.Server, *int) {
	t.Helper()
	var n int
	var mu sync.Mutex
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
			mu.Lock()
			n++
			c := n
			mu.Unlock()
			status, body := completions(c)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		case "/api/v0/chat_session/delete":
			_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// sseReply builds a single snapshot-frame SSE response carrying content.
func sseReply(t *testing.T, content string) string {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"v": map[string]any{
			"response":   map[string]any{"fragments": []any{map[string]any{"type": "response", "content": content}}, "message_id": 2},
			"message_id": 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(line) + "\n\n"
}
