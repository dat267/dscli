package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/dat267/dscli/internal/deepseek"
)

// TestTranscriptsEnabled: transcripts are saved when the config (data)
// directory is known and the run is neither ephemeral nor opted out.
func TestTranscriptsEnabled(t *testing.T) {
	cases := []struct {
		cfgPath      string
		noPersist    bool
		noTranscript bool
		want         bool
	}{
		{"/x/cfg.json", false, false, true},
		{"", false, false, false},           // no data dir
		{"/x/cfg.json", true, false, false}, // ephemeral run leaves nothing
		{"/x/cfg.json", false, true, false}, // explicit opt-out
	}
	for _, tc := range cases {
		if got := transcriptsEnabled(tc.cfgPath, tc.noPersist, tc.noTranscript); got != tc.want {
			t.Errorf("transcriptsEnabled(%q, %v, %v) = %v, want %v",
				tc.cfgPath, tc.noPersist, tc.noTranscript, got, tc.want)
		}
	}
}

// TestTranscriptPath: the file lives in the transcripts folder next to the
// config file, named after the bare session id.
func TestTranscriptPath(t *testing.T) {
	if p := transcriptPath("/home/u/.config/dscli/dscli.json", "sess-42:17"); p != "/home/u/.config/dscli/transcripts/sess-42.jsonl" {
		t.Errorf("path = %q", p)
	}
	if p := transcriptPath("/home/u/.config/dscli/dscli.json", ""); p != "" {
		t.Errorf("empty session should give no path, got %q", p)
	}
	if p := transcriptPath("", "sess-42"); p != "" {
		t.Errorf("no config should give no path, got %q", p)
	}
}

// TestAppendTranscriptRoundTrip: messages are appended as JSON lines, one per
// message, and read back in order with roles and texts intact; the file is
// created private (0600) in a private folder.
func TestAppendTranscriptRoundTrip(t *testing.T) {
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "dscli.json")
	appendTranscript(cfgPath, "sess-1", "user", "hello")
	appendTranscript(cfgPath, "sess-1:5", "assistant", "hi there") // position string still maps to sess-1
	entries, err := loadTranscript(cfgPath, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Role != "user" || entries[0].Text != "hello" {
		t.Errorf("entry 0 = %+v", entries[0])
	}
	if entries[1].Role != "assistant" || entries[1].Text != "hi there" {
		t.Errorf("entry 1 = %+v", entries[1])
	}
	p := transcriptPath(cfgPath, "sess-1")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("transcript perms = %v, want 0600", perm)
	}
	dinfo, err := os.Stat(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dinfo.Mode().Perm(); perm != 0700 {
		t.Errorf("transcripts dir perms = %v, want 0700", perm)
	}
	data, _ := os.ReadFile(p)
	if lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; lines != 2 {
		t.Errorf("file has %d lines, want 2", lines)
	}
}

// TestChatAskTranscript: a one-shot `dscli chat "..."` saves the user prompt
// and the streamed reply to the session transcript next to the config file.
func TestChatAskTranscript(t *testing.T) {
	srv, _ := fakeDeepSeekServerWith(t, []string{completionSSE(t, 2, "Hello back")})
	defer srv.Close()
	cfgPath := filepath.Join(t.TempDir(), "dscli.json")
	cmd := &ChatCmd{Prompt: []string{"hi"}, Token: "tok", cfgPath: cfgPath, clientBase: srv.URL}
	captureStdout(t, func() {
		if err := cmd.Run(nil, context.Background()); err != nil {
			t.Fatalf("chat: %v", err)
		}
	})
	entries, err := loadTranscript(cfgPath, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Role != "user" || entries[0].Text != "hi" ||
		entries[1].Role != "assistant" || entries[1].Text != "Hello back" {
		t.Errorf("transcript = %+v, want [user:hi, assistant:Hello back]", entries)
	}
}

// TestChatAskTranscriptDisabled: with --no-persist (ephemeral) no transcript
// is written; with --no-transcript the folder never appears.
func TestChatAskTranscriptDisabled(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		noPersist, noTranscript bool
	}{
		{"no-persist", true, false},
		{"no-transcript", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := fakeDeepSeekServerWith(t, []string{completionSSE(t, 2, "ok")})
			defer srv.Close()
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "dscli.json")
			cmd := &ChatCmd{Prompt: []string{"hi"}, Token: "tok", cfgPath: cfgPath,
				NoPersist: tc.noPersist, NoTranscript: tc.noTranscript, clientBase: srv.URL}
			captureStdout(t, func() {
				if err := cmd.Run(nil, context.Background()); err != nil {
					t.Fatalf("chat: %v", err)
				}
			})
			if _, err := os.Stat(filepath.Join(dir, "transcripts")); !os.IsNotExist(err) {
				t.Fatalf("transcripts folder should not exist, err = %v", err)
			}
		})
	}
}

// TestReplTranscript: the line-based REPL saves each turn's prompt and reply.
func TestReplTranscript(t *testing.T) {
	srv, _ := fakeDeepSeekServerWith(t, []string{
		completionSSE(t, 2, "one"),
		completionSSE(t, 3, "two"),
	})
	defer srv.Close()
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cfgPath := filepath.Join(t.TempDir(), "dscli.json")
	cmd := &ChatCmd{cfgPath: cfgPath}

	withStdin(t, "first\nsecond\n/quit\n", func() {
		captureStdout(t, func() {
			captureStderr(t, func() {
				if err := cmd.replLoop(context.Background(), client, "sess-1", nil, false); err != nil {
					t.Fatalf("replLoop: %v", err)
				}
			})
		})
	})
	entries, err := loadTranscript(cfgPath, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	var roles, texts []string
	for _, e := range entries {
		roles = append(roles, e.Role)
		texts = append(texts, e.Text)
	}
	if got := strings.Join(roles, ","); got != "user,assistant,user,assistant" {
		t.Errorf("roles = %q, want user,assistant,user,assistant", got)
	}
	if got := strings.Join(texts, "|"); got != "first|one|second|two" {
		t.Errorf("texts = %q, want first|one|second|two", got)
	}
}

// TestTUITranscript: the TUI saves the submitted prompt before the turn and
// the streamed reply when it finishes.
func TestTUITranscript(t *testing.T) {
	m, _ := tuiHarness(t, []string{completionSSE(t, 2, "Hello TUI")}, "")
	m.input.SetValue("hi")
	m.Update(press(tea.KeyEnter))
	pumpTUI(m)
	entries, err := loadTranscript(m.cfgPath, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Role != "user" || entries[0].Text != "hi" ||
		entries[1].Role != "assistant" || entries[1].Text != "Hello TUI" {
		t.Errorf("transcript = %+v, want [user:hi, assistant:Hello TUI]", entries)
	}
}

// TestSessionTranscriptCommand: `dscli session transcript` prints the saved
// texts with their roles; a session with no saved texts says so.
func TestSessionTranscriptCommand(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "dscli.json")
	appendTranscript(cfgPath, "sess-9", "user", "why?")
	appendTranscript(cfgPath, "sess-9", "assistant", "because")
	app := &App{cfgPath: cfgPath}

	out := captureStdout(t, func() {
		cmd := &SessionTranscriptCmd{Session: "sess-9"}
		if err := cmd.Run(app); err != nil {
			t.Fatalf("session transcript: %v", err)
		}
	})
	for _, want := range []string{"sess-9", "user", "assistant", "why?", "because"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	empty := captureStdout(t, func() {
		cmd := &SessionTranscriptCmd{Session: "sess-none"}
		if err := cmd.Run(app); err != nil {
			t.Fatalf("session transcript: %v", err)
		}
	})
	if !strings.Contains(empty, "no saved texts for session sess-none") {
		t.Errorf("empty output = %q", empty)
	}
}
