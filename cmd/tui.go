package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/dat267/dscli/internal/deepseek"
)

// tuiModel is the bubbletea model for the interactive `dscli chat` prompt: a
// bottom-pinned multi-line input with a scrollback pane above it, arrow-key
// history, and a slash-command suggestion menu — the terminal-agent feel of
// opencode / Claude Code.
//
// Output routing: the underlying ChatCmd's answer/preview hooks are pointed
// at this model, so the model's reply and progress notes render inside the
// TUI instead of stdout/stderr. A turn runs in a goroutine and reports deltas
// over a channel that the model pumps.
type tuiModel struct {
	chat         *ChatCmd
	client       *deepseek.Client
	model        string
	thinking     bool
	search       bool
	cfgPath      string
	noPersist    bool
	conversation string
	trusted      bool
	firstTurn    bool
	owned        []string

	width, height int
	status        string
	u             ui

	scroll     string // accumulated conversation text; deltas stream onto the last line
	viewTop    int    // first visible scrollback line
	autoScroll bool   // follow the bottom when new content arrives
	input      tuiInput
	history    []string
	histIdx    int
	draft      string // input stashed while recalling history

	busy     bool
	cancel   context.CancelFunc
	streamCh chan tea.Msg

	suggestions []string
	suggestIdx  int
	suggestKind suggestKind

	turns int
	quit  bool
	err   error
}

// suggestKind distinguishes what the suggestion menu is completing, so Tab
// completes it correctly (a command gets a trailing space, a @mention is
// replaced in place).
type suggestKind int

const (
	suggestCommand suggestKind = iota
	suggestMention
)

// stream messages sent by the turn goroutine into the model.
type streamDelta struct{ text string }
type streamNote struct{ text string }
type streamDone struct {
	err     error
	convID  string
	sources []deepseek.Source
}

func newTUIModel(chat *ChatCmd, client *deepseek.Client, conversation string, trusted bool) *tuiModel {
	m := &tuiModel{
		chat:         chat,
		client:       client,
		model:        effectiveModel(chat.Model),
		thinking:     chat.Thinking,
		search:       chat.Search,
		cfgPath:      chat.cfgPath,
		noPersist:    chat.NoPersist,
		conversation: conversation,
		trusted:      trusted,
		firstTurn:    true,
		autoScroll:   true,
		u:            ui{color: true},
		input:        tuiInput{lines: [][]rune{{}}},
		histIdx:      -1,
	}
	m.refreshStatus()
	return m
}

func (m *tuiModel) refreshStatus() {
	mode := "ephemeral"
	if m.chat.Conversation != "" {
		mode = "continuing"
	} else if !m.noPersist && m.cfgPath != "" {
		mode = "persisted"
	}
	m.status = m.u.muted(fmt.Sprintf(
		"DeepSeek · model %s · thinking %s · search %s · %s",
		m.model, onoff(m.thinking), onoff(m.search), mode,
	))
}

// appendText streams reply text onto the current (last) line, so consecutive
// reply deltas concatenate instead of each starting a new line.
func (m *tuiModel) appendText(s string) {
	m.scroll += s
	m.trimScroll()
}

// appendLine ends the current line and adds a complete, newline-terminated
// line (prompts, tool notes, previews, errors).
func (m *tuiModel) appendLine(s string) {
	if m.scroll != "" && !strings.HasSuffix(m.scroll, "\n") {
		m.scroll += "\n"
	}
	m.scroll += s + "\n"
	m.trimScroll()
}

// trimScroll caps the scrollback by dropping oldest complete lines.
func (m *tuiModel) trimScroll() {
	const maxBytes = 200_000
	for len(m.scroll) > maxBytes {
		i := strings.IndexByte(m.scroll, '\n')
		if i < 0 {
			break
		}
		m.scroll = m.scroll[i+1:]
	}
}

// pump waits for the next turn message from the stream channel.
func pump(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func (m *tuiModel) Init() tea.Cmd {
	return nil
}

// startTurn launches a turn in a goroutine and starts pumping its messages.
func (m *tuiModel) startTurn(prompt string) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.busy = true
	m.streamCh = make(chan tea.Msg, 128)
	ch := m.chat
	ch.answer = func(d string) error {
		m.streamCh <- streamDelta{d}
		return nil
	}
	ch.preview = func(t string) {
		m.streamCh <- streamNote{t}
	}
	recoverStale := m.firstTurn && m.trusted
	m.firstTurn = false
	go func() {
		var convID string
		var sources []deepseek.Source
		run := func(sid string) error {
			cid, e := m.doTurn(ctx, ch, sid, prompt, &sources)
			if e == nil {
				convID = cid
			}
			return e
		}
		var err error
		if recoverStale {
			var ns string
			ns, err = recoverStaleSession(ctx, m.client, m.cfgPath, m.conversation, true, run)
			_ = ns // conversation advances via convID on success
		} else {
			err = run(m.conversation)
		}
		m.streamCh <- streamDone{err: err, convID: convID, sources: sources}
	}()
	return pump(m.streamCh)
}

// doTurn runs one user message, mirroring the scanner REPL.
func (m *tuiModel) doTurn(ctx context.Context, ch *ChatCmd, sid, prompt string, sources *[]deepseek.Source) (string, error) {
	return ch.oneTurn(ctx, m.client, sid, prompt, m.model, ch.answerWriter(), sources)
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.MouseWheelMsg:
		switch msg.Mouse().Button {
		case tea.MouseWheelUp:
			m.scrollUpN(wheelScrollLines)
		case tea.MouseWheelDown:
			m.scrollDownN(wheelScrollLines)
		}
		return m, nil
	case tea.PasteMsg:
		for _, r := range normalizePasteRunes([]rune(msg.Content)) {
			m.input.InsertRune(r)
		}
		m.updateSuggestions()
		return m, nil
	case tea.KeyPressMsg:
		// Viewport scrolling works even while a turn streams or a confirm is
		// pending.
		switch msg.String() {
		case "pgup", "ctrl+u":
			m.scrollUp()
			return m, nil
		case "pgdown", "ctrl+d":
			m.scrollDown()
			return m, nil
		case "home":
			m.scrollTop()
			return m, nil
		case "end":
			m.scrollBottom()
			return m, nil
		}
		return m.handleKey(msg)
	case streamDelta:
		m.appendText(msg.text)
		return m, pump(m.streamCh)
	case streamNote:
		m.appendLine(m.u.dim(msg.text))
		return m, pump(m.streamCh)
	case streamDone:
		m.busy = false
		if msg.err != nil {
			m.appendLine(m.u.red("error: " + msg.err.Error()))
			m.appendLine("")
		} else {
			m.conversation = msg.convID
			persistConversation(m.cfgPath, m.noPersist, msg.convID)
			m.renderSources(msg.sources)
			m.appendLine("")
		}
		m.turns++
		m.refreshStatus()
		return m, nil
	}
	return m, nil
}

func (m *tuiModel) renderSources(sources []deepseek.Source) {
	if len(sources) == 0 {
		return
	}
	m.appendLine("")
	m.appendLine("Sources:")
	for i, s := range sources {
		if s.Title != "" {
			m.appendLine(fmt.Sprintf("  [%d] %s — %s", i+1, s.Title, s.URL))
		} else {
			m.appendLine(fmt.Sprintf("  [%d] %s", i+1, s.URL))
		}
	}
}

func (m *tuiModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.busy {
		if msg.String() == "ctrl+c" {
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		}
		return m, nil
	}

	switch msg.String() {
	case "ctrl+c":
		m.quitCleanup()
		return m, tea.Quit
	case "enter":
		return m.handleSubmit()
	case "shift+enter", "alt+enter", "ctrl+j":
		m.input.InsertRune('\n')
		m.updateSuggestions()
		return m, nil
	case "tab":
		if len(m.suggestions) == 1 {
			m.completeSuggestion()
		} else if len(m.suggestions) > 1 {
			m.cycleSuggestion()
		}
		return m, nil
	case "shift+tab":
		m.suggestPrev()
		return m, nil
	case "esc":
		m.input.Clear()
		m.suggestions = nil
		return m, nil
	case "up":
		if len(m.suggestions) > 0 {
			m.suggestPrev()
			return m, nil
		}
		// Move the cursor up through multiline input; at the first line,
		// recall history instead.
		if m.input.row > 0 {
			m.input.MoveUp()
			return m, nil
		}
		m.historyPrev()
		return m, nil
	case "down":
		if len(m.suggestions) > 0 {
			m.suggestNext()
			return m, nil
		}
		// Move the cursor down; at the last line, go to the next history entry.
		if m.input.row < len(m.input.lines)-1 {
			m.input.MoveDown()
			return m, nil
		}
		m.historyNext()
		return m, nil
	case "left":
		m.input.MoveLeft()
		return m, nil
	case "right":
		m.input.MoveRight()
		return m, nil
	case "home":
		m.input.Home()
		return m, nil
	case "end":
		m.input.End()
		return m, nil
	case "backspace":
		m.input.Backspace()
		m.updateSuggestions()
		return m, nil
	case "delete":
		m.input.Delete()
		m.updateSuggestions()
		return m, nil
	case "space":
		m.input.InsertRune(' ')
		m.updateSuggestions()
		return m, nil
	default:
		if text := msg.Text; text != "" {
			for _, r := range text {
				m.input.InsertRune(r)
			}
			m.updateSuggestions()
		}
		return m, nil
	}
}

// normalizePasteRunes converts pasted text so it renders cleanly: CRLF and a
// lone carriage return become a single '\n' line break, and tabs become four
// spaces (a raw tab would misalign the box's rune-based padding).
func normalizePasteRunes(runes []rune) []rune {
	out := make([]rune, 0, len(runes))
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '\r':
			if i+1 < len(runes) && runes[i+1] == '\n' {
				continue // CRLF: let the following \n create the break
			}
			out = append(out, '\n')
		case '\t':
			out = append(out, ' ', ' ', ' ', ' ')
		default:
			out = append(out, runes[i])
		}
	}
	return out
}

func (m *tuiModel) quitCleanup() {
	if m.cancel != nil {
		m.cancel()
	}
	if m.noPersist && len(m.owned) > 0 {
		if err := m.client.DeleteSessions(context.Background(), m.owned); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to delete session(s): %v\n", err)
		}
	}
}

// handleSubmit sends the typed input: a fully-typed slash command runs it, a
// partial one completes from the suggestion menu, and anything else becomes a
// model message.
func (m *tuiModel) handleSubmit() (tea.Model, tea.Cmd) {
	line := strings.TrimSpace(m.input.Value())
	if strings.HasPrefix(line, "/") && knownCommand(line) {
		m.input.Clear()
		m.updateSuggestions()
		return m.handleCommand(line)
	}
	// A partial command or @mention completes from the menu (Enter or Tab both
	// land the cursor at the end, ready for an argument).
	if len(m.suggestions) > 0 {
		m.completeSuggestion()
		return m, nil
	}
	if line == "" {
		return m, nil
	}
	m.input.Clear()
	m.updateSuggestions()
	m.addHistory(line)
	m.appendLine(renderUserLine(line))
	m.appendLine("") // breathing room before the reply streams
	return m, m.startTurn(m.chat.expandMentions(line))
}

// renderUserLine styles a submitted user message with a foreground colour so
// it stands apart from the plain assistant reply (no "you>" prefix — the
// input box already shows what was typed).
func renderUserLine(line string) string {
	return ansiCyan + line + ansiReset
}

// loadHistoryInto preloads a resumed conversation's past messages into the
// scrollback so the user sees the thread's context on launch. The model
// already holds the context server-side; this is purely for display. On
// failure a note is shown and the chat still works.
func loadHistoryInto(ctx context.Context, m *tuiModel, client *deepseek.Client, conversation string) {
	sess, _ := splitConversation(conversation)
	if sess == "" {
		return
	}
	hist, err := client.ChatHistory(ctx, sess)
	if err != nil {
		m.scroll = m.u.muted("note: could not load conversation history: "+err.Error()) + "\n"
		return
	}
	m.scroll = renderHistory(hist)
	m.trimScroll()
}

// renderHistory builds the scrollback text for a resumed conversation: user
// lines styled violet, assistant replies plain, one blank line between turns,
// in message-id order.
func renderHistory(hist []deepseek.HistoryMessage) string {
	sort.Slice(hist, func(i, j int) bool { return hist[i].MessageID < hist[j].MessageID })
	var b strings.Builder
	for _, msg := range hist {
		text := strings.TrimSpace(msg.Text())
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		if msg.Role == "USER" {
			b.WriteString(renderUserLine(text))
		} else {
			b.WriteString(text)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// knownCommand reports whether line is a complete slash command (with or
// without arguments), so Enter runs it instead of completing a suggestion.
func knownCommand(line string) bool {
	cmd, _, _ := strings.Cut(line, " ")
	switch cmd {
	case "/exit", "/quit", "/new", "/help", "/model", "/thinking", "/search", "/clear":
		return true
	}
	return false
}

// addHistory records a submitted prompt for Up/Down recall.
func (m *tuiModel) addHistory(p string) {
	m.history = append(m.history, p)
	m.histIdx = len(m.history)
}

// historyPrev recalls the previous entry. Starting a recall from the neutral
// position stashes the current input as draft, so an accidental Up never loses
// the typed prompt (Down past the newest entry restores it).
func (m *tuiModel) historyPrev() {
	if len(m.history) == 0 {
		return
	}
	if m.histIdx == len(m.history) {
		m.draft = m.input.Value()
	}
	if m.histIdx > 0 {
		m.histIdx--
		m.input.SetValue(m.history[m.histIdx])
	}
}

// historyNext moves forward through recalled entries; past the newest one it
// restores the stashed draft instead of wiping the input.
func (m *tuiModel) historyNext() {
	if m.histIdx >= len(m.history) {
		return
	}
	m.histIdx++
	if m.histIdx < len(m.history) {
		m.input.SetValue(m.history[m.histIdx])
		return
	}
	if m.draft != "" {
		m.input.SetValue(m.draft)
		m.draft = ""
	} else {
		m.input.Clear()
	}
}

// updateSuggestions recomputes the completion menu from the input's last line:
// slash commands first, then @file mentions.
func (m *tuiModel) updateSuggestions() {
	lines := strings.Split(m.input.Value(), "\n")
	last := lines[len(lines)-1]
	if cmds := suggestCommands(last); len(cmds) > 0 {
		m.suggestions = cmds
		m.suggestIdx = 0
		m.suggestKind = suggestCommand
		return
	}
	if ms := suggestMentions(last, m.chat.Workdir); len(ms) > 0 {
		m.suggestions = ms
		m.suggestIdx = 0
		m.suggestKind = suggestMention
		return
	}
	m.suggestions = nil
}

// completeSuggestion fills in the highlighted suggestion and clears the menu.
// Commands complete to "<command> " (cursor after the space) when they accept
// an argument, or just "<command>" when they take none; @mentions replace the
// typed @path in place with the cursor at its end. Completing a @folder
// re-opens its contents as the next suggestions, so Enter/Tab can keep
// descending; completing a file leaves nothing to suggest.
func (m *tuiModel) completeSuggestion() {
	if len(m.suggestions) == 0 {
		return
	}
	s := m.suggestions[m.suggestIdx]
	lines := strings.Split(m.input.Value(), "\n")
	last := lines[len(lines)-1]
	switch m.suggestKind {
	case suggestCommand:
		if commandTakesArg(s) {
			s += " "
		}
		lines[len(lines)-1] = s
		m.input.SetValue(strings.Join(lines, "\n"))
		m.input.End()
		m.suggestions = nil
		m.suggestIdx = 0
	case suggestMention:
		if token := mentionToken(last); token != "" {
			idx := strings.LastIndex(last, token)
			lines[len(lines)-1] = last[:idx] + s + last[idx+len(token):]
			m.input.SetValue(strings.Join(lines, "\n"))
			m.input.End()
			// A completed directory lists its entries for the next completion;
			// a completed file resolves to no suggestions.
			m.updateSuggestions()
		}
	}
}

// noArgCommands are slash commands that take no parameters, so completing
// them leaves no trailing space.
var noArgCommands = map[string]bool{
	"/exit": true, "/quit": true, "/new": true, "/help": true,
}

// commandTakesArg reports whether a completed command expects a parameter and
// should get a trailing space.
func commandTakesArg(s string) bool {
	return !noArgCommands[s]
}

func (m *tuiModel) cycleSuggestion() {
	if len(m.suggestions) == 0 {
		return
	}
	m.suggestIdx = (m.suggestIdx + 1) % len(m.suggestions)
}

func (m *tuiModel) suggestPrev() {
	if len(m.suggestions) == 0 {
		return
	}
	m.suggestIdx = (m.suggestIdx - 1 + len(m.suggestions)) % len(m.suggestions)
}

func (m *tuiModel) suggestNext() {
	m.cycleSuggestion()
}

// suggestCommands returns command completions for a partial slash token.
func suggestCommands(token string) []string {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "/") {
		return nil
	}
	var out []string
	for _, c := range []string{"/exit", "/quit", "/new", "/help", "/model", "/thinking", "/search", "/clear"} {
		if strings.HasPrefix(c, token) {
			out = append(out, c)
		}
	}
	if rest := strings.TrimSpace(strings.TrimPrefix(token, "/model")); rest != "" && strings.HasPrefix(token, "/model ") {
		for _, o := range []string{"default", "expert"} {
			if strings.HasPrefix(o, rest) {
				out = append(out, "/model "+o)
			}
		}
	}
	return out
}

// mentionTokenRE matches an @-mention path token being typed. Unlike the
// expansion regex it also matches an empty path ("@"), so a bare @ offers the
// workdir root.
var mentionTokenRE = regexp.MustCompile(`@[A-Za-z0-9_./~+-]*`)

// mentionToken returns the last @-mention token on a line, or "" when none.
func mentionToken(line string) string {
	all := mentionTokenRE.FindAllString(line, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1]
}

// maxMentionSuggestions caps how many @-mention completions are offered.
const maxMentionSuggestions = 20

// suggestMentions returns @file completions for a partial @path on the line,
// listing entries under workdir whose names extend the typed path. An already
// complete (existing) path yields no suggestions. Directories are marked with
// a trailing "/" so Tab can keep descending.
func suggestMentions(line, workdir string) []string {
	token := mentionToken(line)
	if token == "" {
		return nil
	}
	partial := strings.TrimPrefix(token, "@")
	dir, base := filepath.Split(partial)
	dir = filepath.Clean(dir)
	if dir == "." || dir == "" {
		dir = "."
	}
	// A resolved file (or the empty-partial case where base is empty but the
	// dir exists) needs no completion when the path already exists.
	if base != "" {
		if _, err := os.Stat(filepath.Join(workdir, partial)); err == nil {
			return nil
		}
	}
	entries, err := os.ReadDir(filepath.Join(workdir, dir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if len(out) >= maxMentionSuggestions {
			break
		}
		name := e.Name()
		if !strings.HasPrefix(name, base) {
			continue
		}
		rel := name
		if dir != "." {
			rel = dir + "/" + name
		}
		if e.IsDir() {
			rel += "/"
		}
		out = append(out, "@"+rel)
	}
	return out
}

// handleCommand runs a slash command, mirroring the scanner REPL.
func (m *tuiModel) handleCommand(line string) (tea.Model, tea.Cmd) {
	cmd, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
	arg = strings.TrimSpace(arg)
	switch cmd {
	case "/exit", "/quit":
		m.quitCleanup()
		return m, tea.Quit
	case "/new":
		m.newSession()
		m.appendLine(m.u.dim("new conversation"))
		m.refreshStatus()
		return m, nil
	case "/help":
		m.appendLine(m.u.dim("commands: /exit /quit /new /model /thinking /search /clear /help"))
		m.appendLine(m.u.dim("  /clear [--delete]  forget the persisted default session (--delete removes it server-side)"))
		m.appendLine(m.u.dim("enter submits · ctrl+j / alt+enter newline · up/down cursor (history at first/last line) · pgup/pgdn/home/end scroll · tab completes /commands"))
		return m, nil
	case "/model":
		if arg == "" {
			m.appendLine(m.u.dim("model: " + m.model + " (fixed per thread; /model <default|expert> starts a new conversation)"))
			return m, nil
		}
		if arg != "default" && arg != "expert" {
			m.appendLine(m.u.red(fmt.Sprintf("unknown model %q (want default or expert)", arg)))
			return m, nil
		}
		m.model = arg
		m.chat.Model = arg
		m.newSession()
		m.appendLine(m.u.dim("new conversation"))
		m.refreshStatus()
		return m, nil
	case "/thinking":
		m.thinking = toggleState(line, "/thinking", m.thinking)
		m.chat.Thinking = m.thinking
		m.refreshStatus()
		return m, nil
	case "/search":
		m.search = toggleState(line, "/search", m.search)
		m.chat.Search = m.search
		m.refreshStatus()
		return m, nil
	case "/clear":
		return m.handleClearCommand(arg)
	}
	m.appendLine(m.u.red("unknown command (/help for commands)"))
	return m, nil
}

// handleClearCommand implements `/clear [--delete]`: forget the persisted
// default session and start a fresh conversation. `--delete` also removes the
// thread server-side before forgetting it.
func (m *tuiModel) handleClearCommand(arg string) (tea.Model, tea.Cmd) {
	switch arg {
	case "":
		m.appendLine(m.u.dim("clearing persisted session"))
	case "--delete":
		sess, _ := splitConversation(m.conversation)
		if err := m.client.DeleteSessions(context.Background(), []string{sess}); err != nil {
			m.appendLine(m.u.red("error: delete server-side: " + err.Error()))
		} else {
			m.appendLine(m.u.dim("deleted session " + sess + " server-side"))
		}
	default:
		m.appendLine(m.u.red(`unknown /clear arg (want "" or "--delete")`))
		return m, nil
	}
	if m.cfgPath != "" {
		if err := clearSession(m.cfgPath); err != nil {
			m.appendLine(m.u.red("error: " + err.Error()))
		}
	}
	m.newSession()
	m.appendLine(m.u.dim("new conversation"))
	m.refreshStatus()
	return m, nil
}

// newSession starts a fresh thread: persisted (saved as the new default) or,
// with --no-persist, tracked for deletion on quit.
func (m *tuiModel) newSession() {
	sid, err := m.client.CreateChatSession(context.Background())
	if err != nil {
		m.appendLine(m.u.red("error: create chat session: " + err.Error()))
		return
	}
	m.conversation = sid
	m.trusted = false
	if m.noPersist || m.cfgPath == "" {
		m.owned = append(m.owned, sid)
	} else if err := saveSession(m.cfgPath, sid); err != nil {
		m.appendLine(m.u.red("warning: could not save session: " + err.Error()))
	}
}

// outputRows returns how many scrollback lines fit between the status line
// and the input (plus any suggestion rows).
func (m *tuiModel) outputRows() int {
	inputH := m.inputHeight()
	rows := m.height - 1 - inputH - m.suggestionRows() // 1 = status line
	if rows < 0 {
		return 0
	}
	return rows
}

// wheelScrollLines is how many rows the mouse wheel moves per tick; PgUp/
// PgDn and Home/End are coarser.
const wheelScrollLines = 3

func (m *tuiModel) scrollUp() {
	m.scrollUpN(m.outputRows())
}

func (m *tuiModel) scrollDown() {
	m.scrollDownN(m.outputRows())
}

// scrollUpN scrolls up by n rows (or to the top); scrolling away from the
// bottom disables auto-follow.
func (m *tuiModel) scrollUpN(n int) {
	m.autoScroll = false
	if n < 1 {
		n = 1
	}
	m.viewTop -= n
	if m.viewTop < 0 {
		m.viewTop = 0
	}
}

// scrollDownN scrolls down by n rows (or to the bottom, re-enabling
// auto-follow).
func (m *tuiModel) scrollDownN(n int) {
	total := m.scrollLines()
	avail := m.outputRows()
	if n < 1 {
		n = 1
	}
	if m.viewTop+n < total-avail {
		m.viewTop += n
		return
	}
	m.viewTop = total - avail
	if m.viewTop < 0 {
		m.viewTop = 0
	}
	m.autoScroll = true
}

// scrollTop jumps to the first line of the scrollback.
func (m *tuiModel) scrollTop() {
	m.autoScroll = false
	m.viewTop = 0
}

// scrollBottom jumps to the last line (auto-following new content).
func (m *tuiModel) scrollBottom() {
	total := m.scrollLines()
	avail := m.outputRows()
	m.viewTop = total - avail
	if m.viewTop < 0 {
		m.viewTop = 0
	}
	m.autoScroll = true
}

func (m *tuiModel) scrollLines() int {
	return len(m.wrappedRows())
}

// wrappedRows returns the scrollback split into rows that fit the pane width,
// word-wrapping long lines (re-applying any leading ANSI style).
func (m *tuiModel) wrappedRows() []string {
	if m.scroll == "" {
		return nil
	}
	var rows []string
	for _, line := range strings.Split(m.scroll, "\n") {
		rows = append(rows, wrapScrollLine(line, m.width)...)
	}
	return rows
}

// wrapScrollLine word-wraps a scrollback line (which may carry a single ANSI
// style wrapping its visible text, as produced by the ui helpers and
// renderUserLine) to width columns, re-applying the style to each row.
func wrapScrollLine(line string, width int) []string {
	if width < 1 {
		return []string{line}
	}
	prefix, text := splitStyled(line)
	rows := wrapTextRows(text, width)
	if prefix == "" {
		return rows
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = prefix + r + ansiReset
	}
	return out
}

// splitStyled splits a line into a leading ANSI style (empty when plain) and
// the visible text, stripping one trailing reset.
func splitStyled(line string) (prefix, text string) {
	if strings.HasPrefix(line, "\x1b[") {
		if end := strings.IndexByte(line, 'm'); end > 0 {
			prefix = line[:end+1]
			text = strings.TrimSuffix(line[end+1:], ansiReset)
			return prefix, text
		}
	}
	return "", line
}

// wrapTextRows word-wraps plain text into rows of at most width columns.
func wrapTextRows(text string, width int) []string {
	if text == "" {
		return []string{""}
	}
	rows := wrapWords([]rune(text), width)
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = string(r)
	}
	return out
}

// clampView recomputes the visible window: follow the bottom on new content
// when autoScroll is on, and keep the window in range after manual scrolling.
func (m *tuiModel) clampView() {
	total := m.scrollLines()
	avail := m.outputRows()
	if m.autoScroll || m.viewTop > total-avail {
		m.viewTop = total - avail
	}
	if m.viewTop < 0 {
		m.viewTop = 0
	}
}

func (m *tuiModel) View() tea.View {
	if m.quit {
		return tea.View{}
	}
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func (m *tuiModel) render() string {
	m.clampView()
	var b strings.Builder

	// 1. Chat scrollback (top). Rendered to exactly `avail` rows — blank lines
	// pad a short conversation so the input box is pinned to the bottom.
	rows := m.wrappedRows()
	avail := m.outputRows()
	rendered := 0
	for i := m.viewTop; i < len(rows) && rendered < avail; i++ {
		b.WriteString(rows[i])
		b.WriteString("\n")
		rendered++
	}
	for ; rendered < avail; rendered++ {
		b.WriteString("\n")
	}

	// 2. Slash-command menu sits just above the input.
	if len(m.suggestions) > 0 {
		b.WriteString(m.renderSuggestions())
		b.WriteString("\n")
	}

	// 3. Input at the bottom (open textarea with a "::: " prompt).
	b.WriteString(m.input.render(max(0, m.width)))
	b.WriteString("\n")

	// 4. Status line below the input (m.status is already dimmed; only
	// truncate to the terminal width so it never wraps).
	if m.width > 0 {
		b.WriteString(lipgloss.NewStyle().MaxWidth(m.width).Render(m.status))
	} else {
		b.WriteString(m.status)
	}
	return b.String()
}

func (m *tuiModel) inputHeight() int { return maxInputLines }

// maxInputLines is the fixed height (in rows) of the input box; text wraps
// within it.
const maxInputLines = 2

// maxSuggestionRows caps how many completion entries the menu shows; when the
// list is longer the menu scrolls, marking any entries above/below the window.
const maxSuggestionRows = 8

// suggestionWindow returns the [start,end) index range of suggestions to
// display, keeping the highlighted entry visible.
func (m *tuiModel) suggestionWindow() (start, end int) {
	n := len(m.suggestions)
	if n == 0 {
		return 0, 0
	}
	if n <= maxSuggestionRows {
		return 0, n
	}
	start = m.suggestIdx - maxSuggestionRows + 1
	if start < 0 {
		start = 0
	}
	if start+maxSuggestionRows > n {
		start = n - maxSuggestionRows
	}
	return start, start + maxSuggestionRows
}

// suggestionRows returns how many terminal rows the completion menu occupies
// (0 when no suggestions are shown), matching renderSuggestions exactly.
func (m *tuiModel) suggestionRows() int {
	if len(m.suggestions) == 0 {
		return 0
	}
	start, end := m.suggestionWindow()
	rows := end - start
	if start > 0 {
		rows++
	}
	if end < len(m.suggestions) {
		rows++
	}
	return rows
}

func (m *tuiModel) renderSuggestions() string {
	n := len(m.suggestions)
	if n == 0 {
		return ""
	}
	start, end := m.suggestionWindow()
	var b strings.Builder
	if start > 0 {
		b.WriteString(m.u.dim(fmt.Sprintf("↑ %d more", start)))
	}
	for i := start; i < end; i++ {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		s := m.suggestions[i]
		if i == m.suggestIdx {
			b.WriteString(lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("236")).Render(s))
		} else {
			b.WriteString(m.u.dim(s))
		}
	}
	if end < n {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(m.u.dim(fmt.Sprintf("↓ %d more", n-end)))
	}
	return b.String()
}

// tuiInput is a minimal multi-line text buffer with a cursor, used for the
// prompt. Enter submits (handled by the model); Ctrl+J or Alt+Enter inserts a
// newline.
type tuiInput struct {
	lines [][]rune
	row   int
	col   int // rune index into lines[row]
}

func (in *tuiInput) Value() string {
	parts := make([]string, len(in.lines))
	for i, l := range in.lines {
		parts[i] = string(l)
	}
	return strings.Join(parts, "\n")
}

func (in *tuiInput) SetValue(s string) {
	raw := strings.Split(s, "\n")
	in.lines = make([][]rune, len(raw))
	for i, l := range raw {
		in.lines[i] = []rune(l)
	}
	in.row, in.col = 0, 0
}

func (in *tuiInput) Clear() {
	in.lines = [][]rune{{}}
	in.row, in.col = 0, 0
}

func (in *tuiInput) InsertRune(r rune) {
	if r == '\n' {
		line := in.lines[in.row]
		tail := append([]rune(nil), line[in.col:]...)
		in.lines[in.row] = line[:in.col]
		next := append([][]rune{}, in.lines[in.row+1:]...)
		in.lines = append(in.lines[:in.row+1], append([][]rune{tail}, next...)...)
		in.row++
		in.col = 0
		return
	}
	line := in.lines[in.row]
	newLine := make([]rune, 0, len(line)+1)
	newLine = append(newLine, line[:in.col]...)
	newLine = append(newLine, r)
	newLine = append(newLine, line[in.col:]...)
	in.lines[in.row] = newLine
	in.col++
}

func (in *tuiInput) Backspace() {
	line := in.lines[in.row]
	if in.col > 0 {
		in.lines[in.row] = append(line[:in.col-1], line[in.col:]...)
		in.col--
		return
	}
	if in.row > 0 {
		prev := in.lines[in.row-1]
		in.col = len(prev)
		in.lines[in.row-1] = append(prev, line...)
		in.lines = append(in.lines[:in.row], in.lines[in.row+1:]...)
		in.row--
	}
}

func (in *tuiInput) Delete() {
	line := in.lines[in.row]
	if in.col < len(line) {
		in.lines[in.row] = append(line[:in.col], line[in.col+1:]...)
		return
	}
	if in.row < len(in.lines)-1 {
		in.lines[in.row] = append(line, in.lines[in.row+1]...)
		in.lines = append(in.lines[:in.row+1], in.lines[in.row+2:]...)
	}
}

func (in *tuiInput) MoveLeft() {
	if in.col > 0 {
		in.col--
	} else if in.row > 0 {
		in.row--
		in.col = len(in.lines[in.row])
	}
}

func (in *tuiInput) MoveRight() {
	if in.col < len(in.lines[in.row]) {
		in.col++
	} else if in.row < len(in.lines)-1 {
		in.row++
		in.col = 0
	}
}

func (in *tuiInput) MoveUp() {
	if in.row > 0 {
		in.row--
		if in.col > len(in.lines[in.row]) {
			in.col = len(in.lines[in.row])
		}
	}
}

func (in *tuiInput) MoveDown() {
	if in.row < len(in.lines)-1 {
		in.row++
		if in.col > len(in.lines[in.row]) {
			in.col = len(in.lines[in.row])
		}
	}
}

func (in *tuiInput) Home() { in.col = 0 }

func (in *tuiInput) End() { in.col = len(in.lines[in.row]) }

// inputPrompt is the prompt glyph shown in front of the input text, styled in
// the mint accent (Guac).
const inputPrompt = "::: "
const inputPromptAnsi = "\x1b[38;2;18;199;143m"

// render draws the input into a fixed-height (maxInputLines) textarea: the
// text wraps at width-promptW columns, short lines are padded so the input
// spans the terminal, and the cursor is marked with a block character. The
// prompt ("::: ") sits at the left of the first visible row and the window
// follows the cursor, so earlier content scrolls out of view.
func (in *tuiInput) render(width int) string {
	promptW := len(inputPrompt)
	boxW := width - promptW
	if boxW < 1 {
		boxW = 1
	}
	var rows [][]rune
	cursorRow, cursorCol := -1, -1
	for i, line := range in.lines {
		vis := wrapWords(line, boxW)
		if i == in.row {
			// Wrap the prefix to find the cursor's visual row/col; a word-wrap
			// break can land the cursor earlier than a pure char/boxW split.
			prefix := wrapWords(line[:in.col], boxW)
			cursorRow = len(rows) + len(prefix) - 1
			cursorCol = len(prefix[len(prefix)-1])
		}
		rows = append(rows, vis...)
	}
	if len(rows) == 0 {
		rows = [][]rune{{}}
	}
	start := len(rows) - maxInputLines
	if start < 0 {
		start = 0
	}
	if cursorRow >= 0 {
		if cursorRow < start {
			start = cursorRow
		}
		if cursorRow >= start+maxInputLines {
			start = cursorRow - maxInputLines + 1
		}
		if start < 0 {
			start = 0
		}
	}
	var out []string
	for i := start; i < len(rows) && i < start+maxInputLines; i++ {
		s := padRunes(rows[i], boxW)
		if i == cursorRow {
			overChar := cursorCol < len(rows[i])
			s = applyCursor(s, cursorCol, overChar)
		}
		if i == start {
			s = inputPromptAnsi + inputPrompt + ansiReset + s
		} else {
			s = strings.Repeat(" ", promptW) + s
		}
		out = append(out, s)
	}
	for len(out) < maxInputLines {
		out = append(out, strings.Repeat(" ", width))
	}
	return strings.Join(out, "\n")
}

// applyCursor renders the cell under the cursor: the character in reverse
// video (the opposite of the cursor colour) when it is over text, or a solid
// block when the cursor sits past the text. A cursor at the very end of a row
// that exactly fills the box is drawn over the last cell.
func applyCursor(s string, col int, overChar bool) string {
	r := []rune(s)
	if len(r) == 0 {
		return "█"
	}
	if col >= len(r) {
		col = len(r) - 1
		overChar = false
	}
	if overChar {
		return string(r[:col]) + "\x1b[7m" + string(r[col]) + ansiReset + string(r[col+1:])
	}
	return string(r[:col]) + "█" + string(r[col+1:])
}

// wrapWords breaks a logical line into visual rows of at most width runes,
// preferring to break at whitespace so words are never split. A word longer
// than width hard-breaks.
func wrapWords(s []rune, width int) [][]rune {
	if len(s) == 0 {
		return [][]rune{{}}
	}
	var out [][]rune
	for len(s) > width {
		cut := width
		for i := width; i > 0; i-- {
			if s[i-1] == ' ' {
				cut = i
				break
			}
		}
		if cut == width { // single word longer than width: hard break
			out = append(out, s[:width])
			s = s[width:]
			continue
		}
		out = append(out, s[:cut])
		s = s[cut:]
		for len(s) > 0 && s[0] == ' ' {
			s = s[1:]
		}
	}
	if len(s) > 0 || len(out) == 0 {
		out = append(out, s)
	}
	return out
}

// padRunes right-pads a visual row to width columns.
func padRunes(r []rune, width int) string {
	s := string(r)
	if n := width - len(r); n > 0 {
		s += strings.Repeat(" ", n)
	}
	return s
}

// runTUI runs the interactive bubbletea session. It must only be called when
// stdin and stdout are terminals.
func runTUI(m *tuiModel) error {
	prog := tea.NewProgram(m)
	if _, err := prog.Run(); err != nil {
		return err
	}
	return m.err
}
