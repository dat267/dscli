package cmd

import (
	"context"
	"fmt"
	"os"
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
// Output routing: the underlying ChatCmd's answer/preview/confirm hooks are
// pointed at this model, so the model's reply, tool notes, plan previews and
// write-confirmations render inside the TUI instead of stdout/stderr. A turn
// runs in a goroutine and reports deltas over a channel that the model pumps.
type tuiModel struct {
	chat         *ChatCmd
	client       *deepseek.Client
	model        string
	fileTools    bool
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

	busy     bool
	cancel   context.CancelFunc
	streamCh chan tea.Msg

	suggestions []string
	suggestIdx  int

	awaitingConfirm bool
	confirmPrompt   string
	confirmResp     chan bool

	turns int
	quit  bool
	err   error
}

// stream messages sent by the turn goroutine into the model.
type streamDelta struct{ text string }
type streamNote struct{ text string }
type streamConfirm struct {
	prompt string
	resp   chan bool
}
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
		fileTools:    chat.FileTools,
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
	m.status = m.u.dim(fmt.Sprintf(
		"DeepSeek · model %s · thinking %s · search %s · tools %s · %s",
		m.model, onoff(m.thinking), onoff(m.search), onoff(m.fileTools), mode,
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
	ch.confirm = func(q string) bool {
		resp := make(chan bool)
		m.streamCh <- streamConfirm{prompt: q, resp: resp}
		return <-resp
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

// doTurn runs one user message, mirroring the scanner REPL: oneTurn when file
// tools are off, the tool loop otherwise.
func (m *tuiModel) doTurn(ctx context.Context, ch *ChatCmd, sid, prompt string, sources *[]deepseek.Source) (string, error) {
	if !m.fileTools {
		return ch.oneTurn(ctx, m.client, sid, prompt, m.model, ch.answerWriter(), sources)
	}
	return ch.turn(ctx, m.client, sid, prompt, m.model, true, func(s string) {
		m.streamCh <- streamNote{s}
	}, sources)
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.MouseWheelMsg:
		switch msg.Mouse().Button {
		case tea.MouseWheelUp:
			m.scrollUp()
		case tea.MouseWheelDown:
			m.scrollDown()
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
		}
		return m.handleKey(msg)
	case streamDelta:
		m.appendText(msg.text)
		return m, pump(m.streamCh)
	case streamNote:
		m.appendLine(m.u.dim(msg.text))
		return m, pump(m.streamCh)
	case streamConfirm:
		m.awaitingConfirm = true
		m.confirmPrompt = msg.prompt
		m.confirmResp = msg.resp
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
	if m.awaitingConfirm {
		switch msg.String() {
		case "y", "Y", "yes":
			m.respondConfirm(true)
		case "enter", "n", "N", "no", "esc", "ctrl+c":
			m.respondConfirm(false)
		}
		return m, nil
	}
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
		m.cycleSuggestion()
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

func (m *tuiModel) respondConfirm(yes bool) {
	if m.confirmResp != nil {
		m.confirmResp <- yes
		m.confirmResp = nil
	}
	m.awaitingConfirm = false
}

func (m *tuiModel) quitCleanup() {
	m.respondConfirm(false)
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
	if strings.HasPrefix(line, "/") {
		if knownCommand(line) {
			m.input.Clear()
			m.updateSuggestions()
			return m.handleCommand(line)
		}
		if len(m.suggestions) > 0 {
			m.input.SetValue(m.suggestions[m.suggestIdx])
			m.suggestions = nil
			m.suggestIdx = 0
			return m, nil
		}
	}
	if line == "" {
		return m, nil
	}
	m.input.Clear()
	m.updateSuggestions()
	m.addHistory(line)
	m.appendLine(renderUserLine(line))
	m.appendLine("") // breathing room before the reply streams
	return m, m.startTurn(line)
}

// renderUserLine styles a submitted user message with a foreground colour so
// it stands apart from the plain assistant reply (no "you>" prefix — the
// input box already shows what was typed).
func renderUserLine(line string) string {
	return "\x1b[38;5;81m" + line + ansiReset
}

// knownCommand reports whether line is a complete slash command (with or
// without arguments), so Enter runs it instead of completing a suggestion.
func knownCommand(line string) bool {
	cmd, _, _ := strings.Cut(line, " ")
	switch cmd {
	case "/exit", "/quit", "/new", "/help", "/model", "/thinking", "/search", "/tools", "/files", "/clear":
		return true
	}
	return false
}

// addHistory records a submitted prompt for Up/Down recall.
func (m *tuiModel) addHistory(p string) {
	m.history = append(m.history, p)
	m.histIdx = len(m.history)
}

func (m *tuiModel) historyPrev() {
	if len(m.history) == 0 {
		return
	}
	if m.histIdx > 0 {
		m.histIdx--
		m.input.SetValue(m.history[m.histIdx])
	}
}

func (m *tuiModel) historyNext() {
	if m.histIdx < len(m.history) {
		m.histIdx++
		if m.histIdx < len(m.history) {
			m.input.SetValue(m.history[m.histIdx])
		} else {
			m.input.Clear()
		}
	}
}

// updateSuggestions recomputes the slash-command menu from the input's last
// line.
func (m *tuiModel) updateSuggestions() {
	lines := strings.Split(m.input.Value(), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	m.suggestions = suggestCommands(last)
	m.suggestIdx = 0
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
	if !strings.HasPrefix(token, "/") {
		return nil
	}
	var out []string
	for _, c := range []string{"/exit", "/quit", "/new", "/help", "/model", "/thinking", "/search", "/tools", "/files", "/clear"} {
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
		m.appendLine(m.u.dim("commands: /exit /quit /new /model /thinking /search /tools /files /clear /help"))
		m.appendLine(m.u.dim("  /clear [--delete]  forget the persisted default session (--delete removes it server-side)"))
		m.appendLine(m.u.dim("enter submits · ctrl+j / alt+enter newline · up/down history · tab completes /commands"))
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
	case "/tools", "/files":
		cmd := "/tools"
		if strings.HasPrefix(line, "/files") {
			cmd = "/files"
		}
		m.fileTools = toggleState(line, cmd, m.fileTools)
		m.chat.FileTools = m.fileTools
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
// and the input box (plus any confirm/suggestion rows).
func (m *tuiModel) outputRows() int {
	inputH := m.inputHeight() + 2 // box border
	extra := 0
	if m.awaitingConfirm {
		extra++
	}
	if len(m.suggestions) > 0 {
		extra++
	}
	rows := m.height - 1 - inputH - extra // 1 = status line
	if rows < 0 {
		return 0
	}
	return rows
}

func (m *tuiModel) scrollUp() {
	m.autoScroll = false
	avail := m.outputRows()
	if avail < 1 {
		avail = 1
	}
	if m.viewTop > 0 {
		m.viewTop -= avail
	}
	if m.viewTop < 0 {
		m.viewTop = 0
	}
}

func (m *tuiModel) scrollDown() {
	total := m.scrollLines()
	avail := m.outputRows()
	if avail < 1 {
		avail = 1
	}
	if m.viewTop+avail < total {
		m.viewTop += avail
		if m.viewTop+avail >= total {
			m.autoScroll = true
		}
	} else {
		m.autoScroll = true
	}
}

func (m *tuiModel) scrollLines() int {
	if m.scroll == "" {
		return 0
	}
	return len(strings.Split(m.scroll, "\n"))
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
	lines := strings.Split(m.scroll, "\n")
	avail := m.outputRows()
	rendered := 0
	for i := m.viewTop; i < len(lines) && rendered < avail; i++ {
		b.WriteString(lines[i])
		b.WriteString("\n")
		rendered++
	}
	for ; rendered < avail; rendered++ {
		b.WriteString("\n")
	}

	// 2. Write-confirm prompt and slash-command menu sit just above the input.
	if m.awaitingConfirm {
		b.WriteString(m.u.bold("confirm: " + m.confirmPrompt + " [y/N] "))
		b.WriteString("\n")
	}
	if len(m.suggestions) > 0 {
		b.WriteString(m.renderSuggestions())
		b.WriteString("\n")
	}

	// 3. Input box at the bottom.
	b.WriteString(lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Render(m.input.render(max(0, m.width))))
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
const maxInputLines = 3

func (m *tuiModel) renderSuggestions() string {
	var parts []string
	for i, s := range m.suggestions {
		if i == m.suggestIdx {
			parts = append(parts, lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("236")).Render(s))
		} else {
			parts = append(parts, s)
		}
	}
	return m.u.dim(strings.Join(parts, "  "))
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

// render draws the input into a fixed-height (maxInputLines) box: long lines
// wrap at width-2 columns (the box border), short lines are padded so the box
// spans the terminal, and the cursor is marked with a block character. The
// window follows the cursor, so earlier content scrolls out of view.
func (in *tuiInput) render(width int) string {
	boxW := width - 2 // box border sides
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
		out = append(out, s)
	}
	for len(out) < maxInputLines {
		out = append(out, strings.Repeat(" ", boxW))
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
