package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// lineEditor reads one line of input with history recall and line editing
// (arrow keys, Home/End, backspace, Ctrl+A/E). It replaces bufio.Scanner in
// interactive mode so arrow keys produce navigation instead of raw escape
// codes.
//
// The editor shares a history slice across calls; up/down cycle through it.
// Empty lines are not pushed to history.
type lineEditor struct {
	hist    []string
	histIdx int // -1 = fresh, 0..last = history entry

	runes []rune
	pos   int // 0..len(runes)
}

func newLineEditor(hist []string) *lineEditor {
	return &lineEditor{hist: hist, histIdx: -1}
}

// commit finalizes the current buffer: it trims the text (matching the old
// bufio.Scanner path, so a whitespace-only line is a no-op rather than a chat
// message) and records non-blank lines in history. blank reports an empty
// line, which the caller must not echo.
func (e *lineEditor) commit() (text string, blank bool) {
	text = strings.TrimSpace(string(e.runes))
	if text == "" {
		e.reset()
		return "", true
	}
	if len(e.hist) == 0 || e.hist[len(e.hist)-1] != text {
		e.hist = append(e.hist, text)
	}
	return text, false
}

func (e *lineEditor) reset() {
	e.runes = e.runes[:0]
	e.pos = 0
	e.histIdx = -1
}

// readLine reads a line in raw mode.
//   - normal line: (text, false, false, nil)
//   - ctrl+c on an empty buffer (the exit request): ("", false, true, nil)
//   - ctrl+d on an empty buffer (EOF): ("", true, false, nil)
//   - ctrl+c with text: clears the buffer and returns an empty line
func (e *lineEditor) readLine() (text string, eof bool, sig bool, err error) {
	e.reset()
	old, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return "", false, false, fmt.Errorf("raw terminal: %w", err)
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), old) }()
	out := os.Stdout

	buf := make([]byte, 1)
	for {
		n, rerr := os.Stdin.Read(buf)
		if rerr != nil || n == 0 {
			return "", false, false, rerr
		}
		b := buf[0]
		switch b {
		case '\r', '\n':
			text, blank := e.commit()
			if blank {
				// The REPL ignores an empty line. Do not echo the newline,
				// or every stray Enter would pile up a blank line on screen.
				return "", false, false, nil
			}
			fmt.Fprint(out, "\r\n")
			return text, false, false, nil
		case '\x7f', '\b': // backspace
			if e.pos > 0 {
				e.pos--
				e.runes = append(e.runes[:e.pos], e.runes[e.pos+1:]...)
				e.redraw(out)
			}
		case '\x01': // ctrl+a: cursor to start of line
			e.home(out)
		case '\x05': // ctrl+e: cursor to end of line
			e.end(out)
		case '\x0b': // ctrl+k: delete to end of line
			e.runes = e.runes[:e.pos]
			e.redraw(out)
		case '\x15': // ctrl+u: delete the whole line
			e.reset()
			e.redraw(out)
		case '\x03': // ctrl+c
			if len(e.runes) == 0 {
				// Exit request: the REPL counts two consecutive presses.
				fmt.Fprint(out, "^C\r\n")
				return "", false, true, nil
			}
			fmt.Fprint(out, "^C\r\n")
			e.reset()
			e.redraw(out)
		case '\x04': // ctrl+d
			if len(e.runes) == 0 {
				fmt.Fprint(out, "\r\n")
				return "", true, false, nil
			}
			e.deleteAt(out)
		case '\x1b': // escape sequence: arrows, Home/End, Delete
			seq := make([]byte, 3)
			n2, _ := os.Stdin.Read(seq)
			e.handleEscape(seq[:n2], out)
		default:
			if b >= 0x20 {
				e.insert(rune(b), out)
			}
		}
	}
}

// handleEscape parses a CSI/SS3 sequence (the bytes after ESC).
func (e *lineEditor) handleEscape(seq []byte, out *os.File) {
	if len(seq) < 2 {
		return
	}
	switch seq[0] {
	case '[':
		switch seq[1] {
		case 'A':
			e.historyUp()
			e.redraw(out)
		case 'B':
			e.historyDown()
			e.redraw(out)
		case 'C':
			e.right(out)
		case 'D':
			e.left(out)
		case 'H':
			e.home(out)
		case 'F':
			e.end(out)
		case '3':
			if len(seq) >= 3 && seq[2] == '~' {
				e.deleteAt(out) // Delete key
			}
		case '1':
			if len(seq) >= 3 && seq[2] == '~' {
				e.home(out) // Home (linux console)
			}
		case '4':
			if len(seq) >= 3 && seq[2] == '~' {
				e.end(out) // End (linux console)
			}
		}
	case 'O':
		switch seq[1] {
		case 'A':
			e.historyUp()
			e.redraw(out)
		case 'B':
			e.historyDown()
			e.redraw(out)
		case 'C':
			e.right(out)
		case 'D':
			e.left(out)
		case 'H':
			e.home(out)
		case 'F':
			e.end(out)
		}
	}
}

func (e *lineEditor) insert(r rune, out *os.File) {
	e.runes = append(e.runes, 0)
	copy(e.runes[e.pos+1:], e.runes[e.pos:])
	e.runes[e.pos] = r
	e.pos++
	e.redraw(out)
}

func (e *lineEditor) deleteAt(out *os.File) {
	if e.pos < len(e.runes) {
		e.runes = append(e.runes[:e.pos], e.runes[e.pos+1:]...)
		e.redraw(out)
	}
}

func (e *lineEditor) left(out *os.File) {
	if e.pos > 0 {
		e.pos--
		e.redraw(out)
	}
}

func (e *lineEditor) right(out *os.File) {
	if e.pos < len(e.runes) {
		e.pos++
		e.redraw(out)
	}
}

func (e *lineEditor) home(out *os.File) {
	e.pos = 0
	e.redraw(out)
}

func (e *lineEditor) end(out *os.File) {
	e.pos = len(e.runes)
	e.redraw(out)
}

// redraw repaints the current buffer on a fresh line and repositions the
// hardware cursor.
func (e *lineEditor) redraw(out *os.File) {
	fmt.Fprint(out, "\r\x1b[2K", string(e.runes))
	if e.pos > 0 {
		fmt.Fprintf(out, "\r\x1b[%dC", e.pos)
	}
}

func (e *lineEditor) historyUp() {
	if len(e.hist) == 0 {
		return
	}
	if e.histIdx == -1 {
		e.histIdx = len(e.hist) - 1
	} else if e.histIdx > 0 {
		e.histIdx--
	}
	e.runes = []rune(e.hist[e.histIdx])
	e.pos = len(e.runes)
}

func (e *lineEditor) historyDown() {
	if len(e.hist) == 0 {
		return
	}
	if e.histIdx < 0 {
		return
	}
	e.histIdx++
	if e.histIdx >= len(e.hist) {
		e.histIdx = -1
		e.reset()
		return
	}
	e.runes = []rune(e.hist[e.histIdx])
	e.pos = len(e.runes)
}

// scannerLineReader is the fallback for non-interactive (piped) mode.
type scannerLineReader struct {
	s *bufio.Scanner
}

func newScannerLineReader() *scannerLineReader {
	s := bufio.NewScanner(os.Stdin)
	s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &scannerLineReader{s: s}
}

func (r *scannerLineReader) readLine() (text string, eof bool, sig bool, err error) {
	if !r.s.Scan() {
		return "", true, false, r.s.Err()
	}
	return strings.TrimSpace(r.s.Text()), false, false, nil
}
