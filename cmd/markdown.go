package cmd

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// renderMarkdown renders chat markdown (the model's replies) to styled,
// display-width-safe rows in the Pi chat conventions: bold+accent headings,
// bordered code boxes with a dim language label, dim bullets, quote bars and
// rules. Every returned row's visible width is at most `width` (an invariant
// covered by tests), so rows can be fed straight into the scrollback. u
// controls whether any ANSI styling is emitted.
//
// It is deliberately line-oriented and partial-input friendly: an unclosed
// code fence at the end of the text renders as a complete box (with the
// content so far), so a streaming turn re-renders cleanly every frame.
func renderMarkdown(u ui, text string, width int) []string {
	if width < 1 {
		width = 80
	}
	lines := strings.Split(text, "\n")
	var out []string
	emit := func(rows ...string) {
		if len(rows) == 0 {
			return
		}
		if len(out) > 0 {
			out = append(out, "") // one blank row between blocks
		}
		out = append(out, rows...)
	}

	var para []string
	flushPara := func() {
		if len(para) == 0 {
			return
		}
		emit(wrapStyled(mdInline(u, strings.Join(para, " ")), width, "")...)
		para = nil
	}

	inCode := false
	var fenceMarker, codeLang string
	var codeLines []string
	flushCode := func() {
		emit(codeBoxRows(u, codeLang, codeLines, width)...)
		inCode = false
		codeLang, codeLines = "", nil
	}

	for i := 0; i < len(lines); {
		line := strings.TrimRight(lines[i], " \t\r")
		t := strings.TrimSpace(line)

		if inCode {
			if isFenceClose(t, fenceMarker) {
				flushCode()
			} else {
				codeLines = append(codeLines, line)
			}
			i++
			continue
		}
		switch {
		case t == "":
			flushPara()
			i++
		case isFenceOpen(t):
			flushPara()
			inCode = true
			fenceMarker = fenceChars(t)
			codeLang = strings.TrimSpace(strings.TrimPrefix(t, fenceMarker))
			i++
		case isHR(t):
			flushPara()
			emit(u.dim(strings.Repeat("─", width)))
			i++
		case strings.HasPrefix(t, "#"):
			flushPara()
			emit(headingRows(u, t, width)...)
			i++
		case strings.HasPrefix(t, ">"):
			flushPara()
			var texts []string
			for i < len(lines) {
				q := strings.TrimSpace(strings.TrimSpace(lines[i]))
				if !strings.HasPrefix(q, ">") {
					break
				}
				texts = append(texts, strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(q, ">"), " ")))
				i++
			}
			emit(quoteRows(u, texts, width)...)
		case isListItem(t):
			flushPara()
			emit(listRows(u, lines, &i, width)...)
		default:
			para = append(para, t)
			i++
		}
	}
	if inCode {
		flushCode()
	}
	flushPara()
	return out
}

// codeBoxRows renders a fenced code block: a dim box whose top border
// carries the language label, with the content hard-wrapped inside.
func codeBoxRows(u ui, lang string, body []string, width int) []string {
	inner := width - 4 // "│ " + content + " │"
	if inner < 1 {
		inner = 1
	}
	top := "─"
	if lang != "" {
		top += " " + lang + " "
	}
	if pad := width - runewidth.StringWidth(top); pad > 0 {
		top += strings.Repeat("─", pad)
	} else {
		top = hardWrapANSIFree(top, width)[0]
		top += strings.Repeat(" ", max(0, width-runewidth.StringWidth(top)))
	}
	rows := []string{u.dim(top)}
	for _, cl := range body {
		for _, chunk := range hardWrapANSIFree(strings.ReplaceAll(cl, "\t", "    "), inner) {
			rows = append(rows, u.dim("│")+" "+chunk+strings.Repeat(" ", max(0, inner-runewidth.StringWidth(chunk)))+" "+u.dim("│"))
		}
	}
	return append(rows, u.dim(strings.Repeat("─", width)))
}

// headingRows renders "# heading" (h1/h2 accent+bold, h3+ bold), wrapped.
func headingRows(u ui, t string, width int) []string {
	level := 0
	for level < len(t) && t[level] == '#' {
		level++
	}
	text := strings.TrimSpace(t[min(level, len(t)):])
	if text == "" {
		return nil
	}
	var styled string
	if level <= 2 {
		styled = u.wrap(ansiBold+ansiAccent, text)
	} else {
		styled = u.bold(text)
	}
	return wrapStyled(styled, width, "")
}

// quoteRows renders "> quoted" lines as a dim bar + dim text block, each
// line wrapped with a hanging indent under the bar.
func quoteRows(u ui, texts []string, width int) []string {
	var rows []string
	for _, inner := range texts {
		r := wrapStyled(u.note(inner), max(1, width-2), "  ")
		if len(r) == 0 {
			continue
		}
		r[0] = u.dim("▎ ") + r[0]
		rows = append(rows, r...)
	}
	return rows
}

// listRows consumes a run of list items (plus indented continuation lines
// appended to the previous item) starting at *i, returning styled rows.
func listRows(u ui, lines []string, i *int, width int) []string {
	type item struct {
		indent int
		marker string
		text   string
	}
	var items []item
	for *i < len(lines) {
		t := strings.TrimSpace(lines[*i])
		if m := mdListItemRE.FindStringSubmatch(strings.TrimRight(lines[*i], " \t\r")); m != nil {
			indent := len(m[1]) / 2
			if indent > 3 {
				indent = 3
			}
			marker := "•"
			if m[3] != "" {
				marker = m[3] + "."
			}
			items = append(items, item{indent, marker, m[4]})
			*i++
			continue
		}
		// Continuation: an indented plain line extends the previous item.
		if len(items) > 0 && t != "" && (lines[*i][0] == ' ' || lines[*i][0] == '\t') &&
			!isFenceOpen(t) && !strings.HasPrefix(t, "#") && !strings.HasPrefix(t, ">") {
			items[len(items)-1].text += " " + t
			*i++
			continue
		}
		break
	}
	var rows []string
	for _, it := range items {
		hang := strings.Repeat("  ", it.indent) + u.muted(it.marker) + " "
		hangW := textWidth(stripANSI(hang))
		body := wrapStyled(mdInline(u, it.text), max(1, width-hangW), strings.Repeat(" ", hangW))
		for j, r := range body {
			if j == 0 {
				rows = append(rows, hang+r)
			} else {
				rows = append(rows, r)
			}
		}
	}
	return rows
}

// mdInline styles inline markdown: `code`, **bold**, __bold__, *italic*,
// _italic_, ~~strike~~ and [text](url). Code spans win over the other rules.
func mdInline(u ui, s string) string {
	return mdInlineRE.ReplaceAllStringFunc(s, func(match string) string {
		switch {
		case strings.HasPrefix(match, "`"):
			return u.dim(match[1 : len(match)-1])
		case strings.HasPrefix(match, "**"):
			return u.bold(match[2 : len(match)-2])
		case strings.HasPrefix(match, "~~"):
			return u.wrap("\x1b[9m", match[2:len(match)-2])
		case strings.HasPrefix(match, "__"):
			return u.bold(match[2 : len(match)-2])
		case strings.HasPrefix(match, "["):
			m := mdLinkRE.FindStringSubmatch(match)
			if m == nil {
				return match
			}
			return u.accent(m[1]) + u.muted(" ("+m[2]+")")
		case strings.HasPrefix(match, "*"):
			return u.wrap("\x1b[3m", match[1:len(match)-1])
		default: // _italic_
			return u.wrap("\x1b[3m", match[1:len(match)-1])
		}
	})
}

var (
	mdInlineRE   = regexp.MustCompile("`[^`]+`|(\\*\\*[^*]+\\*\\*)|(~~[^~]+~~)|(__[^_]+__)|(\\*[^*\\s][^*]*\\*)|(_[^_\\s][^_]*_)|(\\[[^\\]\\n]+\\]\\([^)\\s]+\\))")
	mdLinkRE     = regexp.MustCompile(`^\[([^\]\n]+)\]\(([^)\s]+)\)$`)
	mdListItemRE = regexp.MustCompile(`^(\s*)(?:([-*+])|(\d{1,3})\.)\s+(.+)$`)
)

// isFenceOpen reports whether the line opens a ``` or ~~~ code block.
func isFenceOpen(t string) bool {
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

// fenceChars returns the fence marker of an opening line.
func fenceChars(t string) string {
	if strings.HasPrefix(t, "```") {
		return "```"
	}
	return "~~~"
}

// isFenceClose reports whether the line closes the given fence marker.
func isFenceClose(t, marker string) bool {
	return strings.HasPrefix(t, marker)
}

// isHR reports whether the trimmed line is a thematic break (---, ***, ___).
func isHR(t string) bool {
	if len(t) < 3 {
		return false
	}
	c := t[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for _, r := range t {
		if r != rune(c) && r != ' ' {
			return false
		}
	}
	return true
}

// isListItem reports whether the trimmed line starts a list item.
func isListItem(t string) bool {
	return mdListItemRE.MatchString(t)
}

// mdSeg is a run of visible text carrying the ANSI codes active over it.
type mdSeg struct{ code, text string }

// styledSegs splits a styled string into visible segments, each remembering
// which SGR codes are active over it, so wrapping can re-emit them per word.
func styledSegs(s string) []mdSeg {
	var segs []mdSeg
	var active strings.Builder
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			segs = append(segs, mdSeg{active.String(), cur.String()})
			cur.Reset()
		}
	}
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := strings.IndexByte(s[i:], 'm')
			if j < 0 {
				cur.WriteString(s[i:])
				break
			}
			code := s[i : i+j+1]
			flush()
			if code == ansiReset || code == "\x1b[m" {
				active.Reset()
			} else {
				active.WriteString(code)
			}
			i += j + 1
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		cur.WriteRune(r)
		i += size
	}
	flush()
	return segs
}

// wrapStyled word-wraps an ANSI-styled string so each row's content is at
// most `width` display columns, re-emitting any codes active over each word
// so styles survive row breaks. hang prefixes continuation rows only —
// callers reserve space for their own first-row prefix by subtracting it
// from width. Words longer than a whole row are hard-split.
func wrapStyled(s string, width int, hang string) []string {
	if width < 1 {
		return []string{s}
	}
	indentW := textWidth(stripANSI(hang))
	if indentW >= width {
		hang, indentW = "", 0
	}
	type word struct {
		code, text string
		w          int
	}
	var words []word
	for _, seg := range styledSegs(s) {
		for _, w := range strings.Fields(seg.text) {
			words = append(words, word{seg.code, w, textWidth(w)})
		}
	}
	if len(words) == 0 {
		return []string{""}
	}

	var rows []string
	var row strings.Builder
	rowW := 0 // content width on the current row (hang excluded)
	writeWord := func(w word) {
		if w.code != "" {
			row.WriteString(w.code)
		}
		row.WriteString(w.text)
		if w.code != "" {
			row.WriteString(ansiReset)
		}
	}
	for _, w := range words {
		// A word longer than a whole row is hard-split; the first chunk can
		// finish the current row, the rest each get their own row.
		if w.w > width {
			text := w.text
			first := true
			for text != "" {
				chunk := hardWrapANSIFree(text, width)[0]
				cw := textWidth(chunk)
				if !first || (rowW > 0 && rowW+cw > width) {
					rows = append(rows, row.String())
					row.Reset()
					row.WriteString(hang)
					rowW = 0
					first = false
				}
				writeWord(word{w.code, chunk, cw})
				rowW += cw
				text = text[len(chunk):]
			}
			continue
		}
		need := w.w
		if rowW > 0 {
			need++
		}
		if rowW > 0 && rowW+need > width {
			rows = append(rows, row.String())
			row.Reset()
			row.WriteString(hang)
			rowW = 0
			need = w.w
		}
		if rowW > 0 {
			row.WriteString(" ")
			rowW++
		}
		writeWord(w)
		rowW += w.w
	}
	rows = append(rows, row.String())
	return rows
}

// hardWrapANSIFree hard-splits a plain (code-free) string into chunks of at
// most width display columns, on rune boundaries.
func hardWrapANSIFree(s string, width int) []string {
	if width < 1 {
		return []string{s}
	}
	if runewidth.StringWidth(s) <= width {
		return []string{s}
	}
	var chunks []string
	var cur strings.Builder
	curW := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if curW+rw > width {
			chunks = append(chunks, cur.String())
			cur.Reset()
			curW = 0
		}
		cur.WriteRune(r)
		curW += rw
	}
	if cur.Len() > 0 {
		chunks = append(chunks, cur.String())
	}
	return chunks
}
