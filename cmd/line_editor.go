package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// lineEditor reads one line of input with history recall and basic line
// editing (arrow keys, backspace). It replaces bufio.Scanner in interactive
// mode so arrow keys produce history navigation instead of raw escape codes.
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

func (e *lineEditor) reset() {
	e.runes = e.runes[:0]
	e.pos = 0
	e.histIdx = -1
}

// readLine reads a line in raw mode, returns the text.
// eof is true for ctrl+d on an empty line; sig is true for ctrl+c.
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
		switch {
		case b == '\r' || b == '\n':
			fmt.Fprint(out, "\r\n")
			text = string(e.runes)
			if text != "" {
				if len(e.hist) == 0 || e.hist[len(e.hist)-1] != text {
					e.hist = append(e.hist, text)
				}
			}
			return text, false, false, nil
		case b == '\x7f' || b == '\b':
			if e.pos > 0 {
				e.pos--
				e.runes = append(e.runes[:e.pos], e.runes[e.pos+1:]...)
				e.redraw(out)
			}
		case b == '\x03':
			fmt.Fprint(out, "^C\r\n")
			e.reset()
			return "", false, true, nil
		case b == '\x04':
			if len(e.runes) == 0 {
				fmt.Fprint(out, "\r\n")
				return "", true, false, nil
			}
			if e.pos < len(e.runes) {
				e.runes = append(e.runes[:e.pos], e.runes[e.pos+1:]...)
				e.redraw(out)
			}
		case b == '\x1b':
			seq := make([]byte, 2)
			if n2, _ := os.Stdin.Read(seq); n2 == 2 && seq[0] == '[' {
				switch seq[1] {
				case 'A':
					e.historyUp()
					e.redraw(out)
				case 'B':
					e.historyDown()
					e.redraw(out)
				case 'C':
					if e.pos < len(e.runes) {
						e.pos++
						fmt.Fprint(out, "\r", string(e.runes))
						fmt.Fprintf(out, "\r\x1b[%dC", e.pos)
					}
				case 'D':
					if e.pos > 0 {
						e.pos--
						fmt.Fprint(out, "\r", string(e.runes))
						fmt.Fprintf(out, "\r\x1b[%dC", e.pos)
					}
				}
			}
		default:
			if b >= 0x20 {
				e.runes = append(e.runes, 0)
				copy(e.runes[e.pos+1:], e.runes[e.pos:])
				e.runes[e.pos] = rune(b)
				e.pos++
				e.redraw(out)
			}
		}
	}
}

func (e *lineEditor) redraw(out *os.File) {
	if out == nil {
		return
	}
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
		if len(e.runes) > 0 {
			e.hist = append(e.hist, string(e.runes))
		}
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

func (r *scannerLineReader) readLine() (text string, eof bool, err error) {
	if !r.s.Scan() {
		return "", true, r.s.Err()
	}
	return strings.TrimSpace(r.s.Text()), false, nil
}