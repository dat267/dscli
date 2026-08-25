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

// TestSessionStaleRecoveredViaErrorFrame: the server reports the deleted
// session with an SSE error frame (as chat.deepseek.com does) instead of an
// HTTP error. The frame must be surfaced as a failure so recovery still kicks
// in — a silent "successful" empty reply would keep the stale session forever.
func TestSessionStaleRecoveredViaErrorFrame(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "dscli.json")
	if err := saveSession(cfg, "dead-sess"); err != nil {
		t.Fatal(err)
	}
	srv, rec := fakeDeepSeekServerWith(t, []string{
		"data: {\"type\":\"error\",\"content\":\"session not found\",\"finish_reason\":\"session_not_found\"}\n\n",
		completionSSE(t, 2, "recovered"),
	})
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
	if got := loadSavedSession(cfg); got != "sess-1:2" {
		t.Errorf("saved session after recovery = %q, want sess-1:2", got)
	}
	rec.mu.Lock()
	creates := rec.creates
	rec.mu.Unlock()
	if creates != 1 {
		t.Errorf("creates = %d, want 1 (recovery created a fresh session)", creates)
	}
}

// TestReplStaleRecovered: launching a chat with a persisted session that was
// deleted server-side (reported via SSE error frame) recovers to a fresh
// session: the first message is answered, the config points at the new
// conversation, and the user is told the old one was abandoned.
func TestReplStaleRecovered(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "dscli.json")
	if err := saveSession(cfg, "dead-sess"); err != nil {
		t.Fatal(err)
	}
	srv, rec := fakeDeepSeekServerWith(t, []string{
		"data: {\"type\":\"error\",\"content\":\"session not found\",\"finish_reason\":\"session_not_found\"}\n\n",
		completionSSE(t, 2, "recovered"),
	})
	defer srv.Close()
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{cfgPath: cfg}
	var stdout, stderr string
	withStdin(t, "hello\n/exit\n", func() {
		stdout = captureStdout(t, func() {
			stderr = captureStderr(t, func() {
				_ = cmd.replLoop(context.Background(), client, "dead-sess", nil, true)
			})
		})
	})
	if !strings.Contains(stdout, "recovered") {
		t.Errorf("stdout = %q, want the recovered reply", stdout)
	}
	if !strings.Contains(stderr, "no longer exists server-side") {
		t.Errorf("stderr = %q, want a stale-session note", stderr)
	}
	if got := loadSavedSession(cfg); got != "sess-1:2" {
		t.Errorf("saved session after recovery = %q, want sess-1:2", got)
	}
	rec.mu.Lock()
	creates := rec.creates
	rec.mu.Unlock()
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

// TestSessionList: list shows the locally saved sessions (transcripts) with
// their message counts and marks the persisted default.
func TestSessionList(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "dscli.json")
	appendTranscript(cfg, "sess-1", "user", "hi")
	appendTranscript(cfg, "sess-1", "assistant", "hello")
	appendTranscript(cfg, "sess-2", "user", "yo")
	if err := saveSession(cfg, "sess-1:7"); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := (&SessionListCmd{}).Run(&App{cfgPath: cfg}); err != nil {
			t.Fatalf("session list: %v", err)
		}
	})
	for _, want := range []string{"sess-1", "sess-2", "2 msgs", "1 msgs", "(default)"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	// The default marker sits on sess-1, not sess-2.
	sess1 := strings.Index(out, "sess-1")
	sess2 := strings.Index(out, "sess-2")
	if sess1 < 0 || sess2 < 0 || sess1 > sess2 {
		t.Errorf("unexpected order:\n%s", out)
	}
}

// TestSessionListEmpty: no transcripts means no sessions.
func TestSessionListEmpty(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "dscli.json")
	out := captureStdout(t, func() {
		if err := (&SessionListCmd{}).Run(&App{cfgPath: cfg}); err != nil {
			t.Fatalf("session list: %v", err)
		}
	})
	if !strings.Contains(out, "no local sessions") {
		t.Errorf("empty list output = %q", out)
	}
}

// TestSessionSelect: select saves the bare session id as the persisted
// default, reports how many saved messages it has, and warns when the session
// has no local transcript yet.
func TestSessionSelect(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "dscli.json")
	appendTranscript(cfg, "sess-9", "user", "why?")
	appendTranscript(cfg, "sess-9", "assistant", "because")
	app := &App{cfgPath: cfg}

	out := captureStdout(t, func() {
		if err := (&SessionSelectCmd{Session: "sess-9"}).Run(app); err != nil {
			t.Fatalf("session select: %v", err)
		}
	})
	if !strings.Contains(out, "selected session sess-9 (2 saved messages)") {
		t.Errorf("select output = %q", out)
	}
	if got := loadSavedSession(cfg); got != "sess-9" {
		t.Errorf("saved default = %q, want sess-9", got)
	}

	// A session:message tail is stripped — selection resumes from the root.
	if err := (&SessionSelectCmd{Session: "sess-7:123"}).Run(app); err != nil {
		t.Fatalf("session select: %v", err)
	}
	if got := loadSavedSession(cfg); got != "sess-7" {
		t.Errorf("saved default = %q, want sess-7", got)
	}

	// No local transcript: still accepted, with a note.
	noLocal := captureStdout(t, func() {
		if err := (&SessionSelectCmd{Session: "sess-new"}).Run(app); err != nil {
			t.Fatalf("session select: %v", err)
		}
	})
	if !strings.Contains(noLocal, "no local transcript yet") {
		t.Errorf("no-local output = %q", noLocal)
	}

	// Empty session: error, nothing selected.
	before := loadSavedSession(cfg)
	if err := (&SessionSelectCmd{}).Run(app); err == nil {
		t.Error("empty select should error")
	}
	if got := loadSavedSession(cfg); got != before {
		t.Errorf("saved default changed on empty select: %q", got)
	}
}

var _ = deepseek.Session{} // keep the import if assertions change
