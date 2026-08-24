package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dat267/dscli/internal/deepseek"
)

// TestSessionPersistenceReused: by default the session is created once, the
// advanced conversation position (session:message) is saved to the config, and
// the next run resumes from that exact position instead of the thread root.
func TestSessionPersistenceReused(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "dscli.json")
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, "one"),
		completionSSE(t, 3, "two"),
	})
	defer srv.Close()
	app := &App{cfgPath: cfg}

	cmd1 := &AskCmd{Prompt: []string{"first"}, Token: "tok", clientBase: srv.URL}
	if err := cmd1.Run(app, context.Background()); err != nil {
		t.Fatalf("first ask: %v", err)
	}
	rec.mu.Lock()
	creates := rec.creates
	deleted := append([]string(nil), rec.deleted...)
	rec.mu.Unlock()
	if creates != 1 || len(deleted) != 0 {
		t.Fatalf("run 1: creates=%d deleted=%v, want 1 create and 0 deletes", creates, deleted)
	}
	if got := loadSavedSession(cfg); got != "sess-1:2" {
		t.Fatalf("persisted conversation = %q, want sess-1:2", got)
	}

	cmd2 := &AskCmd{Prompt: []string{"second"}, Token: "tok", clientBase: srv.URL}
	if err := cmd2.Run(app, context.Background()); err != nil {
		t.Fatalf("second ask: %v", err)
	}
	rec.mu.Lock()
	creates2 := rec.creates
	rec.mu.Unlock()
	if creates2 != 1 {
		t.Fatalf("run 2: creates=%d, want 1 (saved session reused)", creates2)
	}
	// Run 2 resumed at message 2 of the same session — the position saved by
	// run 1, not the thread root.
	rec.mu.Lock()
	raw := rec.completionBodies[1]
	rec.mu.Unlock()
	var env map[string]any
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatal(err)
	}
	if env["chat_session_id"] != "sess-1" {
		t.Errorf("run 2 chat_session_id = %v, want sess-1", env["chat_session_id"])
	}
	if env["parent_message_id"] != float64(2) {
		t.Errorf("run 2 parent_message_id = %v, want 2 (resumed from run 1's position)", env["parent_message_id"])
	}
	// Run 2's own position is saved for the next run.
	if got := loadSavedSession(cfg); got != "sess-1:3" {
		t.Errorf("persisted conversation after run 2 = %q, want sess-1:3", got)
	}
}

// TestSessionStaleRecovered: a persisted session id that no longer exists
// server-side is abandoned; a fresh session is created, saved, and the run
// retried once.
func TestSessionStaleRecovered(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "dscli.json")
	if err := saveSession(cfg, "dead-sess"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	creates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/chat_session/create":
			mu.Lock()
			creates++
			mu.Unlock()
			_, _ = io.WriteString(w, `{"code":0,"data":{"biz_data":{"chat_session":{"id":"sess-9"}}}}`)
		case "/api/v0/chat/create_pow_challenge":
			_, _ = io.WriteString(w, testChallengeResponse)
		case "/api/v0/chat/completion":
			body, _ := io.ReadAll(r.Body)
			var env map[string]any
			_ = json.Unmarshal(body, &env)
			if env["chat_session_id"] == "dead-sess" {
				http.Error(w, `{"error":{"message":"session not found"}}`, http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, completionSSE(t, 2, "recovered"))
		case "/api/v0/chat_session/delete":
			_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app := &App{cfgPath: cfg}
	cmd := &AskCmd{Prompt: []string{"hi"}, Token: "tok", clientBase: srv.URL}
	stdout := captureStdout(t, func() {
		if err := cmd.Run(app, context.Background()); err != nil {
			t.Fatalf("ask: %v", err)
		}
	})
	if stdout != "recovered\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if got := loadSavedSession(cfg); got != "sess-9:2" {
		t.Errorf("saved session after recovery = %q, want sess-9:2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if creates != 1 {
		t.Errorf("creates = %d, want 1 (recovery created a fresh session)", creates)
	}
}

// TestReplPersistedStatusAndNoDelete: in persist mode the status line says so
// and the resumed session is not deleted on close.
func TestReplPersistedStatusAndNoDelete(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "dscli.json")
	srv, rec := fakeDeepSeekServer(t)
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{cfgPath: cfg}
	var stderr string
	var runErr error
	withStdin(t, "/exit\n", func() {
		stderr = captureStderr(t, func() {
			runErr = cmd.replLoop(context.Background(), client, "sess-1", nil, true)
		})
	})
	if runErr != nil {
		t.Fatalf("replLoop: %v", runErr)
	}
	if !strings.Contains(stderr, "persisted") {
		t.Errorf("missing persisted status:\n%s", stderr)
	}
	rec.mu.Lock()
	deleted := append([]string(nil), rec.deleted...)
	rec.mu.Unlock()
	if len(deleted) != 0 {
		t.Errorf("persisted session was deleted: %v", deleted)
	}
}

// TestReplPersistNewSessionSaved: a /new-style fresh session in persist mode is
// saved as the new default.
func TestReplPersistNewSessionSaved(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "dscli.json")
	srv, _ := fakeDeepSeekServer(t)
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{cfgPath: cfg}
	withStdin(t, "hello\n/exit\n", func() {
		captureStderr(t, func() {
			_ = cmd.replLoop(context.Background(), client, "", nil, false)
		})
	})
	if got := loadSavedSession(cfg); got != "sess-1:2" {
		t.Errorf("saved session = %q, want sess-1:2", got)
	}
}

// TestSessionShow: bare `dscli session` prints the persisted value.
func TestSessionShow(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "dscli.json")
	cmd := &SessionCmdGroup{}
	if out := captureStdout(t, func() {
		if err := cmd.Run(&App{cfgPath: cfg}); err != nil {
			t.Fatal(err)
		}
	}); out != "no persisted session\n" {
		t.Errorf("empty show = %q", out)
	}
	if err := saveSession(cfg, "sess-1:7"); err != nil {
		t.Fatal(err)
	}
	if out := captureStdout(t, func() {
		if err := cmd.Run(&App{cfgPath: cfg}); err != nil {
			t.Fatal(err)
		}
	}); out != "sess-1:7\n" {
		t.Errorf("show = %q", out)
	}
}

// TestSessionForget: forget removes the config key without touching the
// server thread.
func TestSessionForget(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "dscli.json")
	if err := saveSession(cfg, "sess-1:7"); err != nil {
		t.Fatal(err)
	}
	cmd := &SessionForgetCmd{}
	stdout := captureStdout(t, func() {
		if err := cmd.Run(&App{cfgPath: cfg}); err != nil {
			t.Fatalf("session forget: %v", err)
		}
	})
	if !strings.Contains(stdout, "forgot session sess-1:7") {
		t.Errorf("stdout = %q", stdout)
	}
	if got := loadSavedSession(cfg); got != "" {
		t.Errorf("session still tracked: %q", got)
	}
}

// TestSessionDelete: delete removes the thread server-side (the bare session
// part, without the :message tail) and clears the config key.
func TestSessionDelete(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "dscli.json")
	if err := saveSession(cfg, "sess-1:7"); err != nil {
		t.Fatal(err)
	}
	srv, rec := fakeDeepSeekServer(t)
	defer srv.Close()
	cmd := &SessionDeleteCmd{Token: "tok", clientBase: srv.URL}
	stdout := captureStdout(t, func() {
		if err := cmd.Run(&App{cfgPath: cfg}); err != nil {
			t.Fatalf("session delete: %v", err)
		}
	})
	if !strings.Contains(stdout, "deleted session sess-1 server-side") {
		t.Errorf("stdout = %q", stdout)
	}
	rec.mu.Lock()
	deleted := append([]string(nil), rec.deleted...)
	rec.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "sess-1" {
		t.Errorf("deleted server-side = %v, want [sess-1]", deleted)
	}
	if got := loadSavedSession(cfg); got != "" {
		t.Errorf("session still tracked: %q", got)
	}
}

// TestSessionDeleteNoCreds: without credentials the delete warns but still
// forgets the tracked session.
func TestSessionDeleteNoCreds(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "dscli.json")
	if err := saveSession(cfg, "sess-1"); err != nil {
		t.Fatal(err)
	}
	cmd := &SessionDeleteCmd{}
	var stderr string
	captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if err := cmd.Run(&App{cfgPath: cfg}); err != nil {
				t.Fatalf("session delete: %v", err)
			}
		})
	})
	if !strings.Contains(stderr, "no credentials") {
		t.Errorf("stderr = %q", stderr)
	}
	if got := loadSavedSession(cfg); got != "" {
		t.Errorf("session still tracked: %q", got)
	}
}

var _ = deepseek.Session{} // keep the import if assertions change
