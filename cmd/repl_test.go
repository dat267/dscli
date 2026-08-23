package cmd

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

// completionSSE builds a single snapshot-frame SSE response carrying content
// and an assistant message id.
func completionSSE(t *testing.T, seq int, content string) string {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"v": map[string]any{
			"response": map[string]any{
				"fragments":  []any{map[string]any{"type": "response", "content": content}},
				"message_id": seq,
			},
			"message_id": seq,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(line) + "\n\n"
}

// fakeDeepSeekServer is a minimal chat.deepseek.com stand-in: session create,
// PoW challenge issuance, completion SSE, and session delete.
func fakeDeepSeekServer(t *testing.T) (*httptest.Server, *fakeRecorder) {
	t.Helper()
	return fakeDeepSeekServerWith(t, nil)
}

// fakeDeepSeekServerWith is fakeDeepSeekServer but serves the given completion
// responses in order (falling back to testCompletionStream when exhausted).
func fakeDeepSeekServerWith(t *testing.T, completions []string) (*httptest.Server, *fakeRecorder) {
	t.Helper()
	rec := &fakeRecorder{remaining: completions}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/chat_session/create":
			_, _ = io.WriteString(w, `{"code":0,"data":{"biz_data":{"chat_session":{"id":"sess-1"}}}}`)
		case "/api/v0/chat/create_pow_challenge":
			_, _ = io.WriteString(w, testChallengeResponse)
		case "/api/v0/chat/completion":
			rec.mu.Lock()
			rec.powHeader = r.Header.Get("x-ds-pow-response")
			body, _ := io.ReadAll(r.Body)
			rec.completionBodies = append(rec.completionBodies, string(body))
			resp := testCompletionStream
			if len(rec.remaining) > 0 {
				resp = rec.remaining[0]
				rec.remaining = rec.remaining[1:]
			}
			rec.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, resp)
		case "/api/v0/chat_session/delete":
			body, _ := io.ReadAll(r.Body)
			var env struct {
				ChatSessionIDs []string `json:"chat_session_ids"`
			}
			_ = json.Unmarshal(body, &env)
			rec.mu.Lock()
			rec.deleted = append(rec.deleted, env.ChatSessionIDs...)
			rec.mu.Unlock()
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
	mu               sync.Mutex
	deleted          []string
	powHeader        string
	completionBodies []string
	remaining        []string
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
	withStdin(t, "hello\n/thinking\n/files\n/quit\n", func() {
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
		"DeepSeek · model default · thinking off · search off · files off · ephemeral (deleted on close)",
		"DeepSeek · model default · thinking on · search off · files off · ephemeral (deleted on close)", // bare /thinking flipped it
		"DeepSeek · model default · thinking on · search off · files on · ephemeral (deleted on close)",  // bare /files flipped it
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

// completionBody returns the recorded prompt and parent id of completion i.
func completionBody(t *testing.T, rec *fakeRecorder, i int) (prompt string, parent any) {
	t.Helper()
	rec.mu.Lock()
	if i >= len(rec.completionBodies) {
		t.Fatalf("completion %d not recorded (have %d)", i, len(rec.completionBodies))
	}
	raw := rec.completionBodies[i]
	rec.mu.Unlock()
	var env map[string]any
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("completion body %d not JSON: %v", i, err)
	}
	prompt, _ = env["prompt"].(string)
	return prompt, env["parent_message_id"]
}

// turnWith runs one chat turn against the fake server, capturing stdout and
// the tool notes (stderr).
func turnWith(t *testing.T, cmd *ChatCmd, client *deepseek.Client, prompt string, note *strings.Builder) (convID, stdout string, err error) {
	t.Helper()
	stdout = captureStdout(t, func() {
		captureStderr(t, func() {
			convID, err = cmd.turn(context.Background(), client, "", prompt, "default", true, func(s string) {
				note.WriteString(s + "\n")
			})
		})
	})
	return convID, stdout, err
}

// TestFileToolsReadLoop: the model asks to read a file, gets its content fed
// back, then answers in prose. The raw JSON never reaches stdout.
func TestFileToolsReadLoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello file content"), 0644); err != nil {
		t.Fatal(err)
	}
	readCall := `{"tool":"read_file","path":"a.txt"}`
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, readCall),
		completionSSE(t, 3, "Done reading."),
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	var note strings.Builder
	convID, stdout, err := turnWith(t, cmd, client, "read me the file", &note)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if stdout != "Done reading.\n" {
		t.Errorf("stdout = %q, want only the final prose", stdout)
	}
	if strings.Contains(stdout, `"tool"`) {
		t.Errorf("tool JSON leaked to stdout: %q", stdout)
	}
	if !strings.Contains(note.String(), "read_file a.txt") {
		t.Errorf("missing tool note: %q", note.String())
	}
	if convID != "sess-1:3" {
		t.Errorf("convID = %q", convID)
	}

	prompt2, parent2 := completionBody(t, rec, 1)
	if !strings.Contains(prompt2, "hello file content") {
		t.Errorf("file content not fed back in turn 2:\n%s", prompt2)
	}
	if parent2 != float64(2) {
		t.Errorf("turn 2 parent_message_id = %v, want 2 (the tool-call message)", parent2)
	}
}

// TestFileToolsEditDenied: writes always ask; when the user declines, the
// file is untouched and the model is told not to retry.
func TestFileToolsEditDenied(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("line1\nline2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	editCall := `{"tool":"edit_file","path":"a.txt","old":"line2","new":"line2 edited"}`
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, editCall),
		completionSSE(t, 3, "Understood."),
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	orig := confirmWrite
	t.Cleanup(func() { confirmWrite = orig })
	confirmWrite = func(string) bool { return false }

	var note strings.Builder
	_, stdout, err := turnWith(t, cmd, client, "edit the file", &note)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if stdout != "Understood.\n" {
		t.Errorf("stdout = %q", stdout)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "line1\nline2\n" {
		t.Errorf("file changed despite denied edit: %q", got)
	}
	if !strings.Contains(note.String(), "edit_file a.txt") {
		t.Errorf("missing edit note: %q", note.String())
	}
	prompt2, _ := completionBody(t, rec, 1)
	if !strings.Contains(prompt2, "edit rejected by user") {
		t.Errorf("model not told the edit was rejected:\n%s", prompt2)
	}
}

// TestFileToolsEditApplied: when the user confirms, the first occurrence is
// replaced and the result is fed back.
func TestFileToolsEditApplied(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("aaa\nbbb\naaa\n"), 0644); err != nil {
		t.Fatal(err)
	}
	editCall := `{"tool":"edit_file","path":"a.txt","old":"aaa","new":"AXA"}`
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, editCall),
		completionSSE(t, 3, "Edited it."),
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	orig := confirmWrite
	t.Cleanup(func() { confirmWrite = orig })
	confirmWrite = func(string) bool { return true }

	_, stdout, err := turnWith(t, cmd, client, "edit the file", &strings.Builder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if stdout != "Edited it.\n" {
		t.Errorf("stdout = %q", stdout)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "AXA\nbbb\naaa\n" {
		t.Errorf("file after confirmed edit = %q", got)
	}
	prompt2, _ := completionBody(t, rec, 1)
	if !strings.Contains(prompt2, "replaced first of 2 occurrences") {
		t.Errorf("edit summary not fed back:\n%s", prompt2)
	}
}
