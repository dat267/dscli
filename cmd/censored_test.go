package cmd

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/dat267/dscli/internal/deepseek"
)

// TestChatFilteredShowsNote: a content-filtered reply prints a short note and
// is NOT retried (no style, no re-send).
func TestChatFilteredShowsNote(t *testing.T) {
	filtered := completionSSE(t, 2, "") + "data: {\"v\":[{\"p\":\"status\",\"v\":\"CONTENT_FILTER\"},{\"p\":\"quasi_status\",\"v\":\"CONTENT_FILTER\"}]}\n\n"
	srv, rec := fakeDeepSeekServerWith(t, []string{filtered})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{}
	var stderr string
	withStdin(t, "hi\n/exit\n", func() {
		captureStdout(t, func() {
			stderr = captureStderr(t, func() {
				_ = cmd.replLoop(context.Background(), client, "sess-1", nil, false)
			})
		})
	})
	if !strings.Contains(stderr, "filtered by DeepSeek") {
		t.Errorf("stderr = %q, want a filtered note", stderr)
	}
	// Exactly one completion: no retry.
	if got := len(rec.completionBodies); got != 1 {
		t.Errorf("completions = %d, want 1 (no retry)", got)
	}
	prompt, _ := completionBody(t, rec, 0)
	if prompt != "hi" {
		t.Errorf("prompt = %q, want %q (sent verbatim)", prompt, "hi")
	}
}

// TestAskFilteredShowsNote: `ask` prints the same note without retrying.
func TestAskFilteredShowsNote(t *testing.T) {
	filtered := completionSSE(t, 2, "") + "data: {\"v\":[{\"p\":\"status\",\"v\":\"CONTENT_FILTER\"},{\"p\":\"quasi_status\",\"v\":\"CONTENT_FILTER\"}]}\n\n"
	srv, rec := fakeDeepSeekServerWith(t, []string{filtered})
	defer srv.Close()
	cmd := &AskCmd{Prompt: []string{"hello"}, Token: "tok", clientBase: srv.URL}
	var stderr string
	captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if err := cmd.Run(&App{cfgPath: filepath.Join(t.TempDir(), "dscli.json")}, context.Background()); err != nil {
				t.Fatalf("ask: %v", err)
			}
		})
	})
	if !strings.Contains(stderr, "filtered by DeepSeek") {
		t.Errorf("stderr = %q, want a filtered note", stderr)
	}
	if got := len(rec.completionBodies); got != 1 {
		t.Errorf("completions = %d, want 1 (no retry)", got)
	}
}

// TestChatFilteredNoNoteWhenAccepted: a normal reply produces no note.
func TestChatFilteredNoNoteWhenAccepted(t *testing.T) {
	srv, _ := fakeDeepSeekServerWith(t, []string{completionSSE(t, 2, "fine")})
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{}
	var stderr string
	withStdin(t, "hi\n/exit\n", func() {
		captureStdout(t, func() {
			stderr = captureStderr(t, func() {
				_ = cmd.replLoop(context.Background(), client, "sess-1", nil, false)
			})
		})
	})
	if strings.Contains(stderr, "filtered") {
		t.Errorf("stderr = %q, want no filtered note for an accepted reply", stderr)
	}
}

// TestTUIFilteredNoteStyled: inside the TUI the filtered reply note renders
// muted grey (visibly not the assistant's reply, which is plain) with a
// "note:" prefix, instead of sharing the reply's text style.
func TestTUIFilteredNoteStyled(t *testing.T) {
	filtered := completionSSE(t, 2, "") + "data: {\"v\":[{\"p\":\"status\",\"v\":\"CONTENT_FILTER\"},{\"p\":\"quasi_status\",\"v\":\"CONTENT_FILTER\"}]}\n\n"
	m, _ := tuiHarness(t, []string{filtered}, "")
	m.input.SetValue("hi")
	m.Update(press(tea.KeyEnter))
	pumpTUI(m)
	if !strings.Contains(m.scroll, ansiMuted) {
		t.Errorf("filtered note not muted grey:\n%q", m.scroll)
	}
	if !strings.Contains(m.scroll, "note: reply was filtered by DeepSeek") {
		t.Errorf("missing note text:\n%q", m.scroll)
	}
}
