package cmd

import (
	"context"
	"encoding/json"
	"fmt"
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
		case "/api/v0/file/upload_file":
			rec.mu.Lock()
			rec.uploads = append(rec.uploads, r.Header.Get("x-file-size"))
			id := fmt.Sprintf("file-%d", len(rec.uploads))
			rec.mu.Unlock()
			_, _ = io.WriteString(w, `{"code":0,"data":{"biz_data":{"id":"`+id+`","status":"PENDING","file_name":"x"}}}`)
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
	uploads          []string // x-file-size per uploaded file
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
	withStdin(t, "hello\n/thinking\n/quit\n", func() {
		stdout = captureStdout(t, func() {
			stderr = captureStderr(t, func() {
				runErr = cmd.replLoop(context.Background(), client, "", nil, false)
			})
		})
	})
	if runErr != nil {
		t.Fatalf("replLoop: %v", runErr)
	}

	// Each turn's reply is separated from the echoed prompt by a blank line
	// above and exactly one blank line below, even though the model text has
	// no trailing newline.
	if stdout != "\nHello world!\n\n" {
		t.Errorf("stdout = %q, want %q", stdout, "\nHello world!\n\n")
	}

	for _, want := range []string{
		"DeepSeek · model default · thinking off · search off · ephemeral",
		"DeepSeek · model default · thinking on · search off · ephemeral", // bare /thinking flipped it
		// The redraw is a block: a blank line separates it from the echoed
		// command above and the next prompt below.
		"\n\nDeepSeek · model default · thinking on · search off · ephemeral\n\n",
		"one question per line · /help for commands\n\n",
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

// completionRefIDs returns the ref_file_ids of the i-th completion body.
func completionRefIDs(t *testing.T, rec *fakeRecorder, i int) []string {
	t.Helper()
	rec.mu.Lock()
	if i >= len(rec.completionBodies) {
		rec.mu.Unlock()
		t.Fatalf("completion %d not recorded", i)
	}
	raw := rec.completionBodies[i]
	rec.mu.Unlock()
	var env struct {
		RefFileIDs []string `json:"ref_file_ids"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("completion body %d not JSON: %v", i, err)
	}
	return env.RefFileIDs
}

// completionFlags returns the thinking_enabled/search_enabled flags of the
// i-th completion body.
func completionFlags(t *testing.T, rec *fakeRecorder, i int) (thinking, search bool) {
	t.Helper()
	rec.mu.Lock()
	if i >= len(rec.completionBodies) {
		rec.mu.Unlock()
		t.Fatalf("completion %d not recorded", i)
	}
	raw := rec.completionBodies[i]
	rec.mu.Unlock()
	var env struct {
		ThinkingEnabled bool `json:"thinking_enabled"`
		SearchEnabled   bool `json:"search_enabled"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("completion body %d not JSON: %v", i, err)
	}
	return env.ThinkingEnabled, env.SearchEnabled
}

// TestReplTogglesAffectRequests: /thinking and /search must change the flags
// the completion actually sends, not just the status line.
func TestReplTogglesAffectRequests(t *testing.T) {
	srv, rec := fakeDeepSeekServer(t)
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{}

	withStdin(t, "/thinking\n/search\nhi\n/quit\n", func() {
		captureStdout(t, func() {
			captureStderr(t, func() {
				_ = cmd.replLoop(context.Background(), client, "sess-1", nil, false)
			})
		})
	})

	thinking, search := completionFlags(t, rec, 0)
	if !thinking || !search {
		t.Errorf("completion flags thinking=%v search=%v, want both true", thinking, search)
	}
}

// TestPromptRecolor: the echoed prompt is re-rendered in the user colour by
// erasing the echoed line(s) and reprinting them — but only when every
// physical line fits the terminal width (otherwise the echo wrapped and
// rewriting would leave artifacts).
func TestPromptRecolor(t *testing.T) {
	u := ui{color: true}
	got := promptRecolor(u, []string{"hi"}, 80)
	want := "\x1b[1F\x1b[2K" + u.cyan("hi") + "\n"
	if got != want {
		t.Errorf("promptRecolor = %q, want %q", got, want)
	}
	// Multiple lines (not used for continuations, but supported).
	got = promptRecolor(u, []string{"one", "two"}, 80)
	if got != "\x1b[1F\x1b[2K\x1b[1F\x1b[2K"+u.cyan("one")+"\n"+u.cyan("two")+"\n" {
		t.Errorf("promptRecolor multi = %q", got)
	}
	// A line that cannot fit the width is left alone (the echo wrapped).
	if got := promptRecolor(u, []string{strings.Repeat("x", 81)}, 80); got != "" {
		t.Errorf("promptRecolor over-wide = %q, want empty", got)
	}
	// Colourless ui still emits the cursor moves and the plain text.
	got = promptRecolor(ui{color: false}, []string{"hi"}, 80)
	if got != "\x1b[1F\x1b[2Khi\n" {
		t.Errorf("promptRecolor plain = %q", got)
	}
}

// TestReplFileCommand: /file loads a file into a <file> block that is
// prepended to the next submitted message, and only that message.
func TestReplFileCommand(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, rec := fakeDeepSeekServer(t)
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	withStdin(t, "/file notes.md\nsummarize it\nsecond\n/quit\n", func() {
		captureStdout(t, func() {
			captureStderr(t, func() {
				_ = cmd.replLoop(context.Background(), client, "sess-1", nil, false)
			})
		})
	})

	first, _ := completionBody(t, rec, 0)
	if !strings.Contains(first, `path="notes.md"`) || !strings.Contains(first, "alpha\nbeta") {
		t.Errorf("first prompt missing the file block: %q", first)
	}
	if !strings.HasSuffix(first, "summarize it") {
		t.Errorf("file block must be prepended to the message: %q", first)
	}
	second, _ := completionBody(t, rec, 1)
	if strings.Contains(second, "alpha") {
		t.Errorf("the file block must not carry over to the next message: %q", second)
	}
}

// TestReplEmptyReplyNote: a completion that yields no text must say so rather
// than looking like the CLI did nothing. DSCLI_DEBUG_SSE is named so the raw
// stream can be captured.
func TestReplEmptyReplyNote(t *testing.T) {
	// A stream with a snapshot that carries a message id but no response text.
	empty := "event: ready\ndata: {\"request_message_id\":1,\"response_message_id\":2,\"model_type\":\"default\"}\n\n" +
		"data: {\"v\":{\"response\":{\"message_id\":2,\"status\":\"WIP\",\"fragments\":[{\"id\":2,\"type\":\"SEARCH\",\"content\":null,\"results\":[]}]}}}\n\n" +
		"data: {\"p\":\"response/status\",\"o\":\"SET\",\"v\":\"FINISHED\"}\n\n" +
		"event: close\ndata: {}\n\n"
	srv, _ := fakeDeepSeekServerWith(t, []string{empty})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{}

	var stderr string
	withStdin(t, "hi\n/quit\n", func() {
		captureStdout(t, func() {
			stderr = captureStderr(t, func() {
				_ = cmd.replLoop(context.Background(), client, "sess-1", nil, false)
			})
		})
	})
	if !strings.Contains(stderr, "no reply text") {
		t.Errorf("empty reply should be reported; stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "DSCLI_DEBUG_SSE") {
		t.Errorf("the note should point at DSCLI_DEBUG_SSE; stderr = %q", stderr)
	}
}

// TestReplSourcesSpacing: the citation block is separated from the reply by
// exactly one blank line, and followed by one blank line before the next
// prompt. It previously had two blank lines above it (the loop's post-reply
// blank plus renderSources' own) and none below.
func TestReplSourcesSpacing(t *testing.T) {
	srv, _ := fakeDeepSeekServerWith(t, []string{
		searchSSE(t, 2, "Here is the news [citation:1]", []map[string]string{
			{"url": "https://ex.com/a", "title": "A"},
		}),
	})
	c := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{}

	var out string
	withStdin(t, "hi\n/quit\n", func() {
		out = captureCombined(t, func() {
			_ = cmd.replLoop(context.Background(), c, "sess-1", nil, false)
		})
	})

	if strings.Contains(out, "\n\n\nSources:") {
		t.Errorf("double blank line before Sources:\n%q", out)
	}
	if !strings.Contains(out, "\n\nSources:\n") {
		t.Errorf("exactly one blank line before Sources: expected\n%q", out)
	}
	if !strings.Contains(out, "https://ex.com/a\n\n") {
		t.Errorf("a blank line after the sources block expected\n%q", out)
	}
}

// TestReplExitBlankBeforeConversation: leaving the REPL prints the final
// conversation id as its own block, separated by a blank line so the echoed
// /quit (or the EOF) is not glued to it. The banner already ends with a blank
// line, so the exit block adds the second one.
func TestReplExitBlankBeforeConversation(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"quit", "hi\n/quit\n"},
		{"eof", "hi\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := fakeDeepSeekServer(t)
			c := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
			cmd := &ChatCmd{}

			var stderr string
			withStdin(t, tc.input, func() {
				captureStdout(t, func() {
					stderr = captureStderr(t, func() {
						_ = cmd.replLoop(context.Background(), c, "sess-1", nil, false)
					})
				})
			})
			if !strings.HasSuffix(stderr, "\n\n\nconversation: sess-1:2\n") {
				t.Errorf("expected a blank line before the exit conversation line; stderr = %q", stderr)
			}
		})
	}
}

// TestReplEphemeralDeletionNote: an ephemeral run reports that its session
// was deleted, so "nothing persists" is visible rather than implied.
func TestReplEphemeralDeletionNote(t *testing.T) {
	srv, rec := fakeDeepSeekServer(t)
	cmd := &ChatCmd{Token: "tok", clientBase: srv.URL}

	var stderr string
	withStdin(t, "hi\n/quit\n", func() {
		captureStdout(t, func() {
			stderr = captureStderr(t, func() {
				if err := cmd.repl(context.Background()); err != nil {
					t.Errorf("repl: %v", err)
				}
			})
		})
	})

	if !strings.Contains(stderr, "ephemeral session deleted") {
		t.Errorf("expected a deletion note; stderr = %q", stderr)
	}
	rec.mu.Lock()
	deleted := append([]string(nil), rec.deleted...)
	rec.mu.Unlock()
	if len(deleted) == 0 {
		t.Error("no session was deleted")
	}
}

// TestReplUnknownCommandSpacing: slash-command feedback is its own block, so
// the next prompt is not glued to it.
func TestReplUnknownCommandSpacing(t *testing.T) {
	srv, _ := fakeDeepSeekServer(t)
	c := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{}

	var stderr string
	withStdin(t, "/clear\n/quit\n", func() {
		captureStdout(t, func() {
			stderr = captureStderr(t, func() {
				_ = cmd.replLoop(context.Background(), c, "sess-1", nil, false)
			})
		})
	})
	if !strings.Contains(stderr, "unknown command (/help for commands)\n\n") {
		t.Errorf("blank line after unknown-command feedback expected; stderr = %q", stderr)
	}
}

// TestChatSourcesBeforeConversation: the non-interactive chat path prints the
// citation block before the closing conversation line, matching the REPL (the
// conversation line is always last).
func TestChatSourcesBeforeConversation(t *testing.T) {
	srv, _ := fakeDeepSeekServerWith(t, []string{
		searchSSE(t, 2, "Gold is high [citation:1]", []map[string]string{
			{"url": "https://ex.com/gold", "title": "Gold Prices"},
		}),
	})
	cmd := &ChatCmd{Prompt: []string{"gold?"}, Search: true, Token: "tok", clientBase: srv.URL}

	out := captureCombined(t, func() {
		if err := cmd.Run(nil, context.Background()); err != nil {
			t.Fatalf("chat: %v", err)
		}
	})
	iSrc := strings.Index(out, "Sources:")
	iConv := strings.Index(out, "conversation: ")
	if iSrc < 0 || iConv < 0 {
		t.Fatalf("missing block; output:\n%q", out)
	}
	if iSrc > iConv {
		t.Errorf("Sources: must precede the conversation line; output:\n%q", out)
	}
	if !strings.HasSuffix(out, "note: ephemeral session deleted\n") {
		t.Errorf("output should still end with the deletion note; got\n%q", out)
	}
}
