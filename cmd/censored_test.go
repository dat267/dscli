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
// in the dimmed-grey note style (visibly not the assistant's reply, which is
// plain) with a "note:" prefix, instead of sharing the reply's text style.
func TestTUIFilteredNoteStyled(t *testing.T) {
	m, _ := tuiHarness(t, []string{filteredSSE(t, "")}, "")
	m.input.SetValue("hi")
	m.Update(press(tea.KeyEnter))
	pumpTUI(m)
	if !strings.Contains(m.scroll, ansiDim) || !strings.Contains(m.scroll, ansiMuted) {
		t.Errorf("filtered note not in dimmed-grey note style:\n%q", m.scroll)
	}
	if !strings.Contains(m.scroll, "note: reply was filtered by DeepSeek") {
		t.Errorf("missing note text:\n%q", m.scroll)
	}
}

// filteredSSE builds a stream that emits partial text and then ends with a
// CONTENT_FILTER status: the reply is rejected mid-way, keeping the partial.
func filteredSSE(t *testing.T, partial string) string {
	t.Helper()
	return completionSSE(t, 2, partial) +
		"data: {\"v\":[{\"p\":\"status\",\"v\":\"CONTENT_FILTER\"},{\"p\":\"quasi_status\",\"v\":\"CONTENT_FILTER\"}]}\n\n"
}

// TestResumePrompt: the /resume continuation embeds the filtered partial as
// context and an instruction to continue it, plus any user hint.
func TestResumePrompt(t *testing.T) {
	p := resumePrompt("para one\npara two", "keep it short")
	for _, want := range []string{"para one\npara two", "Continue the answer", "keep it short"} {
		if !strings.Contains(p, want) {
			t.Errorf("resume prompt missing %q:\n%s", want, p)
		}
	}
	if !strings.Contains(p, "do not restate") {
		t.Errorf("resume prompt missing the no-restate guard:\n%s", p)
	}
}

// TestReplFilteredResume: after a filtered turn the line REPL keeps the
// partial; /resume sends it back as context and the continuation is answered.
func TestReplFilteredResume(t *testing.T) {
	srv, rec := fakeDeepSeekServerWith(t, []string{
		filteredSSE(t, "partial text"),
		completionSSE(t, 3, "continuation"),
	})
	defer srv.Close()
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{}
	var stdout, stderr string
	withStdin(t, "hi\n/resume\n/quit\n", func() {
		stdout = captureStdout(t, func() {
			stderr = captureStderr(t, func() {
				_ = cmd.replLoop(context.Background(), client, "sess-1", nil, false)
			})
		})
	})
	if !strings.Contains(stdout, "continuation") {
		t.Errorf("stdout = %q, want the resumed reply", stdout)
	}
	if !strings.Contains(stderr, "hint: /resume") {
		t.Errorf("stderr missing resume hint:\n%s", stderr)
	}
	if !strings.Contains(stderr, "resuming from the filtered partial") {
		t.Errorf("stderr missing resume note:\n%s", stderr)
	}
	prompt, _ := completionBody(t, rec, 1)
	if !strings.Contains(prompt, "partial text") || !strings.Contains(prompt, "Continue the answer") {
		t.Errorf("resume prompt = %q, want the partial + continue instruction", prompt)
	}
	if got := len(rec.completionBodies); got != 2 {
		t.Errorf("completions = %d, want 2", got)
	}
}

// TestReplResumeNothingFiltered: /resume without a filtered partial is a
// no-op note; no extra completion is sent.
func TestReplResumeNothingFiltered(t *testing.T) {
	srv, rec := fakeDeepSeekServerWith(t, []string{completionSSE(t, 2, "fine")})
	defer srv.Close()
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{}
	var stderr string
	withStdin(t, "hi\n/resume\n/quit\n", func() {
		captureStdout(t, func() {
			stderr = captureStderr(t, func() {
				_ = cmd.replLoop(context.Background(), client, "sess-1", nil, false)
			})
		})
	})
	if !strings.Contains(stderr, "nothing to resume") {
		t.Errorf("stderr = %q, want nothing-to-resume note", stderr)
	}
	if got := len(rec.completionBodies); got != 1 {
		t.Errorf("completions = %d, want 1 (no resume turn)", got)
	}
}

// TestTUIFilteredResume: the TUI offers /resume after a filtered turn and
// sends the partial back as context for the continuation.
func TestTUIFilteredResume(t *testing.T) {
	m, rec := tuiHarness(t, []string{
		filteredSSE(t, "partial text"),
		completionSSE(t, 3, "continuation"),
	}, "")
	m.input.SetValue("hi")
	m.Update(press(tea.KeyEnter))
	pumpTUI(m)
	if !strings.Contains(m.scroll, "hint: /resume") {
		t.Errorf("missing resume hint:\n%q", m.scroll)
	}
	if m.lastPartial != "partial text" {
		t.Errorf("lastPartial = %q, want %q", m.lastPartial, "partial text")
	}
	m.input.SetValue("/resume")
	m.Update(press(tea.KeyEnter))
	if !m.busy {
		t.Fatal("/resume should start a turn")
	}
	pumpTUI(m)
	if !strings.Contains(m.scroll, "continuation") {
		t.Errorf("missing resumed reply:\n%q", m.scroll)
	}
	prompt, _ := completionBody(t, rec, 1)
	if !strings.Contains(prompt, "partial text") || !strings.Contains(prompt, "Continue the answer") {
		t.Errorf("resume prompt = %q, want the partial + continue instruction", prompt)
	}
}
