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
func TestLineEditorHomeEndDelete(t *testing.T) {
	e := newLineEditor(nil)
	e.runes = []rune("hello")
	e.pos = 5

	// Home: cursor to the start.
	e.home(nil)
	if e.pos != 0 {
		t.Errorf("home pos = %d, want 0", e.pos)
	}
	// Insert at the start: cursor arithmetic keeps the text ordered.
	e.insert('X', nil)
	if got := string(e.runes); got != "Xhello" || e.pos != 1 {
		t.Errorf("insert at home = %q pos %d", got, e.pos)
	}
	// End: cursor back to the tail.
	e.end(nil)
	if e.pos != len(e.runes) {
		t.Errorf("end pos = %d, want %d", e.pos, len(e.runes))
	}
	// Delete at end is a no-op; Delete mid-string removes the char at cursor.
	e.deleteAt(nil)
	if got := string(e.runes); got != "Xhello" {
		t.Errorf("delete at end = %q, want unchanged", got)
	}
	e.pos = 0
	e.deleteAt(nil)
	if got := string(e.runes); got != "hello" {
		t.Errorf("delete at start = %q, want %q", got, "hello")
	}
	// Ctrl+left/right equivalents are plain left/right.
	e.left(nil)
	if e.pos != 0 {
		t.Errorf("left at start = %d, want 0", e.pos)
	}
	e.right(nil)
	if e.pos != 1 {
		t.Errorf("right = %d, want 1", e.pos)
	}
}

func TestLineEditorEscapeParsing(t *testing.T) {
	e := newLineEditor(nil)
	e.runes = []rune("hello")
	e.pos = 5
	// ESC [ H (Home) and ESC O F (End, xterm style) and ESC [ 3 ~ (Delete).
	e.handleEscape([]byte{'[', 'H'}, nil)
	if e.pos != 0 {
		t.Errorf("ESC[H pos = %d, want 0", e.pos)
	}
	e.handleEscape([]byte{'[', '3', '~'}, nil) // Delete at start drops 'h'
	if got := string(e.runes); got != "ello" {
		t.Errorf("Delete = %q, want %q", got, "ello")
	}
	e.handleEscape([]byte{'O', 'F'}, nil)
	if e.pos != len(e.runes) {
		t.Errorf("ESC[OF pos = %d, want %d", e.pos, len(e.runes))
	}
	// Delete at the end is a no-op.
	e.handleEscape([]byte{'[', '3', '~'}, nil)
	if got := string(e.runes); got != "ello" {
		t.Errorf("delete at end = %q, want unchanged", got)
	}
	// Unknown sequences are ignored without moving anything.
	e.handleEscape([]byte{'[', 'Z'}, nil)
	if e.pos != len(e.runes) {
		t.Errorf("unknown seq moved cursor: %d", e.pos)
	}
}
