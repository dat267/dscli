package cmd

import (
	"strings"
	"testing"
)

func mdColor() ui { return ui{color: true} }

// assertRowsWithinWidth checks the renderMarkdown invariant: every row's
// visible width must fit the given width.
func assertRowsWithinWidth(t *testing.T, rows []string, width int) {
	t.Helper()
	for i, r := range rows {
		if w := textWidth(stripANSI(r)); w > width {
			t.Errorf("row %d overflows %d columns (%d): %q", i, width, w, stripANSI(r))
		}
	}
}

func TestMarkdownHeadings(t *testing.T) {
	rows := renderMarkdown(mdColor(), "# Title\n\n## Sub\n\n### Deep\n\nplain", 40)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, ansiBold+ansiAccent+"Title"+ansiReset) {
		t.Errorf("h1 missing accent+bold styling:\n%q", joined)
	}
	if !strings.Contains(joined, ansiBold+ansiAccent+"Sub"+ansiReset) {
		t.Errorf("h2 missing accent+bold styling:\n%q", joined)
	}
	if !strings.Contains(joined, ansiBold+"Deep"+ansiReset) {
		t.Errorf("h3 missing bold styling:\n%q", joined)
	}
	assertRowsWithinWidth(t, rows, 40)
}

func TestMarkdownCodeBox(t *testing.T) {
	src := "Example:\n\n```go\nfmt.Println(\"hi\")\nx := 1\n```\n"
	rows := renderMarkdown(mdColor(), src, 40)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "─ go ") {
		t.Errorf("code box top border missing language label:\n%q", joined)
	}
	if !strings.Contains(joined, "fmt.Println(\"hi\")") {
		t.Errorf("code box content missing:\n%q", joined)
	}
	// Two content lines each on its own bordered row.
	if got := strings.Count(joined, "│"); got < 4 {
		t.Errorf("want at least 4 border bars (2 rows x 2 sides), got %d:\n%q", got, joined)
	}
	assertRowsWithinWidth(t, rows, 40)
}

func TestMarkdownUnclosedFenceStillBoxes(t *testing.T) {
	// Mid-stream input: the fence never closes, but the box renders complete.
	rows := renderMarkdown(mdColor(), "```go\nfmt.Println", 40)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "fmt.Println") {
		t.Errorf("partial code content missing:\n%q", joined)
	}
	if got := strings.Count(joined, "─"); got < 2 {
		t.Errorf("want top and bottom borders even for an unclosed fence:\n%q", joined)
	}
	assertRowsWithinWidth(t, rows, 40)
}

func TestMarkdownInline(t *testing.T) {
	rows := renderMarkdown(mdColor(), "a **bold** b `code` c *ital* d ~~gone~~ e [link](http://x)", 60)
	joined := strings.Join(rows, "\n")
	for _, want := range []string{
		ansiBold + "bold" + ansiReset,
		ansiDim + "code" + ansiReset,
		"\x1b[3m" + "ital" + ansiReset,
		"\x1b[9m" + "gone" + ansiReset,
		ansiAccent + "link" + ansiReset,
		"(http://x)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("inline output missing %q:\n%q", want, joined)
		}
	}
}

func TestMarkdownList(t *testing.T) {
	rows := renderMarkdown(mdColor(), "Intro:\n\n- one\n- two words\n  continued\n3. third\n", 40)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, ansiMuted+"•"+ansiReset+" one") {
		t.Errorf("bullet missing:\n%q", joined)
	}
	if !strings.Contains(joined, "two words continued") {
		t.Errorf("continuation line not merged into its item:\n%q", joined)
	}
	if !strings.Contains(joined, ansiMuted+"3."+ansiReset+" third") {
		t.Errorf("ordered marker missing:\n%q", joined)
	}
	assertRowsWithinWidth(t, rows, 40)
}

func TestMarkdownQuoteAndHR(t *testing.T) {
	rows := renderMarkdown(mdColor(), "> quoted line\n\n---\n", 40)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "▎") || !strings.Contains(joined, "quoted") || !strings.Contains(joined, "line") {
		t.Errorf("quote bar or text missing:\n%q", joined)
	}
	if !strings.Contains(joined, strings.Repeat("─", 40)) {
		t.Errorf("hr rule missing or wrong width:\n%q", joined)
	}
	assertRowsWithinWidth(t, rows, 40)
}

func TestMarkdownParagraphWrapsAndStylesSurvive(t *testing.T) {
	text := "**" + strings.Repeat("boldword ", 15) + "** tail"
	rows := renderMarkdown(mdColor(), text, 30)
	if len(rows) < 3 {
		t.Fatalf("expected wrapping, got %d rows:\n%q", len(rows), strings.Join(rows, "\n"))
	}
	assertRowsWithinWidth(t, rows, 30)
	// The bold span survives the wrap: every row it touches carries the code.
	boldRows := 0
	for _, r := range rows {
		if strings.Contains(r, ansiBold) {
			boldRows++
		}
	}
	if boldRows < 2 {
		t.Errorf("bold styling lost across the wrap (only %d rows carry it):\n%q", boldRows, strings.Join(rows, "\n"))
	}
	// The plain tail after the span is unstyled.
	if strings.Contains(rows[len(rows)-1], ansiBold) && !strings.Contains(stripANSI(rows[len(rows)-1]), "boldword") {
		t.Errorf("style leaked past the span:\n%q", rows[len(rows)-1])
	}
}

func TestMarkdownBlockSeparation(t *testing.T) {
	rows := renderMarkdown(mdColor(), "para one\n\npara two\n\n- item", 40)
	// exactly one blank row between blocks
	blanks := 0
	for _, r := range rows {
		if stripANSI(r) == "" {
			blanks++
		}
	}
	if blanks != 2 {
		t.Errorf("want 2 separating blank rows, got %d:\n%q", blanks, strings.Join(rows, "\n"))
	}
}

func TestMarkdownNoColor(t *testing.T) {
	rows := renderMarkdown(ui{}, "# H\n\n**b** `c`", 40)
	joined := strings.Join(rows, "\n")
	if strings.Contains(joined, "\x1b[") {
		t.Errorf("no-color mode must not emit ANSI:\n%q", joined)
	}
	if !strings.Contains(joined, "H") || !strings.Contains(joined, "b") {
		t.Errorf("text content lost in no-color mode:\n%q", joined)
	}
}

func TestMarkdownHardSplitLongCodeLine(t *testing.T) {
	long := strings.Repeat("x", 100)
	rows := renderMarkdown(mdColor(), "```\n"+long+"\n```\n", 30)
	if len(rows) < 6 {
		t.Errorf("long code line not hard-split (%d rows):\n%q", len(rows), strings.Join(rows, "\n"))
	}
	assertRowsWithinWidth(t, rows, 30)
}

func TestMarkdownCJKWidth(t *testing.T) {
	rows := renderMarkdown(mdColor(), strings.Repeat("あいうえお", 10), 20)
	assertRowsWithinWidth(t, rows, 20)
	if len(rows) < 4 {
		t.Errorf("wide text not wrapped by display width (%d rows)", len(rows))
	}
}
