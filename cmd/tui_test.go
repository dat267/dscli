package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/dat267/dscli/internal/deepseek"
)

// press builds a v2 key-press message for a code (e.g. tea.KeyEnter).
func press(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

// pressText builds a v2 key-press message for a printable string.
func pressText(s string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
}

// tuiHarness builds a TUI model against the fake server with a fixed
// conversation, ready to drive via Update.
func tuiHarness(t *testing.T, completions []string, workdir string) (*tuiModel, *fakeRecorder) {
	t.Helper()
	srv, rec := fakeDeepSeekServerWith(t, completions)
	t.Cleanup(srv.Close)
	client := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	chat := &ChatCmd{Workdir: workdir, cfgPath: filepath.Join(t.TempDir(), "cfg.json")}
	m := newTUIModel(chat, client, "sess-1", false)
	return m, rec
}

// pumpTUI drains the turn goroutine's messages into the model until the turn
// finishes.
func pumpTUI(m *tuiModel) {
	for {
		msg, ok := <-m.streamCh
		if !ok {
			return
		}
		switch msg.(type) {
		case streamDone:
			m.Update(msg)
			return
		}
		m.Update(msg)
	}
}

// TestTUIStreamConcatsOnOneLine: streaming reply deltas must concatenate on
// the current line, not each start a new line (the visual-break regression).
func TestTUIStreamConcatsOnOneLine(t *testing.T) {
	m, _ := tuiHarness(t, nil, "")
	m.appendLine("hi")
	m.Update(streamDelta{text: "Hel"})
	m.Update(streamDelta{text: "lo "})
	m.Update(streamDelta{text: "world"})
	if got := m.scroll; !strings.Contains(got, "hi\nHello world") {
		t.Errorf("scroll = %q, want streamed tokens on one line", got)
	}
}

// TestTUIScroll: the chat pane is a scrollable window; it auto-follows the
// bottom, PgUp scrolls back, PgDn returns to the bottom.
func TestTUIScroll(t *testing.T) {
	m, _ := tuiHarness(t, nil, "")
	m.height, m.width = 20, 60
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	m.scroll = sb.String()
	m.clampView()
	avail := m.outputRows()
	bottom := 51 - avail // 50 lines + trailing empty
	if m.viewTop != bottom {
		t.Errorf("auto-scroll viewTop = %d, want %d", m.viewTop, bottom)
	}
	m.scrollUp()
	if m.viewTop >= bottom {
		t.Errorf("scrollUp did not move: viewTop=%d", m.viewTop)
	}
	m.scrollDown()
	if !m.autoScroll || m.viewTop != bottom {
		t.Errorf("scrollDown should return to bottom: viewTop=%d autoScroll=%v", m.viewTop, m.autoScroll)
	}
}

// TestTUIViewLayout: the view keeps the chat pane, the input box (bordered)
// and the status line; the status line sits after the input box.
func TestTUIViewLayout(t *testing.T) {
	m, _ := tuiHarness(t, nil, "")
	m.height, m.width = 24, 80
	m.appendLine(renderUserLine("hi"))
	m.Update(streamDelta{text: "Hello"})
	v := m.View().Content
	for _, want := range []string{"hi", "Hello", ":::"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
	// The chat pane renders above the input, and the status line below it.
	chatIdx := strings.Index(v, "Hello")
	boxIdx := strings.Index(v, ":::")
	statusIdx := strings.Index(v, "DeepSeek · model")
	if !(chatIdx >= 0 && boxIdx >= 0 && statusIdx >= 0 && chatIdx < boxIdx && boxIdx < statusIdx) {
		t.Errorf("layout order wrong (chat=%d box=%d status=%d):\n%s", chatIdx, boxIdx, statusIdx, v)
	}
}

// TestTUIViewPinnedToBottom: the rendered view always fills the full terminal
// height — the input box sits directly above the status line at the very
// bottom, even when the conversation is short (the chat pane pads with blank
// rows) or overflows (the window shows the last rows).
func TestTUIViewPinnedToBottom(t *testing.T) {
	cases := []struct {
		name    string
		height  int
		scroll  string
		content string
	}{
		{"empty", 12, "", "hello"},
		{"short", 12, "line one\n", "abc"},
		{"multiline-input", 14, "chat\n", "first\nsecond"},
		{"overflow", 12, strings.Repeat("fill\n", 40), "tail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := tuiHarness(t, nil, "")
			m.height, m.width = tc.height, 60
			m.scroll = tc.scroll
			m.input.SetValue(tc.content)
			m.input.End() // cursor at the end, so the block appends, not overlays
			v := m.View().Content
			rows := strings.Split(strings.TrimSuffix(v, "\n"), "\n")
			if len(rows) != tc.height {
				t.Fatalf("view has %d rows, want %d", len(rows), tc.height)
			}
			statusRow := tc.height - 1
			inputRows := m.inputHeight()
			inputTop := statusRow - inputRows
			if !strings.Contains(rows[statusRow], "DeepSeek") {
				t.Errorf("last row is not the status line: %q", rows[statusRow])
			}
			if !strings.Contains(rows[inputTop], ":::") {
				t.Errorf("row %d is not the input prompt row: %q", inputTop, rows[inputTop])
			}
			for _, cl := range strings.Split(tc.content, "\n") {
				found := false
				for _, r := range rows {
					if strings.Contains(r, cl) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("input line %q not rendered", cl)
				}
			}
			// The chat pane occupies the top rows above the input.
			if tc.scroll != "" && !strings.Contains(rows[0], strings.TrimSpace(strings.Split(tc.scroll, "\n")[0])) {
				t.Errorf("first row is not chat content: %q", rows[0])
			}
		})
	}
}

// TestTUISpaceInput: a lone space arrives as KeySpace and must be inserted.
func TestTUISpaceInput(t *testing.T) {
	m, _ := tuiHarness(t, nil, "")
	m.Update(press(' '))
	m.Update(pressText("a"))
	m.Update(press(' '))
	if got := m.input.Value(); got != " a " {
		t.Errorf("input = %q, want %q", got, " a ")
	}
}

// TestTUIUserLineStyled: a submitted user message renders with a subtle
// background and no "you>" prefix.
func TestTUIUserLineStyled(t *testing.T) {
	m, _ := tuiHarness(t, []string{completionSSE(t, 2, "ok")}, "")
	m.input.SetValue("hello")
	m.Update(press(tea.KeyEnter))
	pumpTUI(m)
	if !strings.Contains(m.scroll, "\x1b[38;2;107;80;255m") {
		t.Errorf("user line missing foreground ANSI:\n%q", m.scroll)
	}
	if !strings.Contains(m.scroll, "hello") {
		t.Errorf("user message missing:\n%q", m.scroll)
	}
	if strings.Contains(m.scroll, "you>") {
		t.Errorf("user prompt should be removed:\n%q", m.scroll)
	}
}

// TestTUIInputWrap: long input text wraps inside the fixed 2-row textarea and
// the cursor stays visible.
func TestTUIInputWrap(t *testing.T) {
	m, _ := tuiHarness(t, nil, "")
	m.width = 30
	m.input.SetValue(strings.Repeat("x", 60))
	m.input.End() // cursor at the end (appended block)
	box := m.input.render(m.width)
	rows := strings.Split(box, "\n")
	if len(rows) != maxInputLines {
		t.Fatalf("box rows = %d, want %d", len(rows), maxInputLines)
	}
	// 60 chars at boxW=26 (width minus the 4-col "::: " prompt) wrap into
	// 26+26+8; the first row is a full wrap.
	if !strings.Contains(rows[0], strings.Repeat("x", 26)) {
		t.Errorf("row 0 not wrapped at text width: %q", rows[0])
	}
	if !strings.Contains(box, "█") {
		t.Errorf("end-of-text block cursor missing:\n%s", box)
	}
}

// TestTUICursorAtExactWidth: a cursor at the very end of a row that exactly
// fills the text width must not panic (slice-bounds regression) and still draw
// a block.
func TestTUICursorAtExactWidth(t *testing.T) {
	m, _ := tuiHarness(t, nil, "")
	m.width = 30 // text width = 26
	m.input.SetValue(strings.Repeat("x", 26))
	m.input.End() // col = 26 = text width
	box := m.input.render(m.width)
	if !strings.Contains(box, "█") {
		t.Errorf("cursor block missing:\n%s", box)
	}
}

// TestTUICursorOverlay: the block cursor shows the character under it in
// reverse video (the opposite of the cursor colour) instead of inserting a
// solid cell that shifts the text.
func TestTUICursorOverlay(t *testing.T) {
	m, _ := tuiHarness(t, nil, "")
	m.width = 30
	m.input.SetValue("hello")
	m.input.Home() // cursor at the start: over the 'h'
	box := m.input.render(m.width)
	if strings.Contains(box, "█hello") {
		t.Errorf("cursor inserted itself (shifting text): %q", box)
	}
	if !strings.Contains(box, "\x1b[7mh\x1b[0m") {
		t.Errorf("char under cursor should render in reverse video: %q", box)
	}
}

// TestTUIClear: /clear forgets the persisted default session and starts a
// fresh conversation.
func TestTUIClear(t *testing.T) {
	m, rec := tuiHarness(t, nil, "")
	if err := saveSession(m.cfgPath, "sess-1:2"); err != nil {
		t.Fatal(err)
	}
	m.conversation = "sess-1:2"
	m.input.SetValue("/clear")
	m.Update(press(tea.KeyEnter))
	if got := loadSavedSession(m.cfgPath); got != "sess-1" {
		t.Errorf("persisted session after /clear = %q, want fresh sess-1", got)
	}
	rec.mu.Lock()
	creates := rec.creates
	rec.mu.Unlock()
	if creates != 1 {
		t.Errorf("/clear created %d sessions, want 1 (fresh conversation)", creates)
	}
	if !strings.Contains(m.scroll, "new conversation") {
		t.Errorf("missing new-conversation note:\n%q", m.scroll)
	}
}

// TestTUIPasteMultiline: pasted text with CRLF / lone CR line endings becomes
// clean '\n' line breaks, and tabs expand to spaces.
func TestTUIPasteMultiline(t *testing.T) {
	m, _ := tuiHarness(t, nil, "")
	pasted := "line one\r\nline two\rlast line\there"
	m.Update(tea.PasteMsg{Content: pasted})
	if got := m.input.Value(); got != "line one\nline two\nlast line    here" {
		t.Errorf("pasted value = %q", got)
	}
}

// TestTUIPasteNewline: a newline inside pasted text inserts a line break
// instead of submitting the prompt.
func TestTUIPasteNewline(t *testing.T) {
	m, _ := tuiHarness(t, nil, "")
	m.input.SetValue("abc")
	m.input.End()
	m.Update(tea.PasteMsg{Content: "\n"})
	if got := m.input.Value(); got != "abc\n" {
		t.Errorf("pasted newline should append a line break, got %q", got)
	}
	if m.busy {
		t.Error("paste must not submit")
	}
}

// TestTUICursorUpDown: Up/Down move the cursor through the multiline input;
// at the first/last line they fall back to history navigation.
func TestTUICursorUpDown(t *testing.T) {
	m, _ := tuiHarness(t, nil, "")
	m.input.SetValue("line one\nline two\nline three")
	// Cursor starts at (0,0); Down moves into the following lines.
	m.Update(press(tea.KeyDown))
	if m.input.row != 1 {
		t.Errorf("down: row = %d, want 1", m.input.row)
	}
	m.Update(press(tea.KeyDown))
	if m.input.row != 2 {
		t.Errorf("down: row = %d, want 2", m.input.row)
	}
	m.Update(press(tea.KeyUp))
	if m.input.row != 1 {
		t.Errorf("up: row = %d, want 1", m.input.row)
	}
	// At the first line, Up recalls history instead of moving the cursor.
	m.addHistory("previous")
	m.Update(press(tea.KeyUp))
	m.Update(press(tea.KeyUp))
	if got := m.input.Value(); got != "previous" {
		t.Errorf("up at first line should recall history, got %q", got)
	}
}

// TestTUIScrollbackWraps: long scrollback lines are word-wrapped to the pane
// width instead of relying on the terminal to wrap them.
func TestTUIScrollbackWraps(t *testing.T) {
	m, _ := tuiHarness(t, nil, "")
	m.height, m.width = 12, 40
	m.appendText(strings.Repeat("word ", 30)) // 150 chars, no newline
	rows := m.wrappedRows()
	if len(rows) <= 1 {
		t.Fatalf("expected wrapped rows, got %d", len(rows))
	}
	for i, r := range rows {
		if n := len([]rune(r)); n > 40 {
			t.Errorf("row %d too long (%d cells): %q", i, n, r)
		}
	}
	// A styled user line wraps and keeps its colour on every row.
	m2, _ := tuiHarness(t, nil, "")
	m2.height, m2.width = 12, 20
	m2.appendLine(renderUserLine(strings.Repeat("styled ", 10)))
	rows2 := m2.wrappedRows()
	if len(rows2) <= 1 {
		t.Fatalf("styled line not wrapped")
	}
	for _, r := range rows2 {
		if r == "" {
			continue // the blank separator row is unstyled by design
		}
		if !strings.Contains(r, "\x1b[38;2;107;80;255m") {
			t.Errorf("styled row missing colour: %q", r)
		}
	}
}

// TestTUISubmitStreamsReply: submit a prompt, then stream the reply into the
// scrollback.
func TestTUISubmitStreamsReply(t *testing.T) {
	m, _ := tuiHarness(t, []string{completionSSE(t, 2, "Hello TUI")}, "")
	m.input.SetValue("hi")
	m.Update(press(tea.KeyEnter))
	if !m.busy {
		t.Fatal("submit should start a turn")
	}
	pumpTUI(m)
	if m.busy {
		t.Fatal("turn should finish")
	}
	if got := m.scroll; !strings.Contains(got, "Hello TUI") {
		t.Errorf("output missing reply: %q", got)
	}
	if m.conversation != "sess-1:2" {
		t.Errorf("conversation = %q, want sess-1:2", m.conversation)
	}
	if got := loadSavedSession(m.cfgPath); got != "sess-1:2" {
		t.Errorf("persisted conversation = %q, want sess-1:2", got)
	}
}

func TestTUISlashCommands(t *testing.T) {
	m, rec := tuiHarness(t, nil, "")

	m.input.SetValue("/thinking")
	m.Update(press(tea.KeyEnter))
	if !m.thinking {
		t.Error("/thinking should enable DeepThink")
	}

	m.input.SetValue("/model expert")
	m.Update(press(tea.KeyEnter))
	if m.model != "expert" {
		t.Errorf("model = %q, want expert", m.model)
	}
	// /model starts a fresh conversation: a new session was created.
	rec.mu.Lock()
	creates := rec.creates
	rec.mu.Unlock()
	if creates != 1 {
		t.Errorf("/model created %d sessions, want 1 (fresh thread)", creates)
	}
}

func TestTUISuggestions(t *testing.T) {
	m, _ := tuiHarness(t, nil, "")
	m.input.SetValue("/m")
	m.updateSuggestions()
	if len(m.suggestions) != 1 || m.suggestions[0] != "/model" {
		t.Errorf("suggestions = %v, want [/model]", m.suggestions)
	}
	// A single suggestion: Tab completes it with a trailing space and the
	// cursor past it, ready for an argument.
	m.input.SetValue("/mod")
	m.updateSuggestions()
	m.Update(press(tea.KeyTab))
	if got := m.input.Value(); got != "/model " {
		t.Errorf("tab completed to %q, want %q", got, "/model ")
	}
	if m.input.col != len([]rune("/model ")) {
		t.Errorf("cursor col = %d, want %d", m.input.col, len([]rune("/model ")))
	}
	// A command that takes no parameter completes without a trailing space.
	m.input.SetValue("/ne")
	m.updateSuggestions()
	m.Update(press(tea.KeyTab))
	if got := m.input.Value(); got != "/new" {
		t.Errorf("tab completed no-arg command to %q, want %q", got, "/new")
	}
	// Multiple suggestions cycle on Tab (no premature completion).
	m.input.SetValue("/")
	m.updateSuggestions()
	if len(m.suggestions) < 2 {
		t.Fatalf("bare / should offer several commands, got %v", m.suggestions)
	}
	first := m.suggestions[m.suggestIdx]
	m.Update(press(tea.KeyTab))
	if m.suggestions[m.suggestIdx] == first {
		t.Errorf("tab did not cycle through multiple suggestions")
	}
}

// TestTUISuggestionsVertical: the completion menu renders one entry per row,
// and a long list scrolls to keep the highlighted entry visible.
func TestTUISuggestionsVertical(t *testing.T) {
	m, _ := tuiHarness(t, nil, "")
	m.suggestions = []string{"a", "b", "c"}
	m.suggestIdx = 1
	got := m.renderSuggestions()
	if !strings.Contains(got, "\n") {
		t.Errorf("vertical menu missing line breaks: %q", got)
	}
	if m.suggestionRows() != 3 {
		t.Errorf("suggestionRows = %d, want 3", m.suggestionRows())
	}

	// A long list is capped and scrolls; the below-marker tail shrinks as you
	// navigate down and disappears at the bottom.
	var many []string
	for i := 0; i < 20; i++ {
		many = append(many, fmt.Sprintf("item%d", i))
	}
	m.suggestions = many
	m.suggestIdx = 15
	rendered := m.renderSuggestions()
	if !strings.Contains(rendered, "item15") {
		t.Errorf("highlighted item not visible in scrolled menu: %v", rendered)
	}
	if !strings.Contains(rendered, "↓ 4 more") {
		t.Errorf("missing below-tail at mid-list: %v", rendered)
	}
	if !strings.Contains(rendered, "↑ 8 more") {
		t.Errorf("missing above-tail at mid-list: %v", rendered)
	}
	// suggestionRows matches the actual rendered line count.
	if got := strings.Count(rendered, "\n") + 1; got != m.suggestionRows() {
		t.Errorf("rendered rows = %d, suggestionRows = %d", got, m.suggestionRows())
	}
	// At the bottom the below-tail is gone.
	m.suggestIdx = 19
	rendered = m.renderSuggestions()
	if strings.Contains(rendered, "↓") {
		t.Errorf("below-tail should vanish at the bottom: %v", rendered)
	}
	if !strings.Contains(rendered, "item19") {
		t.Errorf("last item not visible: %v", rendered)
	}
}

// TestTUIHandleFile: /file <path> loads a file into a buffer prepended to the
// next message; repeating the command stacks files one by one.
func TestTUIHandleFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("contents of a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("contents of b"), 0644); err != nil {
		t.Fatal(err)
	}
	m, rec := tuiHarness(t, []string{completionSSE(t, 2, "ok")}, dir)

	m.input.SetValue("/file a.txt")
	m.Update(press(tea.KeyEnter))
	if len(m.loaded) != 1 {
		t.Fatalf("loaded = %d, want 1", len(m.loaded))
	}
	if !strings.Contains(m.loaded[0].block, "contents of a") {
		t.Errorf("loaded block missing file contents: %q", m.loaded[0].block)
	}
	// A second /file stacks the next file one by one.
	m.input.SetValue("/file b.txt")
	m.Update(press(tea.KeyEnter))
	if len(m.loaded) != 2 {
		t.Fatalf("loaded = %d, want 2", len(m.loaded))
	}
	// Submitting sends both loaded files ahead of the typed message, then
	// drops the buffer.
	m.input.SetValue("summarize these")
	m.Update(press(tea.KeyEnter))
	pumpTUI(m)
	prompt, _ := completionBody(t, rec, 0)
	if !strings.Contains(prompt, "contents of a") || !strings.Contains(prompt, "contents of b") {
		t.Errorf("sent prompt missing loaded files:\n%q", prompt)
	}
	if !strings.HasSuffix(strings.TrimSpace(prompt), "summarize these") {
		t.Errorf("sent prompt should end with the typed message:\n%q", prompt)
	}
	if len(m.loaded) != 0 {
		t.Errorf("loaded buffer not cleared after send: %d", len(m.loaded))
	}

	// A missing file reports an error and loads nothing.
	m.input.SetValue("/file nope.txt")
	m.Update(press(tea.KeyEnter))
	if len(m.loaded) != 0 {
		t.Errorf("missing file should not load, got %d", len(m.loaded))
	}
	if !strings.Contains(m.scroll, "could not load nope.txt") {
		t.Errorf("missing-file error not shown:\n%q", m.scroll)
	}
}

func TestTUIHistory(t *testing.T) {
	m, _ := tuiHarness(t, []string{
		completionSSE(t, 2, "one"),
		completionSSE(t, 3, "two"),
	}, "")

	m.input.SetValue("first")
	m.Update(press(tea.KeyEnter))
	pumpTUI(m)

	m.input.SetValue("second")
	m.Update(press(tea.KeyEnter))
	pumpTUI(m)

	// Up recalls the previous prompt into the input.
	m.input.SetValue("")
	m.Update(press(tea.KeyUp))
	if got := m.input.Value(); got != "second" {
		t.Errorf("up 1 = %q, want second", got)
	}
	m.Update(press(tea.KeyUp))
	if got := m.input.Value(); got != "first" {
		t.Errorf("up 2 = %q, want first", got)
	}
	m.Update(press(tea.KeyDown))
	if got := m.input.Value(); got != "second" {
		t.Errorf("down = %q, want second", got)
	}
}

// TestTUIHistoryDraftPreserved: pressing Up to recall history must not lose
// the currently typed prompt — Down past the newest entry restores it.
func TestTUIHistoryDraftPreserved(t *testing.T) {
	m, _ := tuiHarness(t, []string{
		completionSSE(t, 2, "one"),
		completionSSE(t, 3, "two"),
	}, "")

	m.input.SetValue("first")
	m.Update(press(tea.KeyEnter))
	pumpTUI(m)
	m.input.SetValue("second")
	m.Update(press(tea.KeyEnter))
	pumpTUI(m)

	// Type a fresh draft, then accidentally press Up: it must be stashed, not
	// lost.
	m.input.SetValue("my draft")
	m.Update(press(tea.KeyUp))
	if got := m.input.Value(); got != "second" {
		t.Errorf("up 1 = %q, want second", got)
	}
	// Down past the newest entry restores the draft.
	m.Update(press(tea.KeyDown))
	if got := m.input.Value(); got != "my draft" {
		t.Errorf("down past newest = %q, want the stashed draft %q", got, "my draft")
	}
}

// TestTUIRenderHistory: past messages render in message-id order, user lines
// styled violet, assistant replies plain, empty messages skipped.
func TestTUIRenderHistory(t *testing.T) {
	hist := []deepseek.HistoryMessage{
		{MessageID: 2, Role: "ASSISTANT", Content: "Hello back"},
		{MessageID: 1, Role: "USER", Content: "Hi"},
	}
	got := renderHistory(hist)
	u := strings.Index(got, "\x1b[38;2;107;80;255mHi\x1b[0m")
	a := strings.Index(got, "Hello back")
	if u < 0 || a < 0 || u > a {
		t.Errorf("history rendered out of order (user=%d assistant=%d):\n%q", u, a, got)
	}
	if got2 := renderHistory([]deepseek.HistoryMessage{{MessageID: 1, Role: "USER", Content: ""}}); got2 != "" {
		t.Errorf("empty history = %q, want empty", got2)
	}
}

// TestTUIWheelAndHomeEndScroll: the mouse wheel scrolls a few lines, Home
// jumps to the top, End returns to the bottom with auto-follow on.
func TestTUIWheelAndHomeEndScroll(t *testing.T) {
	m, _ := tuiHarness(t, nil, "")
	m.height, m.width = 20, 60
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	m.scroll = sb.String()
	m.clampView()
	avail := m.outputRows()
	bottom := 51 - avail

	m.scrollUpN(wheelScrollLines)
	if m.viewTop != bottom-wheelScrollLines {
		t.Errorf("wheel up viewTop = %d, want %d", m.viewTop, bottom-wheelScrollLines)
	}
	m.scrollTop()
	if m.viewTop != 0 {
		t.Errorf("home viewTop = %d, want 0", m.viewTop)
	}
	m.scrollBottom()
	if m.viewTop != bottom || !m.autoScroll {
		t.Errorf("end viewTop = %d auto=%v, want %d true", m.viewTop, m.autoScroll, bottom)
	}
	m.scrollTop()
	m.scrollDownN(wheelScrollLines)
	if m.viewTop != wheelScrollLines {
		t.Errorf("wheel down viewTop = %d, want %d", m.viewTop, wheelScrollLines)
	}
}
