package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

// TestDoRunsTaskWithTools drives `do` end to end: the model reads a file via a
// tool call, then answers in prose. The ephemeral session is deleted and
// DeepThink is on by default.
func TestDoRunsTaskWithTools(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("the answer is 42"), 0644); err != nil {
		t.Fatal(err)
	}
	readCall := `{"tool":"read_file","path":"a.txt"}`
	srv, rec := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, readCall),
		completionSSE(t, 3, "Answer: 42."),
	})
	cmd := &DoCmd{
		NoPersist:  true,
		Task:       []string{"what is in a.txt?"},
		Token:      "tok",
		Thinking:   true, // kong default for `do` (verified by TestDoThinkingDefaultsOn)
		Workdir:    dir,
		clientBase: srv.URL,
	}

	var stdout, stderr string
	var runErr error
	stdout = captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			runErr = cmd.Run(&App{}, context.Background())
		})
	})
	if runErr != nil {
		t.Fatalf("do.Run: %v", runErr)
	}
	if stdout != "Answer: 42.\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "read_file a.txt") {
		t.Errorf("missing tool note: %q", stderr)
	}

	rec.mu.Lock()
	body0 := ""
	if len(rec.completionBodies) > 0 {
		body0 = rec.completionBodies[0]
	}
	deleted := append([]string(nil), rec.deleted...)
	rec.mu.Unlock()
	var env map[string]any
	if err := json.Unmarshal([]byte(body0), &env); err != nil {
		t.Fatalf("completion body 0: %v", err)
	}
	if env["thinking_enabled"] != true {
		t.Errorf("thinking_enabled = %v, want true by default", env["thinking_enabled"])
	}
	if env["search_enabled"] != false {
		t.Errorf("search_enabled = %v, want false", env["search_enabled"])
	}
	if len(deleted) != 1 || deleted[0] != "sess-1" {
		t.Errorf("ephemeral session not deleted: %v", deleted)
	}
}

// TestDoThinkingDefaultsOn verifies the kong default wiring: `dscli do` runs
// with DeepThink on unless the user turns it off.
func TestDoThinkingDefaultsOn(t *testing.T) {
	cmd := &DoCmd{
		NoPersist: true}
	k, err := kong.New(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Parse([]string{"refactor the parser"}); err != nil {
		t.Fatal(err)
	}
	if !cmd.Thinking {
		t.Error("thinking must default to true")
	}

	cmd2 := &DoCmd{
		NoPersist: true}
	k2, err := kong.New(cmd2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k2.Parse([]string{"--thinking=false", "quick"}); err != nil {
		t.Fatal(err)
	}
	if cmd2.Thinking {
		t.Error("--thinking=false must disable DeepThink")
	}
}

// TestDoReadsStdin: an empty task list falls back to stdin.
func TestDoReadsStdin(t *testing.T) {
	srv, _ := fakeDeepSeekServer(t)
	cmd := &DoCmd{
		NoPersist:  true,
		Token:      "tok",
		clientBase: srv.URL,
	}
	var stdout string
	var runErr error
	withStdin(t, "summarize this\n", func() {
		stdout = captureStdout(t, func() {
			runErr = cmd.Run(&App{}, context.Background())
		})
	})
	if runErr != nil {
		t.Fatalf("do.Run: %v", runErr)
	}
	if stdout != "Hello world!\n" {
		t.Errorf("stdout = %q", stdout)
	}
}

// TestDoJSONOut: the final answer arrives as NDJSON, then a done line.
func TestDoJSONOut(t *testing.T) {
	srv, _ := fakeDeepSeekServer(t)
	cmd := &DoCmd{
		NoPersist:  true,
		Task:       []string{"hello"},
		Token:      "tok",
		JSONOut:    true,
		clientBase: srv.URL,
	}
	var stdout string
	var runErr error
	stdout = captureStdout(t, func() {
		runErr = cmd.Run(&App{}, context.Background())
	})
	if runErr != nil {
		t.Fatalf("do.Run: %v", runErr)
	}
	want := `{"delta":"Hello world!\n"}` + "\n" + `{"done":true}` + "\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestDoYesAutoConfirmsWrites: with -y, a write tool call is applied without
// any confirmation prompt (the ephemeral session is deleted afterwards).
func TestDoYesAutoConfirmsWrites(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("aaa\nbbb\naaa\n"), 0644); err != nil {
		t.Fatal(err)
	}
	editCall := `{"tool":"edit_file","path":"a.txt","old":"aaa","new":"AXA"}`
	srv, _ := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, editCall),
		completionSSE(t, 3, "Edited."),
	})
	cmd := &DoCmd{
		NoPersist:  true,
		Task:       []string{"edit the file"},
		Token:      "tok",
		Thinking:   true,
		Yes:        true,
		Workdir:    dir,
		clientBase: srv.URL,
	}
	var runErr error
	captureStdout(t, func() {
		runErr = cmd.Run(&App{}, context.Background())
	})
	if runErr != nil {
		t.Fatalf("do.Run: %v", runErr)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "AXA\nbbb\naaa\n" {
		t.Errorf("file after -y edit = %q", got)
	}
}

// TestDoWithoutYesPrompts: without -y the write goes through the standard
// confirmation (stubbed to deny here), so the file stays untouched.
func TestDoWithoutYesPrompts(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("aaa\nbbb\naaa\n"), 0644); err != nil {
		t.Fatal(err)
	}
	editCall := `{"tool":"edit_file","path":"a.txt","old":"aaa","new":"AXA"}`
	srv, _ := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, editCall),
		completionSSE(t, 3, "Understood."),
	})
	cmd := &DoCmd{
		NoPersist:  true,
		Task:       []string{"edit the file"},
		Token:      "tok",
		Thinking:   true,
		Workdir:    dir,
		clientBase: srv.URL,
	}

	orig := confirmWrite
	t.Cleanup(func() { confirmWrite = orig })
	confirmWrite = func(string) bool { return false }

	var runErr error
	captureStdout(t, func() {
		runErr = cmd.Run(&App{}, context.Background())
	})
	if runErr != nil {
		t.Fatalf("do.Run: %v", runErr)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "aaa\nbbb\naaa\n" {
		t.Errorf("file changed despite denied confirmation: %q", got)
	}
}

// TestDoRejectsNoInput: no task and empty stdin is an error, not a session.
func TestDoRejectsNoInput(t *testing.T) {
	cmd := &DoCmd{
		NoPersist: true, Token: "tok"}
	withStdin(t, "  \n", func() {
		if err := cmd.Run(&App{}, context.Background()); err == nil {
			t.Error("expected an error for empty input")
		}
	})
}
