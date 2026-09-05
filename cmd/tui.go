package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"

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
	draft      string       // input stashed while recalling history
	loaded     []loadedFile // <file>/<dir> blocks stacked by /file, sent with the next message

	// reply accumulates the streamed assistant text of the current turn: it
	// is rendered live as markdown while the turn runs (activeTurnRows),
	// committed to the scrollback on streamDone, and saved raw to the session
	// transcript. It is written only from Update (the streamDelta case), so
	// no cross-goroutine handoff is needed.
	reply strings.Builder
	// lastPartial keeps the text of the most recent filtered reply so /resume
	// can continue it; cleared by later successful turns and /new.
	lastPartial string

	busy     bool
	cancel   context.CancelFunc
	streamCh chan tea.Msg
	// spin is the current frame of the working indicator shown while a turn
	// runs; spinnerTickMsg advances it.
	spin int
	// interrupted is true from the moment Ctrl+C cancels an in-flight turn
	// until that turn's streamDone arrives; it selects the "interrupted" note
	// over the error line.
	interrupted bool

	suggestions []string
	suggestIdx  int

	turns int
	quit  bool
	err   error
}

// stream messages sent by the turn goroutine into the model.
type streamDelta struct{ text string }
type streamNote struct{ text string }
type streamDone struct {
	err      error
	convID   string
	sources  []deepseek.Source
	filtered bool
}

// spinnerTickMsg advances the pulsing working indicator while a turn runs;
// stream deltas alone cannot animate it (the model may think for seconds
// between deltas).
type spinnerTickMsg struct{}

// spinnerFrames is pi's braille pulse.
var spinnerFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerTick schedules the next working-indicator frame.
func spinnerTick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return spinnerTickMsg{} })
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
	conv := ""
	if m.conversation != "" {
		sess, _ := splitConversation(m.conversation)
		if sess != "" {
			if len(sess) > 8 {
				conv = sess[:8] + "…"
			} else {
				conv = sess
			}
		}
	}
	m.status = m.u.muted(fmt.Sprintf(
		"DeepSeek · model %s · %s · turn %d",
		m.model, mode, m.turns,
	))
	if conv != "" {
		m.status = m.u.muted(fmt.Sprintf(
			"DeepSeek · model %s · %s · turn %d · %s",
			m.model, mode, m.turns, conv,
		))
	}
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
	m.interrupted = false
	m.reply.Reset()
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
		var filtered bool
		run := func(sid string) error {
			cid, isFiltered, e := m.doTurn(ctx, ch, sid, prompt, &sources)
			if e == nil {
				convID = cid
				filtered = isFiltered
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
		m.streamCh <- streamDone{err: err, convID: convID, sources: sources, filtered: filtered}
	}()
	return tea.Batch(pump(m.streamCh), spinnerTick())
}

// doTurn runs one user message, mirroring the scanner REPL.
func (m *tuiModel) doTurn(ctx context.Context, ch *ChatCmd, sid, prompt string, sources *[]deepseek.Source) (string, bool, error) {
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
		case "ctrl+l":
			// Clear the scrollback pane (display only — the conversation and
			// its server-side thread are untouched).
			m.scroll = ""
			m.viewTop = 0
			m.autoScroll = true
			return m, nil
		}
		return m.handleKey(msg)
	case streamDelta:
		// The turn's reply accumulates in m.reply and is rendered live as
		// markdown below the committed scrollback (activeTurnRows); it is
		// committed to the scroll on streamDone.
		m.reply.WriteString(msg.text)
		return m, pump(m.streamCh)
	case spinnerTickMsg:
		if !m.busy {
			return m, nil
		}
		m.spin = (m.spin + 1) % len(spinnerFrames)
		return m, spinnerTick()
	case streamNote:
		// System notes (e.g. the filtered reply note) render dimmed grey so
		// they read as side notes rather than the assistant's reply.
		m.appendLine(m.u.note(msg.text))
		return m, pump(m.streamCh)
	case streamDone:
		m.busy = false
		if msg.err != nil {
			// A cancelled turn (Ctrl+C) is not an error; keep the chat usable.
			if m.interrupted {
				m.appendLine(m.u.note("interrupted"))
			} else {
				m.appendLine(m.u.red("error: " + msg.err.Error()))
			}
			m.appendLine("")
		} else {
			m.conversation = msg.convID
			persistConversation(m.cfgPath, m.noPersist, msg.convID)
			if transcriptsEnabled(m.cfgPath, m.noPersist, m.chat.NoTranscript) {
				appendTranscript(m.cfgPath, msg.convID, "assistant", m.reply.String())
			}
			if msg.filtered {
				// The filter cut the reply off; whatever streamed before it is
				// the seed for /resume (a fully-filtered reply offers nothing).
				if m.reply.Len() > 0 {
					m.lastPartial = m.reply.String()
					m.appendLine(m.u.note("hint: /resume continues from the partial reply above"))
				} else {
					m.lastPartial = ""
				}
			} else {
				m.lastPartial = ""
			}
			m.commitReply()
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
		// While a turn streams, Ctrl+C interrupts the completion but keeps the
		// chat open; Ctrl+C when idle quits.
		if msg.String() == "ctrl+c" {
			m.interruptTurn()
			return m, nil
		}
		return m, nil
	}

	switch msg.String() {
	case "ctrl+c":
		m.quitCleanup()
		return m, tea.Quit
	case "ctrl+a":
		m.input.Home()
		return m, nil
	case "ctrl+e":
		m.input.End()
		return m, nil
	case "ctrl+left", "alt+left":
		m.input.WordLeft()
		return m, nil
	case "ctrl+right", "alt+right":
		m.input.WordRight()
		return m, nil
	case "alt+backspace":
		m.input.WordBackspace()
		m.updateSuggestions()
		return m, nil
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
		if len(m.suggestions) > 0 {
			// First Esc dismisses the completion menu without losing the
			// typed message, so you can break out of a folder descent and
			// submit; a second Esc clears the input.
			m.suggestions = nil
			m.suggestIdx = 0
		} else {
			m.input.Clear()
		}
		return m, nil
	case "up":
		if len(m.suggestions) > 0 {
			m.suggestPrev()
			return m, nil
		}
		// Move the cursor up one visual row (through wrapped rows); at the
		// very top row, recall history instead.
		if m.input.moveUpVisual(m.width) {
			return m, nil
		}
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
		// Move the cursor down one visual row; at the bottom row, recall the
		// next history entry instead.
		if m.input.moveDownVisual(m.width) {
			return m, nil
		}
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

// interruptTurn cancels the in-flight completion; the turn's streamDone then
// reports "interrupted" instead of an error, and the chat stays open.
func (m *tuiModel) interruptTurn() {
	if m.cancel != nil {
		m.cancel()
	}
	m.interrupted = true
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
	if transcriptsEnabled(m.cfgPath, m.noPersist, m.chat.NoTranscript) {
		appendTranscript(m.cfgPath, m.conversation, "user", line)
	}
	prompt := line
	if len(m.loaded) > 0 {
		// Files loaded via /file are prepended to this message, then dropped.
		names := make([]string, len(m.loaded))
		blocks := make([]string, len(m.loaded))
		for i, f := range m.loaded {
			names[i] = f.path
			blocks[i] = f.block
		}
		m.appendSystem("attached " + strings.Join(names, ", "))
		prompt = strings.Join(blocks, "") + "\n" + line
		m.loaded = nil
	}
	return m, m.startTurn(prompt)
}

// appendSystem renders a system note (file loads, attachments) in the dimmed
// grey note style, distinct from the user and assistant text.
func (m *tuiModel) appendSystem(text string) {
	m.appendLine(m.u.note(text))
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
		m.scroll = m.u.note("note: could not load conversation history: "+err.Error()) + "\n"
		return
	}
	m.scroll = renderHistory(m.u, hist, m.width)
	m.trimScroll()
}

// renderHistory builds the scrollback text for a resumed conversation: user
// lines styled violet, assistant replies plain, one blank line between turns,
// in message-id order.
// renderHistory builds the scrollback text for a resumed conversation: user
// lines styled violet, assistant replies rendered as markdown, one blank
// line between turns, in message-id order. width is the terminal width (80
// as a fallback when no resize has arrived yet); committed rows are
// re-wrapped ANSI-aware by the scroll renderer after a resize.
func renderHistory(u ui, hist []deepseek.HistoryMessage, width int) string {
	if width < 1 {
		width = 80
	}
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
			b.WriteString("\n")
			continue
		}
		for _, row := range renderMarkdown(u, text, width) {
			b.WriteString(row)
			b.WriteString("\n")
		}
	}
	// End on a blank line, so the first message sent after a resume keeps
	// the breathing room every later turn gets from streamDone.
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	return b.String()
}

// knownCommand reports whether line is a complete slash command (with or
// without arguments), so Enter runs it instead of completing a suggestion.
func knownCommand(line string) bool {
	cmd, _, _ := strings.Cut(line, " ")
	switch cmd {
	case "/exit", "/quit", "/new", "/help", "/model", "/thinking", "/search", "/clear", "/file", "/resume", "/session", "/sessions", "/copy":
		return true
	}
	return false
}

// addHistory records a submitted prompt for Up/Down recall. Consecutive
// duplicates are coalesced, so re-sending the same prompt does not pile up
// identical history entries.
func (m *tuiModel) addHistory(p string) {
	if len(m.history) == 0 || m.history[len(m.history)-1] != p {
		m.history = append(m.history, p)
	}
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

// updateSuggestions recomputes the slash-command completion menu from the
// input's last line.
func (m *tuiModel) updateSuggestions() {
	lines := strings.Split(m.input.Value(), "\n")
	m.suggestions = suggestCommands(lines[len(lines)-1])
	m.suggestIdx = 0
}

// completeSuggestion fills in the highlighted suggestion and clears the menu.
// Commands complete to "<command> " (cursor after the space) when they accept
// an argument, or just "<command>" when they take none.
func (m *tuiModel) completeSuggestion() {
	if len(m.suggestions) == 0 {
		return
	}
	s := m.suggestions[m.suggestIdx]
	if commandTakesArg(s) {
		s += " "
	}
	lines := strings.Split(m.input.Value(), "\n")
	lines[len(lines)-1] = s
	m.input.SetValue(strings.Join(lines, "\n"))
	m.input.End()
	m.suggestions = nil
	m.suggestIdx = 0
}

// noArgCommands are slash commands that take no parameters, so completing
// them leaves no trailing space.
var noArgCommands = map[string]bool{
	"/exit": true, "/quit": true, "/new": true, "/help": true,
	"/sessions": true,
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
	for _, c := range []string{"/exit", "/quit", "/new", "/help", "/model", "/thinking", "/search", "/clear", "/file", "/resume", "/session", "/sessions", "/copy"} {
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
		m.appendLine(m.u.note("new conversation"))
		m.appendLine("")
		m.refreshStatus()
		return m, nil
	case "/sessions":
		m.handleSessionsCommand()
		return m, nil
	case "/session":
		m.handleSessionCommand(arg)
		return m, nil
	case "/copy":
		m.handleCopyCommand()
		return m, nil
	case "/resume":
		// Continue a reply the content filter cut off: the partial text is
		// sent back as context with a continue instruction. Nothing is
		// bypassed — the filter still applies to the new generation.
		if m.lastPartial == "" {
			m.appendLine(m.u.red("nothing to resume: no filtered partial (or the last reply was accepted)"))
			return m, nil
		}
		shown := strings.TrimSpace(line)
		m.appendLine(renderUserLine(shown))
		m.appendLine("") // breathing room
		if transcriptsEnabled(m.cfgPath, m.noPersist, m.chat.NoTranscript) {
			appendTranscript(m.cfgPath, m.conversation, "user", shown)
		}
		m.loaded = nil
		return m, m.startTurn(resumePrompt(m.lastPartial, arg))
	case "/help":
		m.appendLine(m.u.note("commands: /exit /quit /new /model /thinking /search /clear /file /resume /help"))
		m.appendLine(m.u.note("  /clear [--delete]  forget the persisted default session (--delete removes it server-side)"))
		m.appendLine(m.u.note("  /file <path>       load a file/directory into the message (repeat to stack files)"))
		m.appendLine(m.u.note("  /resume [hint]     continue a reply the filter cut off, from its partial text"))
		m.appendLine(m.u.note("  /session [id]     show the current conversation; select a saved session to resume"))
		m.appendLine(m.u.note("  /sessions          list sessions with saved texts"))
		m.appendLine(m.u.note("  /copy              copy the chat text to the system clipboard"))
		m.appendLine(m.u.note("enter submits · ctrl+j/alt+enter newline · up/down cursor (history at first/last row) · ctrl+left/right word · alt+backspace deletes a word · ctrl+a/e line start/end · tab completes /commands"))
		m.appendLine(m.u.note("pgup/pgdn/home/end scroll · ctrl+l clears the pane · ctrl+c interrupts a reply (tapped again when idle, quits) · esc dismisses the menu or clears the input"))
		m.appendLine("")
		return m, nil
	case "/model":
		if arg == "" {
			m.appendLine(m.u.note("model: " + m.model + " (fixed per thread; /model <default|expert> starts a new conversation)"))
			m.appendLine("")
			return m, nil
		}
		if arg != "default" && arg != "expert" {
			m.appendLine(m.u.red(fmt.Sprintf("unknown model %q (want default or expert)", arg)))
			return m, nil
		}
		m.model = arg
		m.chat.Model = arg
		m.newSession()
		m.appendLine(m.u.note("new conversation"))
		m.appendLine("")
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
	case "/file":
		m.handleFileCommand(arg)
		return m, nil
	}
	m.appendLine(m.u.red("unknown command (/help for commands)"))
	return m, nil
}

// loadedFile is one file stacked by /file: the path as the user gave it, and
// the <file>/<dir> block that carries its contents into the next message.
type loadedFile struct {
	path  string
	block string
}

// handleFileCommand implements `/file <path>`: load the file's (or
// directory's) contents into a buffer that is prepended to the next submitted
// message. Repeating the command stacks files one by one.
func (m *tuiModel) handleFileCommand(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		m.appendLine(m.u.red("usage: /file <path>"))
		return
	}
	block := m.chat.mentionBlock(path)
	if block == "" {
		m.appendLine(m.u.red("could not load " + path))
		return
	}
	m.loaded = append(m.loaded, loadedFile{path: path, block: block})
	m.appendSystem(fmt.Sprintf("loaded %s (%d file(s) in the next message)", path, len(m.loaded)))
}

// handleClearCommand implements `/clear [--delete]`: forget the persisted
// default session and start a fresh conversation. `--delete` also removes the
// thread server-side before forgetting it.
func (m *tuiModel) handleClearCommand(arg string) (tea.Model, tea.Cmd) {
	switch arg {
	case "":
		m.appendLine(m.u.note("clearing persisted session"))
		m.appendLine("")
	case "--delete":
		sess, _ := splitConversation(m.conversation)
		if err := m.client.DeleteSessions(context.Background(), []string{sess}); err != nil {
			m.appendLine(m.u.red("error: delete server-side: " + err.Error()))
		} else {
			m.appendLine(m.u.note("deleted session " + sess + " server-side"))
			m.appendLine("")
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
	m.appendLine(m.u.note("new conversation"))
	m.appendLine("")
	m.refreshStatus()
	return m, nil
}

// handleSessionsCommand implements `/sessions`: list the locally saved
// sessions (most recent first, default marked) in the chat pane.
func (m *tuiModel) handleSessionsCommand() {
	rows, err := localSessionRows(m.cfgPath)
	if err != nil {
		m.appendLine(m.u.red("error: " + err.Error()))
		return
	}
	if len(rows) == 0 {
		m.appendLine(m.u.note("no local sessions (nothing saved yet)"))
		return
	}
	m.appendLine(m.u.note("local sessions (most recent first; the default is resumed on launch):"))
	for _, r := range rows {
		m.appendLine(m.u.note(sessionRowText(r)))
	}
	m.appendLine("")
}

// handleSessionCommand implements `/session [id]`: with an id it selects a
// saved session — the live chat switches to it and it becomes the persisted
// default (resumed from its root); without an id it shows the current
// conversation.
func (m *tuiModel) handleSessionCommand(arg string) {
	if arg == "" {
		if saved := loadSavedSession(m.cfgPath); saved != "" {
			m.appendLine(m.u.note("conversation: " + saved))
		} else {
			m.appendLine(m.u.note("no persisted session"))
		}
		m.appendLine("")
		return
	}
	bare, _ := splitConversation(arg)
	if bare == "" {
		m.appendLine(m.u.red("give a session id (see /sessions)"))
		return
	}
	if err := saveSession(m.cfgPath, bare); err != nil {
		m.appendLine(m.u.red("error: " + err.Error()))
		return
	}
	// The live chat now continues the selected thread, from its root. The
	// pane is cleared so it stops showing the previous thread, then the
	// switch is announced.
	m.conversation = bare
	m.trusted = false
	m.firstTurn = false
	m.lastPartial = ""
	m.scroll = ""
	m.viewTop = 0
	m.autoScroll = true
	if msgs, ok := transcriptCount(m.cfgPath, bare); ok {
		m.appendLine(m.u.note(fmt.Sprintf("switched to session %s (%d saved messages; resumes from its root)", bare, msgs)))
	} else {
		m.appendLine(m.u.note("switched to session " + bare + " (no local transcript yet)"))
	}
	m.appendLine("")
	m.refreshStatus()
}

// handleCopyCommand implements `/copy`: copies the visible chat text
// (stripped of ANSI styling) to the system clipboard via the platform-native
// clipboard tool (xclip, wl-copy, pbcopy, clip.exe, or a plain tee fallback).
func (m *tuiModel) handleCopyCommand() {
	plain := stripANSI(m.scroll)
	if plain == "" {
		m.appendLine(m.u.note("nothing to copy (the chat is empty)"))
		return
	}
	tool := clipboardTool()
	if len(tool) == 0 {
		m.appendLine(m.u.red("no clipboard tool found; install xclip, wl-copy, pbcopy, or clip.exe"))
		return
	}
	cmd := exec.Command(tool[0], tool[1:]...)
	cmd.Stdin = strings.NewReader(plain)
	if err := cmd.Run(); err != nil {
		m.appendLine(m.u.red("clipboard copy failed: " + err.Error()))
		return
	}
	m.appendLine(m.u.note(fmt.Sprintf("copied %d bytes of chat text to clipboard", len(plain))))
}

// clipboardTool is a var so tests can pin the platform detection instead of
// depending on which clipboard tools happen to be installed on the host.
var clipboardTool = findClipboardTool

// findClipboardTool returns the platform-native clipboard command, or "".
func findClipboardTool() []string {
	// $WAYLAND_DISPLAY checked first so wl-copy is preferred over xclip
	// when both are present on a Wayland session.
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		if _, err := exec.LookPath("wl-copy"); err == nil {
			return []string{"wl-copy"}
		}
	}
	for _, c := range []string{"xclip", "xsel"} {
		if _, err := exec.LookPath(c); err == nil {
			if c == "xclip" {
				return []string{"xclip", "-selection", "clipboard"}
			}
			return []string{"xsel", "--clipboard", "--input"}
		}
	}
	if _, err := exec.LookPath("pbcopy"); err == nil {
		return []string{"pbcopy"}
	}
	if _, err := exec.LookPath("clip.exe"); err == nil {
		return []string{"clip.exe"}
	}
	return nil
}

// newSession starts a fresh thread: persisted (saved as the new default) or,
// with --no-persist, tracked for deletion on quit. The thread changes mean any
// filtered partial belongs to the old conversation, so it is dropped.
func (m *tuiModel) newSession() {
	m.lastPartial = ""
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
// and the separator+input (plus any suggestion rows). Streaming does not
// change the layout: the working indicator lives on the status line.
func (m *tuiModel) outputRows() int {
	// +1 for the separator row between scrollback and input
	rows := m.height - 1 - 1 - m.inputHeight() - m.suggestionRows() // 1 = status line, 1 = separator
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
	return len(m.wrappedRows()) + len(m.activeTurnRows())
}

// activeTurnRows renders the streaming turn's accumulated reply as markdown
// — re-rendered every frame so a partial reply (an unclosed code fence, a
// half-written list) always displays cleanly. On streamDone the rows are
// committed to the scrollback (commitReply).
func (m *tuiModel) activeTurnRows() []string {
	if !m.busy || m.reply.Len() == 0 || m.width < 1 {
		return nil
	}
	rows := renderMarkdown(m.u, m.reply.String(), m.width)
	// streamDone commits these rows plus a trailing blank separator; render
	// the blank live too so the visible text does not move when the turn
	// finishes.
	return append(rows, "")
}

// commitReply moves the finished turn's reply from the live markdown view
// into the scrollback, rendered to rows at the current width.
func (m *tuiModel) commitReply() {
	if m.reply.Len() == 0 {
		return
	}
	if m.width < 1 {
		m.appendText(m.reply.String())
		return
	}
	for _, row := range renderMarkdown(m.u, m.reply.String(), m.width) {
		m.appendLine(row)
	}
}

// wrappedRows returns the scrollback split into rows that fit the pane width,
// word-wrapping long lines (re-applying any leading ANSI style).
func (m *tuiModel) wrappedRows() []string {
	if m.scroll == "" {
		return nil
	}
	var rows []string
	lines := strings.Split(m.scroll, "\n")
	if strings.HasSuffix(m.scroll, "\n") && len(lines) > 0 {
		lines = lines[:len(lines)-1] // the trailing newline's split artifact, not a row
	}
	for _, line := range lines {
		if textWidth(stripANSI(line)) <= m.width {
			// Already fits: markdown rows are rendered to width at commit.
			// Re-wrapping would miscount their mid-line ANSI codes as visible
			// columns and re-split them, shifting the committed text.
			rows = append(rows, line)
			continue
		}
		// Overflowing (e.g. after a resize): wrap ANSI-aware so mid-line
		// styles survive and the text still spans the full pane width.
		rows = append(rows, wrapStyled(line, m.width, "")...)
	}
	return rows
}

// splitStyled splits a line into its leading ANSI styles (possibly stacked,
// e.g. dim + a colour, as produced by ui.note) and the visible text, stripping
// one trailing reset.
func splitStyled(line string) (prefix, text string) {
	for strings.HasPrefix(line, "\x1b[") {
		if end := strings.IndexByte(line, 'm'); end > 0 {
			prefix += line[:end+1]
			line = line[end+1:]
		} else {
			break
		}
	}
	return prefix, strings.TrimSuffix(line, ansiReset)
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
	// pad a short conversation so the input box is pinned to the bottom. The
	// rows are the committed scrollback plus the streaming turn's live
	// markdown view.
	rows := m.wrappedRows()
	rows = append(rows, m.activeTurnRows()...)
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

	// 2. Separator: a dimmed line between the scrollback and the input area.
	// While a turn runs the rule doubles as the working indicator — pi-style
	// "── ⠋ Working ───" — so streaming never changes the pane layout.
	if m.width > 0 {
		if m.busy {
			head := "── " + spinnerFrames[m.spin] + " Working "
			b.WriteString(m.u.dim("──") + " " + m.u.accent(spinnerFrames[m.spin]) + " " +
				m.u.muted("Working") + " " +
				m.u.dim(strings.Repeat("─", max(0, m.width-textWidth(head)))))
		} else {
			b.WriteString(m.u.dim(strings.Repeat("─", m.width)))
		}
		b.WriteString("\n")
	}

	// 3. Slash-command menu sits just above the input.
	if len(m.suggestions) > 0 {
		b.WriteString(m.renderSuggestions())
		b.WriteString("\n")
	}

	// 4. Input at the bottom (open textarea, no leading prompt glyph).
	b.WriteString(m.input.render(max(0, m.width)))
	b.WriteString("\n")

	// 5. Status line below the input (m.status is already dimmed; only
	// truncate to the terminal width so it never wraps). The working
	// indicator lives on the separator rule, so the status never mentions it.
	status := m.status
	if m.width > 0 {
		b.WriteString(lipgloss.NewStyle().MaxWidth(m.width).Render(status))
	} else {
		b.WriteString(status)
	}
	return b.String()
}

func (m *tuiModel) inputHeight() int { return maxInputLines }

// maxInputLines is the fixed height (in rows) of the input box; text wraps
// within it.
const maxInputLines = 3

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

// boxWidth is the text width of the input box: the terminal width minus the
// prompt. It returns 0 until a real terminal width is known, in which
// case callers fall back to logical (line-based) movement — a degenerate
// sub-prompt width is not something a user navigates.
func (in *tuiInput) boxWidth(width int) int {
	if width > len(inputPrompt) {
		return width - len(inputPrompt)
	}
	return 0
}

// vrowEntry maps one visual (wrapped) row of the input back to the logical
// line and the rune offset at which the row begins.
type vrowEntry struct {
	line  int
	start int
}

// vrowEntries lists every visual row of the input in order, as a logical line
// plus the rune offset where the row starts.
func (in *tuiInput) vrowEntries(width int) []vrowEntry {
	var out []vrowEntry
	for li, line := range in.lines {
		off := 0
		for _, r := range wrapWords(line, width) {
			out = append(out, vrowEntry{line: li, start: off})
			off += len(r)
		}
	}
	return out
}

// visualPos returns the cursor's visual coordinates: the index of its visual
// row and its display column within that row. A cursor at the very end of a
// line sits just past its last visual row.
func (in *tuiInput) visualPos(width int) (vrow, vcol int) {
	for li := 0; li < in.row; li++ {
		vrow += len(wrapWords(in.lines[li], width))
	}
	line := in.lines[in.row]
	rows := wrapWords(line, width)
	off := 0
	for _, r := range rows {
		if in.col < off+len(r) {
			return vrow, runesWidth(r[:in.col-off])
		}
		off += len(r)
	}
	return vrow + len(rows) - 1, runesWidth(rows[len(rows)-1])
}

// gotoVisual places the cursor at the entry's logical position, preserving
// the display column (clamped to the target row's end). width must be the
// same value used to build the entry list.
func (in *tuiInput) gotoVisual(e vrowEntry, vcol, width int) {
	in.row = e.line
	line := in.lines[e.line]
	off := 0
	var rowText []rune
	for _, r := range wrapWords(line, width) {
		if off == e.start {
			rowText = r
			break
		}
		off += len(r)
	}
	in.col = e.start + runeIndexAt(rowText, vcol)
}

// moveUpVisual moves the cursor up one visual row; it reports false at the
// top of the input (or without a usable width), so the caller can fall back
// to history recall.
func (in *tuiInput) moveUpVisual(width int) bool {
	bw := in.boxWidth(width)
	if bw < 1 {
		return false
	}
	entries := in.vrowEntries(bw)
	vrow, vcol := in.visualPos(bw)
	if vrow <= 0 {
		return false
	}
	in.gotoVisual(entries[vrow-1], vcol, bw)
	return true
}

// moveDownVisual moves the cursor down one visual row; it reports false at
// the bottom of the input, so the caller can fall back to the next history
// entry.
func (in *tuiInput) moveDownVisual(width int) bool {
	bw := in.boxWidth(width)
	if bw < 1 {
		return false
	}
	entries := in.vrowEntries(bw)
	vrow, vcol := in.visualPos(bw)
	if vrow >= len(entries)-1 {
		return false
	}
	in.gotoVisual(entries[vrow+1], vcol, bw)
	return true
}

// WordLeft moves the cursor to the start of the previous word, skipping
// intervening whitespace; at the start of a line it jumps to the end of the
// previous line.
func (in *tuiInput) WordLeft() {
	line := in.lines[in.row]
	if in.col == 0 {
		if in.row > 0 {
			in.row--
			in.col = len(in.lines[in.row])
		}
		return
	}
	j := in.col
	for j > 0 && line[j-1] == ' ' {
		j--
	}
	for j > 0 && line[j-1] != ' ' {
		j--
	}
	in.col = j
}

// WordRight moves the cursor to the start of the next word, skipping
// intervening whitespace; at the end of a line it jumps to the start of the
// next line.
func (in *tuiInput) WordRight() {
	line := in.lines[in.row]
	if in.col >= len(line) {
		if in.row < len(in.lines)-1 {
			in.row++
			in.col = 0
		}
		return
	}
	j := in.col
	for j < len(line) && line[j] != ' ' {
		j++
	}
	for j < len(line) && line[j] == ' ' {
		j++
	}
	in.col = j
}

// WordBackspace deletes from the cursor back to the start of the previous
// word (whitespace included); at the start of a line it joins the previous
// line, mirroring Backspace.
func (in *tuiInput) WordBackspace() {
	line := in.lines[in.row]
	if in.col == 0 {
		if in.row > 0 {
			prev := in.lines[in.row-1]
			in.col = len(prev)
			in.lines[in.row-1] = append(prev, line...)
			in.lines = append(in.lines[:in.row], in.lines[in.row+1:]...)
			in.row--
		}
		return
	}
	j := in.col
	for j > 0 && line[j-1] == ' ' {
		j--
	}
	for j > 0 && line[j-1] != ' ' {
		j--
	}
	in.lines[in.row] = append(line[:j], line[in.col:]...)
	in.col = j
}

func (in *tuiInput) Home() { in.col = 0 }

func (in *tuiInput) End() { in.col = len(in.lines[in.row]) }

// inputPrompt is the prompt glyph shown in front of the input text, styled in
// the mint accent (Guac). Empty means no prompt is drawn and the input text
// starts at the left edge of the terminal.
const inputPrompt = ""

// render draws the input into a fixed-height (maxInputLines) textarea: the
// text wraps at boxW display columns, short lines are padded so the input
// spans the terminal, and the cursor is marked with a block character. The
// window follows the cursor, so earlier content scrolls out of view; a dim
// "…" in the rightmost cell of the first/last visible row marks content
// clipped above or below the window.
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
			// Wrap the prefix to find the cursor's visual row and its display
			// column; a word-wrap break can land the cursor earlier than a
			// pure char/boxW split.
			prefix := wrapWords(line[:in.col], boxW)
			cursorRow = len(rows) + len(prefix) - 1
			cursorCol = runesWidth(prefix[len(prefix)-1])
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
	clipAbove := start > 0
	clipBelow := start+maxInputLines < len(rows)
	lastVisible := len(rows) - 1
	if lastVisible >= start+maxInputLines {
		lastVisible = start + maxInputLines - 1
	}
	var out []string
	for i := start; i <= lastVisible; i++ {
		s := padRunes(rows[i], boxW)
		// Clip markers go under the cursor so the block always stays visible.
		if clipAbove && i == start {
			s = clipMarker(s, boxW)
		} else if clipBelow && i == lastVisible {
			s = clipMarker(s, boxW)
		}
		if i == cursorRow {
			overChar := cursorCol < runesWidth(rows[i])
			s = applyCursor(s, cursorCol, overChar)
		}
		if i == start && inputPrompt != "" {
			s = ansiAccent + inputPrompt + ansiReset + s
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
// block when the cursor sits past the text. col is a display column; it maps
// onto the rune that covers it, so a wide character under the cursor is
// highlighted whole. A cursor at the very end of a row that exactly fills the
// box is drawn over the last cell.
func applyCursor(s string, col int, overChar bool) string {
	r := []rune(s)
	if len(r) == 0 {
		return "█"
	}
	idx := runeIndexAt(r, col)
	if idx >= len(r) {
		idx = len(r) - 1
		overChar = false
	}
	if overChar {
		return string(r[:idx]) + "\x1b[7m" + string(r[idx]) + ansiReset + string(r[idx+1:])
	}
	return string(r[:idx]) + "█" + string(r[idx+1:])
}

// runeWidth returns the display width of a rune in terminal columns: wide
// (CJK, emoji) runes are 2, combining marks 0, everything else 1. Without
// this, wrapping a Japanese prompt by rune count would overflow the box.
func runeWidth(r rune) int { return runewidth.RuneWidth(r) }

// textWidth returns the display width of a string in terminal columns.
func textWidth(s string) int { return runewidth.StringWidth(s) }

// runesWidth is textWidth for a rune slice.
func runesWidth(s []rune) int { return runewidth.StringWidth(string(s)) }

// runeIndexAt maps a display column back to the rune index that covers it
// (the rune whose cell range contains displayCol), clamping to len(s).
func runeIndexAt(s []rune, displayCol int) int {
	w := 0
	for i, r := range s {
		rw := runeWidth(r)
		if w+rw > displayCol {
			return i
		}
		w += rw
	}
	return len(s)
}

// breakIndex returns the rune index where a width-limited first row of s
// ends: the furthest index whose display width fits within width, preferring
// to break just after a space (so a word is never split) — the space stays at
// the end of the row and any further leading spaces of the remainder are
// stripped by wrapWords.
func breakIndex(s []rune, width int) int {
	hard := 0
	w := 0
	for i, r := range s {
		rw := runeWidth(r)
		if w+rw > width {
			break
		}
		w += rw
		hard = i + 1
	}
	if hard == 0 {
		// First rune wider than the box (a wide char in a 1-column box):
		// take it anyway so the loop always makes progress.
		hard = 1
	}
	for j := hard - 1; j >= 0; j-- {
		if s[j] == ' ' {
			return j + 1
		}
	}
	return hard
}

// wrapWords breaks a logical line into visual rows of at most width display
// columns, preferring to break at whitespace so words are never split. A word
// longer than width hard-breaks (a wide rune is never split in half). The
// rows are purely visual: hard line breaks are handled by the caller
// splitting on '\n'.
func wrapWords(s []rune, width int) [][]rune {
	if width < 1 {
		return [][]rune{s}
	}
	if len(s) == 0 {
		return [][]rune{{}}
	}
	var out [][]rune
	for runesWidth(s) > width {
		cut := breakIndex(s, width)
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

// padRunes right-pads a visual row to width display columns.
func padRunes(r []rune, width int) string {
	s := string(r)
	if n := width - textWidth(s); n > 0 {
		s += strings.Repeat(" ", n)
	}
	return s
}

// clipMarker replaces the rightmost display cell of a padded row with "…",
// signalling that input content is scrolled out of the box above or below.
func clipMarker(s string, boxW int) string {
	r := []rune(s)
	r[runeIndexAt(r, boxW-1)] = '…'
	return string(r)
}

// stripANSI removes colour/reset escape sequences so display widths can be
// measured on rendered rows, and for clipboard copy.
func stripANSI(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, 0x1b)
		if i < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		if j := strings.IndexByte(s[i:], 'm'); j >= 0 {
			s = s[i+j+1:]
		} else {
			break
		}
	}
	return b.String()
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
