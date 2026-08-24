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
	"github.com/dat267/dscli/internal/filetools"
	"github.com/dat267/dscli/internal/translate"
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
			rec.mu.Lock()
			rec.creates++
			rec.mu.Unlock()
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
	creates          int
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

// TestReplMultiline: a line ending in a single backslash continues the
// message onto the next line, joined with a newline; a lone "\" line inserts
// a blank line and keeps going; "\\" at the end sends a literal backslash and
// ends the message. Continuation lines are not treated as commands.
func TestReplMultiline(t *testing.T) {
	srv, rec := fakeDeepSeekServer(t)
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{}

	// first " \  second  \  (blank)  third  \  literal-backslash  /quit
	// Escapes: "\\" is one backslash, "\n" a newline. The trailing "\\"
	// (two backslashes) does NOT continue — the line is sent literally.
	input := "first \\\nsecond \\\n\\\nthird \\\\\n/quit\n"
	withStdin(t, input, func() {
		captureStdout(t, func() {
			captureStderr(t, func() {
				_ = cmd.replLoop(context.Background(), client, "sess-1", nil, false)
			})
		})
	})

	prompt, _ := completionBody(t, rec, 0)
	want := "first \nsecond \n\nthird \\\\"
	if prompt != want {
		t.Errorf("multiline prompt = %q, want %q", prompt, want)
	}
}

// TestReplMultilinePipedEndsAtEOF: when stdin ends mid-continuation, the
// accumulated message is still sent rather than dropped.
func TestReplMultilinePipedEndsAtEOF(t *testing.T) {
	srv, rec := fakeDeepSeekServer(t)
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{}

	withStdin(t, "just a continuation \\\nand its tail", func() {
		captureStdout(t, func() {
			captureStderr(t, func() {
				_ = cmd.replLoop(context.Background(), client, "sess-1", nil, false)
			})
		})
	})
	prompt, _ := completionBody(t, rec, 0)
	if want := "just a continuation \nand its tail"; prompt != want {
		t.Errorf("prompt = %q, want %q", prompt, want)
	}
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
	withStdin(t, "hello\n/thinking\n/tools\n/quit\n", func() {
		stdout = captureStdout(t, func() {
			stderr = captureStderr(t, func() {
				runErr = cmd.replLoop(context.Background(), client, "", nil, false)
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
		"DeepSeek · model default · thinking off · search off · tools off · ephemeral",
		"DeepSeek · model default · thinking on · search off · tools off · ephemeral", // bare /thinking flipped it
		"DeepSeek · model default · thinking on · search off · tools on · ephemeral",  // bare /files flipped it
		"one question per line · /help for commands",
		"conversation: sess-1:2",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	// Piped stdin is not a terminal, so no read prompt is ever printed.
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
			runErr = cmd.replLoop(context.Background(), client, "sess-9:5", nil, false)
		})
	})
	if runErr != nil {
		t.Fatalf("replLoop: %v", runErr)
	}
	if !strings.Contains(stderr, "continuing") {
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

// turnWith runs one chat turn against the fake server, capturing stdout,
// stderr (including the edit preview) and the tool notes.
func turnWith(t *testing.T, cmd *ChatCmd, client *deepseek.Client, prompt string, note *strings.Builder) (convID, stdout, stderr string, err error) {
	t.Helper()
	stdout = captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			var sources []deepseek.Source
			convID, err = cmd.turn(context.Background(), client, "", prompt, "default", true, func(s string) {
				note.WriteString(s + "\n")
			}, &sources)
		})
	})
	return convID, stdout, stderr, err
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
	convID, stdout, _, err := turnWith(t, cmd, client, "read me the file", &note)
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
	_, stdout, stderr, err := turnWith(t, cmd, client, "edit the file", &note)
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
	// The planned change is still previewed before the confirmation, and the
	// preview is accurate to the operation that was declined.
	for _, want := range []string{"replacing first of 1 occurrence(s)", "-  2 │ line2", "+  2 │ line2 edited"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing preview %q:\n%s", want, stderr)
		}
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

	_, stdout, stderr, err := turnWith(t, cmd, client, "edit the file", &strings.Builder{})
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
	// The preview showed exactly what was applied: first occurrence of "aaa"
	// on line 1 → "AXA", and 2 total occurrences.
	for _, want := range []string{"replacing first of 2 occurrence(s)", "-  1 │ aaa", "+  1 │ AXA"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing preview %q:\n%s", want, stderr)
		}
	}
	prompt2, _ := completionBody(t, rec, 1)
	if !strings.Contains(prompt2, "replaced first of 2 occurrences") {
		t.Errorf("edit summary not fed back:\n%s", prompt2)
	}
}

// TestFileToolsListLoop: the model lists the workdir, gets the listing fed
// back, then answers in prose.
func TestFileToolsListLoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "cmd"), 0755); err != nil {
		t.Fatal(err)
	}
	listCall := `{"tool":"list_directory","path":"."}`
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, listCall),
		completionSSE(t, 3, "Found a.txt and cmd/."),
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	var note strings.Builder
	convID, stdout, _, err := turnWith(t, cmd, client, "what files are here?", &note)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if stdout != "Found a.txt and cmd/.\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if !strings.Contains(note.String(), "list_directory .") {
		t.Errorf("missing list note: %q", note.String())
	}
	if convID != "sess-1:3" {
		t.Errorf("convID = %q", convID)
	}
	prompt2, _ := completionBody(t, rec, 1)
	for _, want := range []string{"a.txt", "cmd/" /* dir marker */} {
		if !strings.Contains(prompt2, want) {
			t.Errorf("listing not fed back (missing %q):\n%s", want, prompt2)
		}
	}
}

// TestFileToolsCreateLoop: creating a new file goes through preview + confirm
// and lands exactly the model-supplied content.
func TestFileToolsCreateLoop(t *testing.T) {
	dir := t.TempDir()
	createCall := `{"tool":"create_file","path":"new.txt","content":"line1\nline2"}`
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, createCall),
		completionSSE(t, 3, "Created."),
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	orig := confirmWrite
	t.Cleanup(func() { confirmWrite = orig })
	confirmWrite = func(string) bool { return true }

	var note strings.Builder
	_, stdout, stderr, err := turnWith(t, cmd, client, "create a file", &note)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if stdout != "Created.\n" {
		t.Errorf("stdout = %q", stdout)
	}
	for _, want := range []string{"new.txt — creating (2 lines, 11 bytes)", "+  1 │ line1", "+  2 │ line2"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil || string(got) != "line1\nline2" {
		t.Errorf("created file = %q err=%v", got, err)
	}
	prompt2, _ := completionBody(t, rec, 1)
	if !strings.Contains(prompt2, "created new.txt (11 bytes)") {
		t.Errorf("create result not fed back:\n%s", prompt2)
	}
}

// TestFileToolsDeleteLoop: deleting goes through preview + confirm, and a
// denial leaves the file untouched.
func TestFileToolsDeleteLoop(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(f, []byte("alpha\nbeta\n"), 0644); err != nil {
		t.Fatal(err)
	}
	deleteCall := `{"tool":"delete_file","path":"old.txt"}`

	orig := confirmWrite
	t.Cleanup(func() { confirmWrite = orig })

	// Denied: file stays, model told to stop.
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, deleteCall),
		completionSSE(t, 3, "Deleted."),
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}
	confirmWrite = func(string) bool { return false }

	var note strings.Builder
	_, stdout, stderr, err := turnWith(t, cmd, client, "delete the file", &note)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if stdout != "Deleted.\n" {
		t.Errorf("stdout = %q", stdout)
	}
	for _, want := range []string{"old.txt — deleting", "-  1 │ alpha"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	if _, err := os.Stat(f); err != nil {
		t.Errorf("denied delete must leave the file, got %v", err)
	}
	if !strings.Contains(note.String(), "delete_file old.txt") {
		t.Errorf("missing delete note: %q", note.String())
	}
	prompt2, _ := completionBody(t, rec, 1)
	if !strings.Contains(prompt2, "delete rejected by user") {
		t.Errorf("model not told about the rejection:\n%s", prompt2)
	}

	// Accepted: file gone, result fed back (fresh fake server — the first
	// one's completion queue is consumed).
	if err := os.WriteFile(f, []byte("alpha\nbeta\n"), 0644); err != nil {
		t.Fatal(err)
	}
	srv2, rec2 := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, deleteCall),
		completionSSE(t, 3, "Deleted."),
	})
	client2 := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv2.URL)
	cmd2 := &ChatCmd{Workdir: dir}
	confirmWrite = func(string) bool { return true }

	var note2 strings.Builder
	_, _, _, err = turnWith(t, cmd2, client2, "delete the file", &note2)
	if err != nil {
		t.Fatalf("turn (accepted): %v", err)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Errorf("accepted delete must remove the file, got %v", err)
	}
	promptResult, _ := completionBody(t, rec2, 1)
	if !strings.Contains(promptResult, "deleted old.txt") {
		t.Errorf("delete result not fed back:\n%s", promptResult)
	}
}

// TestFileToolsBinaryReadRejected: reading a binary file errors at the tool
// level and the rejection is fed back to the model.
func TestFileToolsBinaryReadRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0x7f, 'E', 'L', 'F', 0x00, 0x01}, 0644); err != nil {
		t.Fatal(err)
	}
	readCall := `{"tool":"read_file","path":"blob.bin"}`
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, readCall),
		completionSSE(t, 3, "I can't read that file."),
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	var note strings.Builder
	_, stdout, _, err := turnWith(t, cmd, client, "read blob.bin", &note)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if stdout != "I can't read that file.\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if !strings.Contains(note.String(), "read_file blob.bin") {
		t.Errorf("missing read note: %q", note.String())
	}
	prompt2, _ := completionBody(t, rec, 1)
	if !strings.Contains(prompt2, "not a text file") {
		t.Errorf("binary rejection not fed back:\n%s", prompt2)
	}
}

// TestFileToolsGrepLoop: the model greps the workdir, gets the matches fed
// back, then answers in prose.
func TestFileToolsGrepLoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha\nbeta\n"), 0644); err != nil {
		t.Fatal(err)
	}
	grepCall := `{"tool":"grep","pattern":"alpha"}`
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, grepCall),
		completionSSE(t, 3, "Found it."),
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	var note strings.Builder
	_, stdout, _, err := turnWith(t, cmd, client, "find alpha", &note)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if stdout != "Found it.\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if !strings.Contains(note.String(), "grep .") {
		t.Errorf("missing grep note: %q", note.String())
	}
	prompt2, _ := completionBody(t, rec, 1)
	if !strings.Contains(prompt2, "a.txt:1:alpha") {
		t.Errorf("grep results not fed back:\n%s", prompt2)
	}
}

// TestFileToolsFetchLoop: the model fetches a URL, gets the body fed back,
// then answers in prose.
func TestFileToolsFetchLoop(t *testing.T) {
	content := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "THE PAGE BODY")
	}))
	defer content.Close()

	dir := t.TempDir()
	fetchCall := `{"tool":"fetch_url","url":"` + content.URL + `/page"}`
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, fetchCall),
		completionSSE(t, 3, "Got the page."),
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	var note strings.Builder
	_, stdout, _, err := turnWith(t, cmd, client, "fetch the page", &note)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if stdout != "Got the page.\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if !strings.Contains(note.String(), "fetch_url "+content.URL+"/page") {
		t.Errorf("missing fetch note: %q", note.String())
	}
	prompt2, _ := completionBody(t, rec, 1)
	if !strings.Contains(prompt2, "THE PAGE BODY") {
		t.Errorf("fetch content not fed back:\n%s", prompt2)
	}
}

// TestFileToolsDuplicateReadOnlySkipped: a model that repeats the exact same
// deterministic read-only call (list_directory . twice in a row) has the
// duplicate skipped and is told to reuse the result it already has, instead
// of burning a tool-call budget slot.
func TestFileToolsDuplicateReadOnlySkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	listCall := `{"tool":"list_directory","path":"."}`
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, listCall),
		completionSSE(t, 3, listCall),
		completionSSE(t, 4, "Done."),
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	var note strings.Builder
	_, stdout, _, err := turnWith(t, cmd, client, "what files?", &note)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if stdout != "Done.\n" {
		t.Errorf("stdout = %q", stdout)
	}
	// The repeated call is flagged, not silently re-run.
	if !strings.Contains(note.String(), "list_directory . (duplicate, skipped)") {
		t.Errorf("missing duplicate note: %q", note.String())
	}
	// The model is pointed at the result it already has (a fresh listing was
	// NOT executed a second time).
	prompt2, _ := completionBody(t, rec, 2)
	if !strings.Contains(prompt2, "already requested this exact tool call") {
		t.Errorf("duplicate hint not fed back:\n%s", prompt2)
	}
	if strings.Contains(prompt2, "a.txt") {
		t.Errorf("duplicate listing was re-executed:\n%s", prompt2)
	}
}

// TestFileToolsDuplicateResetByOtherTool: the dedup only covers consecutive
// identical calls — a list → edit → list sequence runs both listings.
func TestFileToolsDuplicateResetByOtherTool(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	listCall := `{"tool":"list_directory","path":"."}`
	editCall := `{"tool":"edit_file","path":"a.txt","old":"x","new":"y"}`
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, listCall),
		completionSSE(t, 3, editCall),
		completionSSE(t, 4, listCall),
		completionSSE(t, 5, "Done."),
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	orig := confirmWrite
	t.Cleanup(func() { confirmWrite = orig })
	confirmWrite = func(string) bool { return true }

	var note strings.Builder
	_, stdout, _, err := turnWith(t, cmd, client, "list then edit then list", &note)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if stdout != "Done.\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if strings.Contains(note.String(), "duplicate, skipped") {
		t.Errorf("second list was wrongly deduplicated: %q", note.String())
	}
	if n := strings.Count(note.String(), "list_directory ."); n != 2 {
		t.Errorf("expected 2 list notes, got %d: %q", n, note.String())
	}
	// The second listing really ran (its result was fed back).
	prompt3, _ := completionBody(t, rec, 3)
	if !strings.Contains(prompt3, "a.txt") {
		t.Errorf("second listing result not fed back:\n%s", prompt3)
	}
}

// TestFileToolsLoopBounded: a model that keeps calling tools (ever-growing
// exploration) is hard-capped — after the tool budget it is forced to give a
// final answer, so one user message can never loop indefinitely.
func TestFileToolsLoopBounded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	readCall := `{"tool":"read_file","path":"a.txt"}`
	completions := make([]string, 0, filetools.MaxIterations+1)
	for i := 0; i < filetools.MaxIterations; i++ { // the model never answers in prose
		completions = append(completions, completionSSE(t, 2, readCall))
	}
	completions = append(completions, completionSSE(t, 3, "Final answer."))

	srv, rec := fakeDeepSeekServerWith(t, completions)
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	var note strings.Builder
	convID, stdout, _, err := turnWith(t, cmd, client, "explore everything", &note)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if stdout != "Final answer.\n" {
		t.Errorf("stdout = %q, want the forced final answer", stdout)
	}
	if convID != "sess-1:3" {
		t.Errorf("convID = %q", convID)
	}
	rec.mu.Lock()
	n := len(rec.completionBodies)
	rec.mu.Unlock()
	if want := filetools.MaxIterations + 1; n != want {
		t.Errorf("completions = %d, want %d (tool budget + forced final)", n, want)
	}
	final, _ := completionBody(t, rec, filetools.MaxIterations)
	if !strings.Contains(final, "final answer") {
		t.Errorf("forced final prompt wrong: %q", final)
	}
}

// TestFileToolsRenameLoop: renaming goes through preview + confirm and lands
// the move; a denial leaves everything untouched.
func TestFileToolsRenameLoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	renameCall := `{"tool":"rename_file","path":"old.txt","new":"new.txt"}`
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, renameCall),
		completionSSE(t, 3, "Renamed."),
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	orig := confirmWrite
	t.Cleanup(func() { confirmWrite = orig })

	// Denied: nothing moves.
	confirmWrite = func(string) bool { return false }
	var note strings.Builder
	_, stdout, stderr, err := turnWith(t, cmd, client, "rename old.txt", &note)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if stdout != "Renamed.\n" {
		t.Errorf("stdout = %q", stdout)
	}
	for _, want := range []string{"old.txt → new.txt", "(file", "bytes)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "old.txt")); err != nil {
		t.Errorf("denied rename must keep the file: %v", err)
	}
	prompt2, _ := completionBody(t, rec, 1)
	if !strings.Contains(prompt2, "rename rejected by user") {
		t.Errorf("rejection not fed back:\n%s", prompt2)
	}

	// Accepted (fresh server, fresh file): the move lands.
	if err := os.WriteFile(filepath.Join(dir, "old.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	srv2, rec2 := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, renameCall),
		completionSSE(t, 3, "Renamed."),
	})
	client2 := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv2.URL)
	confirmWrite = func(string) bool { return true }
	_, _, _, err = turnWith(t, cmd, client2, "rename old.txt", &strings.Builder{})
	if err != nil {
		t.Fatalf("turn (accepted): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil {
		t.Errorf("accepted rename must move the file: %v", err)
	}
	promptResult, _ := completionBody(t, rec2, 1)
	if !strings.Contains(promptResult, "renamed old.txt → new.txt") {
		t.Errorf("rename result not fed back:\n%s", promptResult)
	}
}

// TestFileToolsRecursiveListLoop: one recursive list call maps the whole
// subtree — the model then answers without spending further list calls.
func TestFileToolsRecursiveListLoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "books", "scifi"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "books", "scifi", "dune.epub"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	listCall := `{"tool":"list_directory","path":".","recursive":true}`
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, listCall),
		completionSSE(t, 3, "I can see the whole tree."),
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	var note strings.Builder
	_, stdout, _, err := turnWith(t, cmd, client, "what's here?", &note)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if stdout != "I can see the whole tree.\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if !strings.Contains(note.String(), "list_directory .") {
		t.Errorf("missing list note: %q", note.String())
	}
	// Exactly two completions: one recursive list + the final answer. The
	// model never had to list directories one by one.
	rec.mu.Lock()
	n := len(rec.completionBodies)
	rec.mu.Unlock()
	if n != 2 {
		t.Errorf("completions = %d, want 2 (recursive list + final answer)", n)
	}
	prompt2, _ := completionBody(t, rec, 1)
	for _, want := range []string{"README.md", "books/", "scifi/", "dune.epub"} {
		if !strings.Contains(prompt2, want) {
			t.Errorf("tree not fed back (missing %q):\n%s", want, prompt2)
		}
	}
}

// TestFileToolsTranslateLoop: the model calls translate_file in chat mode;
// the CLI previews, confirms, translates in a dedicated session, writes the
// output, and the model continues in prose.
func TestFileToolsTranslateLoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "song.lrc"), []byte("[00:01.00]hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	style := filepath.Join(dir, "style.md")
	if err := os.WriteFile(style, []byte("LYRIC-SPECIFIC STYLE"), 0644); err != nil {
		t.Fatal(err)
	}
	translateCall := `{"tool":"translate_file","path":"song.lrc","from":"Japanese","to":"French"}`
	// Completions: 0 = the tool call; 1 = the engine's chunk translation;
	// 2 = the model's final prose answer.
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, translateCall),
		completionSSE(t, 3, "[00:01.00]bonjour\n"),
		completionSSE(t, 4, "Translated."),
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir, Instructions: style}

	orig := confirmWrite
	t.Cleanup(func() { confirmWrite = orig })
	confirmWrite = func(string) bool { return true }

	var note strings.Builder
	convID, stdout, stderr, err := turnWith(t, cmd, client, "translate song.lrc to French", &note)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if stdout != "Translated.\n" {
		t.Errorf("stdout = %q", stdout)
	}
	for _, want := range []string{"song.lrc → song.translated.fr.lrc", "(lrc, 1 chunks, to French)", "chunk 1/1 ok"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, "song.translated.fr.lrc"))
	if err != nil || string(got) != "[00:01.00]bonjour\n" {
		t.Errorf("translated file = %q err=%v", got, err)
	}
	if convID != "sess-1:4" {
		t.Errorf("convID = %q", convID)
	}
	// The dedicated translate session is cleaned up (the chat owner session
	// is the repl's job and is covered by the stateless e2e).
	rec.mu.Lock()
	deleted := append([]string(nil), rec.deleted...)
	rec.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "sess-1" {
		t.Errorf("translate session not cleaned up: %v", deleted)
	}
	// The engine's chunk prompt carried the language, LRC rules and the
	// custom style.
	prompt1, _ := completionBody(t, rec, 1)
	if !strings.Contains(prompt1, "LRC lyrics") || !strings.Contains(prompt1, "French") {
		t.Errorf("chunk prompt wrong:\n%s", prompt1)
	}
	if !strings.Contains(prompt1, "LYRIC-SPECIFIC STYLE") {
		t.Errorf("custom style missing from chunk prompt:\n%s", prompt1)
	}
}

// TestFileToolsTranslateDenied: a rejected translation writes nothing and the
// model is told not to retry.
func TestFileToolsTranslateDenied(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("Hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	translateCall := `{"tool":"translate_file","path":"a.md","to":"Spanish"}`
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, translateCall),
		completionSSE(t, 3, "OK."),
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	orig := confirmWrite
	t.Cleanup(func() { confirmWrite = orig })
	confirmWrite = func(string) bool { return false }

	_, _, _, err := turnWith(t, cmd, client, "translate a.md", &strings.Builder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "a.translated.es.md")); !os.IsNotExist(statErr) {
		t.Error("denied translate must not write output")
	}
	prompt1, _ := completionBody(t, rec, 1)
	if !strings.Contains(prompt1, "translate rejected by user") {
		t.Errorf("rejection not fed back:\n%s", prompt1)
	}
}

// TestFileToolsTranslateTooLarge: oversized input refuses at plan time.
func TestFileToolsTranslateTooLarge(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", translate.MaxChatInputBytes+100)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0644); err != nil {
		t.Fatal(err)
	}
	translateCall := `{"tool":"translate_file","path":"big.txt","to":"French"}`
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, translateCall),
		completionSSE(t, 3, "Too big."),
	})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	orig := confirmWrite
	t.Cleanup(func() { confirmWrite = orig })
	confirmWrite = func(string) bool { return true }

	_, _, _, err := turnWith(t, cmd, client, "translate big.txt", &strings.Builder{})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	prompt1, _ := completionBody(t, rec, 1)
	if !strings.Contains(prompt1, "translate limit") {
		t.Errorf("size limit not fed back:\n%s", prompt1)
	}
}
