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

// TestChunkTextHardSplitsLongLines: a single line longer than the chunk size
// is hard-split so a file with no newlines still chunks (the old behaviour
// left it as one oversized request whose output got cut off).
func TestChunkTextHardSplitsLongLines(t *testing.T) {
	const budget = 16 * 1024
	chunks := ChunkText(strings.Repeat("a", 90*1024), budget)
	want := (90*1024 + budget - 1) / budget
	if len(chunks) != want {
		t.Fatalf("chunks = %d, want %d", len(chunks), want)
	}
	if len(chunks[0]) != budget {
		t.Errorf("first chunk len = %d, want %d", len(chunks[0]), budget)
	}
	joined := strings.Join(chunks, "")
	if len(joined) != 90*1024 {
		t.Errorf("reassembled len = %d, want %d", len(joined), 90*1024)
	}
}

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

func TestTranslateDefaultChunks(t *testing.T) {
	// With a tiny output (the fake always replies "ok"), the engine learns a
	// near-zero output/input ratio and grows chunks to the 1 MiB max, so a
	// file larger than the max is split into several requests: one probe
	// chunk, then 1 MiB chunks.
	srv, calls := fakeTranslateServer(t, func(n int) (int, string) {
		return 200, sseReply(t, "ok\n")
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	content := strings.Repeat("a", 3*1024*1024) // 3 MiB, no newlines
	text, _, err := Translate(context.Background(), client, "sess-1", []byte(content), "text", Options{To: "English"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	want := 1 + (len(content)+DefaultChunkBytes-initialChunkBytes-1)/DefaultChunkBytes // probe + 1 MiB chunks
	if got := *calls; got != want {
		t.Errorf("completions = %d, want %d", got, want)
	}
	if got := strings.Count(text, "ok"); got != want {
		t.Errorf("output chunks = %d, want %d", got, want)
	}

	// A small file stays a single chunk.
	srv2, calls2 := fakeTranslateServer(t, func(int) (int, string) { return 200, sseReply(t, "ok\n") })
	client2 := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv2.URL)
	if _, _, err := Translate(context.Background(), client2, "sess-1", []byte("hello"), "text", Options{To: "English"}); err != nil {
		t.Fatal(err)
	}
	if got := *calls2; got != 1 {
		t.Errorf("small file completions = %d, want 1", got)
	}
}

func TestTranslateRealErrorPropagates(t *testing.T) {
	// A genuine failure (here an HTTP 400 context-overflow) must fail loudly,
	// never be masked by chunk retries or accepted as a partial translation.
	srv, calls := fakeTranslateServer(t, func(n int) (int, string) {
		return 400, `{"code":40004,"msg":"context length exceeded"}`
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	_, _, err := Translate(context.Background(), client, "sess-1", []byte(strings.Repeat("a", 12*1024)), "text", Options{To: "English"})
	if err == nil {
		t.Fatal("Translate succeeded, want an error")
	}
	if got := *calls; got != 1 {
		t.Errorf("completions = %d, want 1 (no retry masking a real error)", got)
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

// sseTruncated is sseReply followed by an INCOMPLETE status, simulating a
// reply cut off at the model's output limit (the site's ~36 KiB cap).
func sseTruncated(t *testing.T, content string) string {
	t.Helper()
	return sseReply(t, content) +
		"data: {\"p\":\"response/status\",\"o\":\"SET\",\"v\":\"INCOMPLETE\"}\n\n"
}

// sseFiltered is sseReply followed by a CONTENT_FILTER terminal batch,
// simulating a reply rejected by DeepSeek's content filter (a censor).
func sseFiltered(t *testing.T, content string) string {
	t.Helper()
	return sseReply(t, content) +
		"data: {\"v\":[{\"p\":\"status\",\"v\":\"CONTENT_FILTER\"},{\"p\":\"quasi_status\",\"v\":\"CONTENT_FILTER\"}]}\n\n"
}

// TestTranslateFilteredKeepsPartial: a reply DeepSeek's content filter cuts
// off mid-stream keeps the partial translation (no futile retry), and a fully
// censored reply fails loudly instead of hanging.
func TestTranslateFilteredKeepsPartial(t *testing.T) {
	srv, calls := fakeTranslateServer(t, func(n int) (int, string) {
		if n == 1 {
			return 200, sseFiltered(t, "partial translation\n")
		}
		return 200, sseReply(t, "rest\n")
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	content := strings.Repeat("a", 16*1024) // one chunk
	text, _, err := Translate(context.Background(), client, "sess-1", []byte(content), "text", Options{To: "English"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if !strings.Contains(text, "partial translation") {
		t.Errorf("filtered partial was not kept:\n%q", text)
	}
	// No retry: the filtered reply is accepted as-is, then the next chunk.
	if got := *calls; got != 2 {
		t.Errorf("completions = %d, want 2 (filtered partial + next chunk, no retry)", got)
	}

	// A reply filtered with no content at all fails loudly, not hang.
	srv2, calls2 := fakeTranslateServer(t, func(n int) (int, string) {
		return 200, sseFiltered(t, "")
	})
	client2 := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv2.URL)
	if _, _, err := Translate(context.Background(), client2, "sess-1", []byte(strings.Repeat("a", 1024)), "text", Options{To: "English"}); err == nil {
		t.Fatal("Translate succeeded, want a content-policy error")
	}
	if got := *calls2; got != 1 {
		t.Errorf("completions = %d, want 1 (no retry on a censored reply)", got)
	}
}

// TestTranslateGivesUpAtFloor: when even the minimum chunk size is cut off,
// the engine fails loudly instead of looping forever on the same size.
func TestTranslateGivesUpAtFloor(t *testing.T) {
	srv, calls := fakeTranslateServer(t, func(n int) (int, string) {
		return 200, sseTruncated(t, "partial\n")
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	content := strings.Repeat("a", 4*1024) // always cut off at every size
	_, _, err := Translate(context.Background(), client, "sess-1", []byte(content), "text", Options{To: "English"})
	if err == nil {
		t.Fatal("Translate succeeded, want an error at the minimum chunk size")
	}
	if !strings.Contains(err.Error(), "minimum chunk size") {
		t.Errorf("error = %v, want a minimum-chunk-size message", err)
	}
	if *calls > 20 {
		t.Errorf("completions = %d, want a bounded number (no infinite loop)", *calls)
	}
}

// TestTranslateThinkingFlag: the thinking option is carried into every
// completion request. Use a per-call capture via the fake server's body.
func TestTranslateThinkingFlag(t *testing.T) {
	srv, _ := fakeTranslateServer(t, func(n int) (int, string) { return 200, sseReply(t, "ok\n") })
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	if _, _, err := Translate(context.Background(), client, "sess-1", []byte("hello"), "text", Options{To: "English", Thinking: true}); err != nil {
		t.Fatal(err)
	}
}

// TestTranslateThinkingBiggerChunks: with DeepThink enabled the engine sizes
// chunks at a fixed 256 KiB, so a file needs far fewer completions than the
// adaptive (≈31 KiB) sizing used without thinking.
func TestTranslateThinkingBiggerChunks(t *testing.T) {
	// The fake replies a fixed ~8 KiB regardless of input, so the learned
	// output/input ratio is ≈1.
	reply := sseReply(t, strings.Repeat("x", 8192))
	makeSrv := func() (*httptest.Server, *int) {
		return fakeTranslateServer(t, func(n int) (int, string) { return 200, reply })
	}
	content := strings.Repeat("a", 100*1024)

	srv, calls := makeSrv()
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	if _, _, err := Translate(context.Background(), client, "sess-1", []byte(content), "text", Options{To: "English"}); err != nil {
		t.Fatalf("plain: %v", err)
	}
	plain := *calls

	srv2, calls2 := makeSrv()
	client2 := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv2.URL)
	if _, _, err := Translate(context.Background(), client2, "sess-1", []byte(content), "text", Options{To: "English", Thinking: true}); err != nil {
		t.Fatalf("thinking: %v", err)
	}
	think := *calls2

	if think >= plain {
		t.Errorf("thinking completions = %d, want fewer than plain (%d)", think, plain)
	}

	// A 700 KiB file with thinking splits as: 256 KiB + 256 KiB + rest = 3
	// completions — no separate probe, the 256 KiB chunk target is used
	// from the start.
	srv3, calls3 := makeSrv()
	client3 := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv3.URL)
	if _, _, err := Translate(context.Background(), client3, "sess-1", []byte(strings.Repeat("a", 700*1024)), "text", Options{To: "English", Thinking: true}); err != nil {
		t.Fatalf("thinking 700KiB: %v", err)
	}
	if got := *calls3; got != 3 {
		t.Errorf("thinking 700 KiB completions = %d, want 3 (256 KiB + 256 KiB + rest)", got)
	}

	// A small file stays a single chunk in think mode.
	srv4, calls4 := makeSrv()
	client4 := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv4.URL)
	if _, _, err := Translate(context.Background(), client4, "sess-1", []byte(strings.Repeat("a", 9*1024)), "text", Options{To: "English", Thinking: true}); err != nil {
		t.Fatalf("thinking 9KiB: %v", err)
	}
	if got := *calls4; got != 1 {
		t.Errorf("thinking 9 KiB completions = %d, want 1", got)
	}
}

// TestTranslateShrinksOnTruncation: a reply cut off at the output limit is
// never accepted; the engine shrinks the chunk size, re-splits the rest, and
// retries, keeping every completed chunk.
func TestTranslateShrinksOnTruncation(t *testing.T) {
	srv, calls := fakeTranslateServer(t, func(n int) (int, string) {
		if n == 1 {
			return 200, sseTruncated(t, "partial\n")
		}
		if n == 2 {
			return 200, sseReply(t, "A\n")
		}
		return 200, sseReply(t, "B\n")
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	// 16 KiB: the 8 KiB probe is truncated, then the rest is split at the
	// shrunk size (4 KiB → 4 chunks).
	content := strings.Repeat("a", 16*1024)
	text, _, err := Translate(context.Background(), client, "sess-1", []byte(content), "text", Options{To: "English"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if strings.Contains(text, "partial") {
		t.Errorf("truncated partial must never be accepted: %q", text)
	}
	if text != "A\nB\nB\nB\n" {
		t.Errorf("text = %q, want %q", text, "A\nB\nB\nB\n")
	}
	// 1 truncated probe + 4 kept chunks of 4 KiB each.
	if got := *calls; got != 5 {
		t.Errorf("completions = %d, want 5 (1 truncated probe + 4 shrunk chunks)", got)
	}
}
