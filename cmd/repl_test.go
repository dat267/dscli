package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/dat267/dscli/internal/deepseek"
)

// golden challenge: the wasm solver inverts it to nonce 999, so the fake
// server never needs to validate the x-ds-pow-response value.
const testChallengeResponse = `{"code":0,"data":{"biz_data":{"challenge":{
	"algorithm":"DeepSeekHashV1",
	"challenge":"9099d8ee62c210152bb06cf47a6071e24785ab2e6c413d4cab9bbb9f849f5a58",
	"salt":"450f343f44a1e9e6",
	"signature":"signed-request",
	"target_path":"/api/v0/chat/completion",
	"difficulty":2000,
	"expire_at":1752033600
}}}}`

// testCompletionStream ends WITHOUT a trailing newline on purpose: the REPL
// must still place the next prompt on a fresh line.
const testCompletionStream = "data: {\"v\":{\"response\":{\"fragments\":[{\"type\":\"response\",\"content\":\"Hello\"}],\"message_id\":2},\"message_id\":2}}\n\n" +
	"data: {\"p\":\"response/fragments/-1/content\",\"o\":\"APPEND\",\"v\":\" world\"}\n\n" +
	"data: {\"v\":\"!\"}\n\n"

// fakeDeepSeekServer is a minimal chat.deepseek.com stand-in: session create,
// PoW challenge issuance, completion SSE, and session delete.
func fakeDeepSeekServer(t *testing.T) (*httptest.Server, *fakeRecorder) {
	t.Helper()
	rec := &fakeRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/chat_session/create":
			_, _ = io.WriteString(w, `{"code":0,"data":{"biz_data":{"chat_session":{"id":"sess-1"}}}}`)
		case "/api/v0/chat/create_pow_challenge":
			_, _ = io.WriteString(w, testChallengeResponse)
		case "/api/v0/chat/completion":
			rec.powHeader = r.Header.Get("x-ds-pow-response")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, testCompletionStream)
		case "/api/v0/chat_session/delete":
			body, _ := io.ReadAll(r.Body)
			var env struct {
				ChatSessionIDs []string `json:"chat_session_ids"`
			}
			_ = json.Unmarshal(body, &env)
			rec.deleted = append(rec.deleted, env.ChatSessionIDs...)
			_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

type fakeRecorder struct {
	mu        sync.Mutex
	deleted   []string
	powHeader string
}

// withStdin redirects os.Stdin to a pipe containing input for the duration
// of fn.
func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, input); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	fn()
}

// TestReplUIStatelessEndToEnd drives the REPL against a fake server with the
// real wasm PoW solver and checks the output layout: a dim hint header, the
// streamed reply on stdout, a guaranteed blank line before the next prompt,
// and the session deleted on /quit.
func TestReplUIStatelessEndToEnd(t *testing.T) {
	srv, rec := fakeDeepSeekServer(t)
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{} // no conversation: ephemeral session

	var stdout, stderr string
	var runErr error
	withStdin(t, "hello\n/quit\n", func() {
		stdout = captureStdout(t, func() {
			stderr = captureStderr(t, func() {
				runErr = cmd.replLoop(context.Background(), client, "", nil)
			})
		})
	})
	if runErr != nil {
		t.Fatalf("replLoop: %v", runErr)
	}

	// The reply streams to stdout and is followed by exactly one blank line,
	// even though the model text has no trailing newline.
	if stdout != "Hello world!\n\n" {
		t.Errorf("stdout = %q, want %q", stdout, "Hello world!\n\n")
	}

	for _, want := range []string{
		"DeepSeek · model default · thinking off · search off · ephemeral (deleted on close)",
		"one question per line · /help for commands",
		"conversation: sess-1:2",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	// Piped stdin is not a terminal, so no "you> " prompt is printed.
	if strings.Contains(stderr, "you>") {
		t.Errorf("prompt printed for non-terminal stdin: %q", stderr)
	}

	// The completion carried a solved PoW header and the session was deleted.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.powHeader == "" {
		t.Error("completion request missing x-ds-pow-response")
	}
	if len(rec.deleted) != 1 || rec.deleted[0] != "sess-1" {
		t.Errorf("deleted sessions = %v, want [sess-1]", rec.deleted)
	}
}

// TestReplUIResumeSkipsDeletion: with -c the REPL must not delete the
// conversation it resumed.
func TestReplUIResumeSkipsDeletion(t *testing.T) {
	srv, rec := fakeDeepSeekServer(t)
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Conversation: "sess-9:5"}

	// conversation "sess-9:5" resumes: no session create, no delete on exit.
	var stderr string
	var runErr error
	withStdin(t, "hi\n/exit\n", func() {
		stderr = captureStderr(t, func() {
			runErr = cmd.replLoop(context.Background(), client, "sess-9:5", nil)
		})
	})
	if runErr != nil {
		t.Fatalf("replLoop: %v", runErr)
	}
	if !strings.Contains(stderr, "continuing conversation") {
		t.Errorf("stderr missing resume banner:\n%s", stderr)
	}
	if !strings.Contains(stderr, "conversation: sess-9:2") {
		t.Errorf("stderr missing resume id (new message id should win):\n%s", stderr)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.deleted) != 0 {
		t.Errorf("resumed conversation was deleted: %v", rec.deleted)
	}
}
