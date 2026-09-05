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

// fakeSummarizeServer is fakeTranslateServer with request-body capture, so
// tests can assert which prompt each completion saw.
func fakeSummarizeServer(t *testing.T, completions func(n int) (int, string)) (*httptest.Server, *int, *[]string) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	var bodies []string
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
			n++
			c := n
			bodies = append(bodies, string(b))
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
	return srv, &n, &bodies
}

func TestSummarizePrompt(t *testing.T) {
	p := summarizePrompt("text", false, "")
	for _, want := range []string{"Summarize the following", "do not translate", "Reply with ONLY the summary"} {
		if !strings.Contains(p, want) {
			t.Errorf("summarize prompt missing %q:\n%s", want, p)
		}
	}
	// A summary never reproduces structural lines, so the preserve rules must
	// not leak into the prompt.
	if strings.Contains(summarizePrompt("srt", false, ""), "EXACTLY as they are") {
		t.Error("summarize prompt must not include structural preserve rules")
	}
	// Style is appended before the closing instruction.
	p2 := summarizePrompt("text", false, "CUSTOM STYLE BLOCK")
	if i, j := strings.Index(p2, "CUSTOM STYLE BLOCK"), strings.Index(p2, "Reply with ONLY"); i < 0 || j < 0 || i > j {
		t.Error("style must come before the closing instruction")
	}
}

func TestResolveSummarizeStyle(t *testing.T) {
	t.Run("default fallback", func(t *testing.T) {
		style, err := ResolveSummarizeStyle("")
		if err != nil {
			t.Fatalf("ResolveSummarizeStyle: %v", err)
		}
		for _, want := range []string{"SUMMARIZE", "compact", "source language"} {
			if !strings.Contains(style, want) {
				t.Errorf("default summarize style missing %q", want)
			}
		}
	})

	t.Run("explicit file", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "style.md")
		if err := os.WriteFile(p, []byte("CUSTOM SUMMARIZE RULES"), 0644); err != nil {
			t.Fatal(err)
		}
		got, err := ResolveSummarizeStyle(p)
		if err != nil || !strings.Contains(got, "CUSTOM SUMMARIZE RULES") {
			t.Errorf("explicit style = %q err=%v", got, err)
		}
		if _, err := ResolveSummarizeStyle(filepath.Join(dir, "missing.md")); err == nil {
			t.Error("missing explicit file must error")
		}
	})

	t.Run("summarize/default.md sidecar", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "sm")
		if err := os.MkdirAll(sub, 0755); err != nil {
			t.Fatal(err)
		}
		orig := summarizeStyleDirs
		t.Cleanup(func() { summarizeStyleDirs = orig })
		summarizeStyleDirs = []string{sub}
		if err := os.WriteFile(filepath.Join(sub, "default.md"), []byte("SUMMARIZE DEFAULT RULES"), 0644); err != nil {
			t.Fatal(err)
		}
		got, err := ResolveSummarizeStyle("")
		if err != nil || !strings.Contains(got, "SUMMARIZE DEFAULT RULES") {
			t.Errorf("summarize/default.md = %q err=%v", got, err)
		}
	})
}

func TestSummarizeSingleChunk(t *testing.T) {
	srv, calls, bodies := fakeSummarizeServer(t, func(n int) (int, string) {
		return 200, sseReply(t, "A terse summary.\n")
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	text, _, err := Summarize(context.Background(), client, "sess-1", []byte("hello"), "text", Options{})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if text != "A terse summary.\n" {
		t.Errorf("summary = %q", text)
	}
	if *calls != 1 {
		t.Errorf("completions = %d, want 1 (no combine pass for a single chunk)", *calls)
	}
	if !strings.Contains((*bodies)[0], "Summarize the following") {
		t.Errorf("summarize prompt not sent:\n%s", (*bodies)[0])
	}
}

func TestSummarizeMultiChunkCombines(t *testing.T) {
	// 24 KiB with no newlines: the 8 KiB probe chunk, then one 16 KiB chunk
	// (ChunkBytes caps the adaptive growth), then a combine pass.
	srv, calls, bodies := fakeSummarizeServer(t, func(n int) (int, string) {
		switch n {
		case 1:
			return 200, sseReply(t, "Section one summary.\n")
		case 2:
			return 200, sseReply(t, "Section two summary.\n")
		default:
			return 200, sseReply(t, "The combined summary.\n")
		}
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	content := strings.Repeat("a ", 12*1024)
	text, _, err := Summarize(context.Background(), client, "sess-1", []byte(content), "text", Options{ChunkBytes: 16 * 1024})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if text != "The combined summary.\n" {
		t.Errorf("summary = %q, want the combined one", text)
	}
	if *calls != 3 {
		t.Errorf("completions = %d, want 3 (two chunks + combine)", *calls)
	}
	if !strings.Contains((*bodies)[2], "single coherent summary") {
		t.Errorf("combine prompt wrong:\n%s", (*bodies)[2])
	}
	if !strings.Contains((*bodies)[2], "Section one summary.") {
		t.Errorf("combine prompt must include the section summaries:\n%s", (*bodies)[2])
	}
}

func TestSummarizeSkipsStructuralVerification(t *testing.T) {
	// A summary never reproduces SRT timing lines, so the chunk reply below
	// must be accepted without the strict verification retry translate would do.
	srt := "1\n00:00:01,000 --> 00:00:02,000\nHello there.\n\n2\n00:00:03,000 --> 00:00:04,000\nGeneral Kenobi.\n"
	srv, calls, _ := fakeSummarizeServer(t, func(n int) (int, string) {
		return 200, sseReply(t, "Two characters exchange a dramatic greeting.\n")
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	text, _, err := Summarize(context.Background(), client, "sess-1", []byte(srt), "srt", Options{})
	if err != nil {
		t.Fatalf("Summarize srt: %v", err)
	}
	if *calls != 1 {
		t.Errorf("completions = %d, want 1 (verification must be skipped for summarize)", *calls)
	}
	if !strings.Contains(text, "dramatic greeting") {
		t.Errorf("summary = %q", text)
	}
}
