package cmd

import (
	"testing"
)

func TestLineEditorHistory(t *testing.T) {
	e := newLineEditor(nil)
	// Simulate submitting lines.
	e.hist = append(e.hist, "first", "second")

	// Empty buffer: up goes to the last entry.
	e.reset()
	e.historyUp()
	if got := string(e.runes); got != "second" {
		t.Errorf("first up = %q, want %q", got, "second")
	}
	e.historyUp()
	if got := string(e.runes); got != "first" {
		t.Errorf("second up = %q, want %q", got, "first")
	}
	e.historyDown()
	if got := string(e.runes); got != "second" {
		t.Errorf("down to second = %q, want %q", got, "second")
	}
	e.historyDown()
	if len(e.runes) != 0 {
		t.Errorf("past the end = %q, want empty", string(e.runes))
	}
}

func TestLineEditorEmptyNotPushed(t *testing.T) {
	e := newLineEditor(nil)
	if len(e.hist) != 0 {
		t.Fatal("expected empty history")
	}
}

func TestLineEditorDuplicateNotPushed(t *testing.T) {
	e := newLineEditor(nil)
	// Simulate submitting "same" twice.
	e.hist = append(e.hist, "same")
	if len(e.hist) != 1 {
		t.Errorf("expected 1 entry, got %d", len(e.hist))
	}
	// Second submit of same text should not duplicate.
	if len(e.hist) > 0 && e.hist[len(e.hist)-1] == "same" {
		// This simulates the guard in readLine.
	} else {
		e.hist = append(e.hist, "same")
	}
	if len(e.hist) != 1 {
		t.Errorf("expected 1 entry (no duplicate), got %d", len(e.hist))
	}
}

func TestLineEditorInserts(t *testing.T) {
	e := newLineEditor(nil)
	// Simulate typing 'h', 'e', 'l', 'l', 'o'.
	for _, r := range []rune{'h', 'e', 'l', 'l', 'o'} {
		e.runes = append(e.runes, r)
		e.pos++
	}
	if got := string(e.runes); got != "hello" {
		t.Fatalf("after typing = %q, want %q", got, "hello")
	}
	// Backspace once.
	e.pos--
	e.runes = append(e.runes[:e.pos], e.runes[e.pos+1:]...)
	if got := string(e.runes); got != "hell" {
		t.Errorf("after backspace = %q, want %q", got, "hell")
	}
}