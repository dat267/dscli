package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dat267/dscli/internal/deepseek"
)

// ChatCmd implements `dscli chat`: a one-shot question, or an interactive
// multi-turn session when the prompt is omitted.
type ChatCmd struct {
	Prompt       []string      `arg:"" optional:"" help:"Question to ask; omit to start an interactive session"`
	Conversation string        `short:"c" help:"Continue an existing conversation (id printed at the end of each reply)"`
	Model        string        `short:"m" help:"Model for a new thread: default (Instant) or expert. Cannot be combined with --conversation — a thread's model is fixed when it is created"`
	Thinking     bool          `short:"t" help:"Enable DeepThink reasoning"`
	Search       bool          `short:"s" help:"Enable web search"`
	JSONOut      bool          `help:"Emit NDJSON: one {\"delta\":...} line per chunk, then a final {\"done\":true,\"conversation_id\":...} line"`
	Timeout      time.Duration `help:"Overall budget for one question (0 = no limit)" default:"15m"`

	Token     string `env:"DS_TOKEN" help:"DeepSeek user token (localStorage.userToken). Alternatively: config set token"`
	Cookie    string `env:"DS_COOKIE" help:"DeepSeek ds_session_id cookie value. Alternatively: config set cookie"`
	UserAgent string `env:"DS_USER_AGENT" help:"Browser user-agent; some deployments reject non-browser UAs"`

	NoPersist    bool   `help:"Do not persist or reuse the default session; the session is deleted when the run ends"`
	NoTranscript bool   `help:"Do not save session texts (transcripts) next to the config file"`
	Workdir      string `help:"Working directory for /file loads" default:"."`

	// answer overrides the reply-text writer (the TUI renders it into its
	// scrollback); nil falls back to deltaWriter (stdout / NDJSON).
	answer func(string) error
	// preview overrides where progress lines go (the TUI renders them into
	// its scrollback); nil falls back to stderr.
	preview func(string)

	// clientBase overrides the API base URL for tests.
	clientBase string

	// cfgPath is the config file path used for session persistence; set from
	// app in Run.
	cfgPath string
}

func (c *ChatCmd) Run(app *App, ctx context.Context) error {
	if app != nil {
		c.cfgPath = app.CfgPath()
	}
	if c.Token == "" {
		return errors.New("no DeepSeek session configured: pass --token/--cookie (or DS_TOKEN/DS_COOKIE) or run 'dscli login' and save the values with 'dscli config set'")
	}
	if c.Cookie == "" {
		fmt.Fprintln(os.Stderr, "warning: no ds_session_id cookie set; the site may reject requests without it")
	}
	if c.Model != "" && c.Conversation != "" {
		return errors.New("--model cannot be combined with --conversation: a thread's model is fixed when it is created (start a new conversation to switch models)")
	}
	if c.Model != "" && c.Model != "default" && c.Model != "expert" {
		return fmt.Errorf("unknown model %q (want default or expert)", c.Model)
	}
	if len(c.Prompt) == 0 {
		return c.repl(ctx)
	}
	return c.ask(ctx, strings.Join(c.Prompt, " "))
}

// newClient builds the DeepSeek client from the command's resolved
// credentials (flag/env/config).
func (c *ChatCmd) newClient() *deepseek.Client {
	return deepseek.NewClient(deepseek.Session{
		Token:     c.Token,
		Cookie:    c.Cookie,
		UserAgent: c.UserAgent,
	}, c.Timeout, c.clientBase)
}

// transcriptsOn reports whether this run saves session texts, given the
// resolved config path and flags.
func (c *ChatCmd) transcriptsOn() bool {
	return transcriptsEnabled(c.cfgPath, c.NoPersist, c.NoTranscript)
}

// oneTurn asks one question in the given conversation, feeding every reply
// delta to write, and returns the conversation id to use on the NEXT turn
// ("<session_id>:<message_id>") and whether the reply was rejected by the
// content-safety filter (the partial text already written is kept for
// /resume).
func (c *ChatCmd) oneTurn(ctx context.Context, client *deepseek.Client, conversation, prompt, model string, write func(string) error, sources *[]deepseek.Source) (string, bool, error) {
	sessionID, parentID := splitConversation(conversation)
	if sessionID == "" {
		sid, err := client.CreateChatSession(ctx)
		if err != nil {
			return "", false, fmt.Errorf("create chat session: %w", err)
		}
		sessionID = sid
	}
	// model_type is only sent (and only meaningful) on the first turn of a
	// thread; resuming reuses the thread's fixed model.
	modelType := ""
	if parentID == nil {
		modelType = model
	}

	reply, err := client.StreamCompletion(ctx, deepseek.CompletionRequest{
		ChatSessionID:   sessionID,
		ParentMessageID: parentID,
		Prompt:          prompt,
		ModelType:       modelType,
		ThinkingEnabled: c.Thinking,
		SearchEnabled:   c.Search,
	}, write)
	if err != nil {
		return "", false, err
	}
	if sources != nil {
		*sources = reply.Sources
	}
	if reply.Filtered {
		c.showPreview("note: reply was filtered by DeepSeek (content policy)")
	}
	return conversationID(sessionID, parentID, reply.MessageID), reply.Filtered, nil
}

// deltaWriter returns the writer that renders reply deltas to the user:
// NDJSON lines in --json-out mode, plain text otherwise.
func (c *ChatCmd) deltaWriter() func(string) error {
	return func(delta string) error {
		if c.JSONOut {
			return json.NewEncoder(os.Stdout).Encode(map[string]string{"delta": delta})
		}
		_, err := os.Stdout.WriteString(delta)
		return err
	}
}

// answerWriter returns the reply-text writer: the TUI override when set, else
// the NDJSON/plain deltaWriter.
func (c *ChatCmd) answerWriter() func(string) error {
	if c.answer != nil {
		return c.answer
	}
	return c.deltaWriter()
}

// showPreview renders a progress or system note, honouring the TUI override
// (nil falls back to noteToStderr so it reads as a side note rather than chat
// output).
func (c *ChatCmd) showPreview(text string) {
	if c.preview != nil {
		c.preview(text)
		return
	}
	noteToStderr(text)
}

// noteToStderr prints a system note to stderr, dim when stderr is a terminal
// (so it is visibly a note, not the model's reply — the same dim style the
// TUI uses for notes); piped output stays plain so scripts are not polluted
// with escape sequences.
func noteToStderr(text string) {
	u := ui{color: isTerminal(os.Stderr)}
	fmt.Fprintln(os.Stderr, u.dim(text))
}

// resumePrompt builds the continuation message sent by /resume: the filtered
// partial reply embedded as context with an instruction to continue it in the
// same voice. The content filter still applies to whatever the model
// generates next — this only recovers the user's own cut-off text.
func resumePrompt(partial, instruction string) string {
	var b strings.Builder
	b.WriteString("The previous reply was cut off by a content-safety filter. ")
	b.WriteString("Continue the answer from where it stopped, keeping the same language, style and format — do not restate the text above:\n\n")
	b.WriteString(partial)
	b.WriteString("\n")
	if instruction != "" {
		b.WriteString("\n" + instruction + "\n")
	}
	return b.String()
}

// maxMentionBytes caps how much of a file /file loads into the prompt.
const maxMentionBytes = 1 << 20 // 1 MiB

// mentionBlock returns the <file>/<dir> block for a path (relative to
// Workdir), or "" if the path cannot be loaded (missing, oversized, or not a
// regular file/directory). Used by the /file command.
func (c *ChatCmd) mentionBlock(p string) string {
	path := p
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.Workdir, p)
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	// A directory expands to a listing of its contents, so the model sees what
	// is in the path instead of a literal name it may misread.
	if info.IsDir() {
		return fmt.Sprintf("\n<dir path=%q>\n%s\n</dir>\n", p, dirListing(path))
	}
	if !info.Mode().IsRegular() || info.Size() > maxMentionBytes {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("\n<file path=%q>\n%s\n</file>\n", p, data)
}

// maxDirListingEntries caps how many directory entries a @dir mention lists.
const maxDirListingEntries = 200

// dirListing renders a directory's immediate entries (relative names, dirs
// marked with a trailing "/"), bounded so a huge directory cannot flood the
// prompt.
func dirListing(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "(could not read directory)"
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	if len(names) > maxDirListingEntries {
		names = append(names[:maxDirListingEntries],
			fmt.Sprintf("… (%d more entries)", len(entries)-maxDirListingEntries))
	}
	return strings.Join(names, "\n")
}

// ask answers a single question and exits. By default it resumes the
// persisted default session (creating and saving one on first use); with
// --no-persist it runs in a fresh session that is deleted afterwards.
func (c *ChatCmd) ask(ctx context.Context, prompt string) error {
	client := c.newClient()
	var sources []deepseek.Source
	conversation := c.Conversation
	trusted := false
	var cleanup func()
	if conversation == "" {
		var err error
		conversation, trusted, cleanup, err = resolveDefaultSession(ctx, client, c.cfgPath, c.NoPersist)
		if err != nil {
			return err
		}
		if cleanup != nil {
			defer cleanup()
		}
	}

	var convID string
	var replyBuf strings.Builder
	if c.transcriptsOn() {
		appendTranscript(c.cfgPath, conversation, "user", prompt)
	}
	_, err := recoverStaleSession(ctx, client, c.cfgPath, conversation, trusted, func(sid string) error {
		cid, _, e := c.oneTurn(ctx, client, sid, prompt, effectiveModel(c.Model), func(delta string) error {
			replyBuf.WriteString(delta)
			return c.answerWriter()(delta)
		}, &sources)
		if e == nil {
			convID = cid
		}
		return e
	})
	if err != nil {
		return err
	}
	if c.transcriptsOn() {
		appendTranscript(c.cfgPath, convID, "assistant", replyBuf.String())
	}
	persistConversation(c.cfgPath, c.NoPersist, convID)
	if c.JSONOut {
		out := map[string]any{"done": true, "conversation_id": convID}
		if len(sources) > 0 {
			out["sources"] = sources
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	fmt.Fprintf(os.Stderr, "\nconversation: %s\n", convID)
	renderSources(os.Stderr, sources)
	return nil
}

// repl runs an interactive multi-turn session.
//
// By default it resumes the persisted default session (creating and saving one
// on first use), so the thread carries across launches. With --no-persist it
// creates a fresh session at launch, keeps every turn in it, and deletes it
// (plus any sessions spawned by /new or /model) when the session ends — however
// it ends, including Ctrl-C.
func (c *ChatCmd) repl(ctx context.Context) error {
	client := c.newClient()

	var owned []string
	if c.Conversation != "" {
		return c.replLoop(ctx, client, c.Conversation, owned, false)
	}
	sessionID, trusted, cleanup, err := resolveDefaultSession(ctx, client, c.cfgPath, c.NoPersist)
	if err != nil {
		return fmt.Errorf("create chat session: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	// When both stdin and stdout are terminals, run the bubbletea TUI (the
	// opencode/Claude Code-style prompt); otherwise fall back to the plain
	// line-based loop (pipes, scripts, tests).
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		m := newTUIModel(c, client, sessionID, trusted)
		if trusted {
			loadHistoryInto(ctx, m, client, sessionID)
		}
		if err := runTUI(m); err != nil {
			return err
		}
		if m.turns > 0 {
			fmt.Fprintf(os.Stderr, "\nconversation: %s\n", m.conversation)
		}
		return nil
	}
	return c.replLoop(ctx, client, sessionID, owned, trusted)
}

// ui wraps the terminal styling used by the REPL. Colours are only emitted
// when the note stream (stderr) is a terminal, so piped output stays clean.
type ui struct {
	color bool
}

var (
	ansiDim   = "\x1b[2m"
	ansiBold  = "\x1b[1m"
	ansiCyan  = "\x1b[38;2;107;80;255m"  // Charple violet
	ansiMuted = "\x1b[38;2;133;131;146m" // Squid grey
	ansiRed   = "\x1b[38;2;255;87;125m"  // Coral
	ansiReset = "\x1b[0m"
)

func (u ui) dim(s string) string   { return u.wrap(ansiDim, s) }
func (u ui) bold(s string) string  { return u.wrap(ansiBold, s) }
func (u ui) cyan(s string) string  { return u.wrap(ansiCyan, s) }
func (u ui) muted(s string) string { return u.wrap(ansiMuted, s) }
func (u ui) red(s string) string   { return u.wrap(ansiRed, s) }

// note renders chat-pane system text (slash-command feedback, hints, notes)
// as dimmed grey: dim alone reads as normal text on terminals that ignore
// SGR 2, and plain grey as the reply on terminals that ignore truecolour, so
// both are combined to keep it visibly a side note on either.
func (u ui) note(s string) string {
	if !u.color {
		return s
	}
	return ansiDim + ansiMuted + s + ansiReset
}
func (u ui) wrap(code, s string) string {
	if !u.color {
		return s
	}
	return code + s + ansiReset
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func (c *ChatCmd) replLoop(ctx context.Context, client *deepseek.Client, conversation string, owned []string, trusted bool) error {
	model := effectiveModel(c.Model)
	thinking := c.Thinking
	search := c.Search
	u := ui{color: isTerminal(os.Stderr)}
	interactive := isTerminal(os.Stdin)

	// The persisted default session is recovered once if its first turn fails
	// (the saved id may no longer exist server-side); fresh and ephemeral
	// sessions never are.
	firstTurn := trusted
	persist := !c.NoPersist && c.cfgPath != ""

	deleteOwned := func() {
		if len(owned) == 0 {
			return
		}
		if err := client.DeleteSessions(context.Background(), owned); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to delete session(s): %v\n", err)
		}
	}
	// Ctrl-C must clean up too: defers do not run on os.Exit.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		deleteOwned()
		os.Exit(130)
	}()
	defer func() {
		signal.Stop(sig)
		deleteOwned()
	}()

	mode := "ephemeral"
	if c.Conversation != "" {
		mode = "continuing"
	} else if persist {
		mode = "persisted"
	}
	// status redraws the live settings line; it is shown at launch and after
	// every /thinking, /search or /model change.
	status := func() {
		fmt.Fprintf(os.Stderr, "%s\n", u.dim(fmt.Sprintf(
			"DeepSeek · model %s · thinking %s · search %s · %s",
			model, onoff(thinking), onoff(search), mode,
		)))
	}
	status()
	fmt.Fprintln(os.Stderr, u.dim("one question per line · /help for commands"))

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var turns int
	// lastPartial keeps the text of the most recent filtered reply so /resume
	// can continue it; it is cleared when a later turn completes unfiltered.
	var lastPartial string
	for {
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "":
			continue
		case line == "/exit" || line == "/quit":
			if turns > 0 {
				fmt.Fprintf(os.Stderr, "conversation: %s\n", conversation)
			}
			return nil
		case line == "/new":
			conversation = ""
			fmt.Fprintln(os.Stderr, u.note("new conversation"))
			continue
		case line == "/help":
			printReplHelp(u)
			fmt.Fprintln(os.Stderr) // blank line: help is a block, not chat text
			continue
		case line == "/model" || strings.HasPrefix(line, "/model "):
			m := strings.TrimSpace(strings.TrimPrefix(line, "/model"))
			if m == "" {
				status()
				fmt.Fprintln(os.Stderr, u.note("model: "+model+" (fixed per thread; /model <default|expert> starts a new conversation)"))
				continue
			}
			if m != "default" && m != "expert" {
				fmt.Fprintf(os.Stderr, "unknown model %q (want default or expert)\n", m)
				continue
			}
			model = m
			conversation = ""
			status()
			fmt.Fprintln(os.Stderr, u.note("new conversation"))
			continue
		case line == "/thinking" || strings.HasPrefix(line, "/thinking "):
			thinking = toggleState(line, "/thinking", thinking)
			status()
			continue
		case line == "/search" || strings.HasPrefix(line, "/search "):
			search = toggleState(line, "/search", search)
			status()
			continue
		case line == "/resume" || strings.HasPrefix(line, "/resume "):
			// Continue a reply the content filter cut off: the partial text
			// is sent back as context with a continue instruction. Nothing is
			// bypassed — the filter still applies to the new generation.
			if lastPartial == "" {
				fmt.Fprintln(os.Stderr, u.red("nothing to resume: no filtered partial (or the last reply was accepted)"))
				continue
			}
			instruction := strings.TrimSpace(strings.TrimPrefix(line, "/resume"))
			line = resumePrompt(lastPartial, instruction)
			fmt.Fprintln(os.Stderr, u.note("resuming from the filtered partial reply"))
		case line == "/sessions":
			rows, err := localSessionRows(c.cfgPath)
			if err != nil {
				fmt.Fprintln(os.Stderr, u.red("error: "+err.Error()))
				continue
			}
			if len(rows) == 0 {
				fmt.Fprintln(os.Stderr, u.note("no local sessions (nothing saved yet)"))
				continue
			}
			fmt.Fprintln(os.Stderr, u.note("local sessions (most recent first; the default is resumed on launch):"))
			for _, r := range rows {
				fmt.Fprintln(os.Stderr, u.note(sessionRowText(r)))
			}
			fmt.Fprintln(os.Stderr)
			continue
		case line == "/session" || strings.HasPrefix(line, "/session "):
			arg := strings.TrimSpace(strings.TrimPrefix(line, "/session"))
			if arg == "" {
				if saved := loadSavedSession(c.cfgPath); saved != "" {
					fmt.Fprintln(os.Stderr, u.note("conversation: "+saved))
				} else {
					fmt.Fprintln(os.Stderr, u.note("no persisted session"))
				}
				fmt.Fprintln(os.Stderr)
				continue
			}
			bare, _ := splitConversation(arg)
			if bare == "" {
				fmt.Fprintln(os.Stderr, u.red("give a session id (see /sessions)"))
				continue
			}
			if err := saveSession(c.cfgPath, bare); err != nil {
				fmt.Fprintln(os.Stderr, u.red("error: "+err.Error()))
				continue
			}
			if msgs, ok := transcriptCount(c.cfgPath, bare); ok {
				fmt.Fprintf(os.Stderr, "%s\n", u.note(fmt.Sprintf("switched to session %s (%d saved messages; resumes from its root)", bare, msgs)))
			} else {
				fmt.Fprintf(os.Stderr, "%s\n", u.note("switched to session "+bare+" (no local transcript yet)"))
			}
			fmt.Fprintln(os.Stderr)
			// The live chat now continues the selected thread, from its root.
			conversation = bare
			lastPartial = ""
			continue
		case strings.HasPrefix(line, "/"):
			fmt.Fprintln(os.Stderr, u.red("unknown command (/help for commands)"))
			continue
		}

		// Multi-line prompt: a line ending in a single backslash continues
		// onto the next line (a lone "\" line inserts a blank line and keeps
		// going; "\\" at the end sends a literal backslash and ends the
		// message). Continuation lines are never treated as commands.
		for hasContinuation(line) {
			line = line[:len(line)-1] // drop the trailing backslash
			if interactive {
				fmt.Fprint(os.Stderr, u.bold(u.cyan("...> ")))
			}
			if !scanner.Scan() {
				break
			}
			line += "\n" + strings.TrimSpace(scanner.Text())
		}

		// A reset (/new, /model) leaves conversation empty; the next turn
		// spawns a fresh session. In persist mode it becomes the new default
		// (saved to the config); otherwise it is tracked for deletion at close.
		if conversation == "" {
			sid, err := client.CreateChatSession(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: create chat session: %v\n", err)
				continue
			}
			conversation = sid
			if persist {
				if err := saveSession(c.cfgPath, sid); err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not save session: %v\n", err)
				}
			} else {
				owned = append(owned, sid)
			}
		}

		var last byte
		var sources []deepseek.Source
		var convID string
		var replyBuf strings.Builder
		write := func(delta string) error {
			if len(delta) > 0 {
				last = delta[len(delta)-1]
			}
			replyBuf.WriteString(delta)
			_, err := os.Stdout.WriteString(delta)
			return err
		}
		turnSession := conversation
		if c.transcriptsOn() {
			appendTranscript(c.cfgPath, turnSession, "user", line)
		}
		var filtered bool
		_, err := recoverStaleSession(ctx, client, c.cfgPath, conversation, firstTurn, func(sid string) error {
			cid, isFiltered, e := c.oneTurn(ctx, client, sid, line, model, write, &sources)
			if e == nil {
				convID = cid
				filtered = isFiltered
			}
			return e
		})
		firstTurn = false
		if err != nil {
			if last != '\n' && last != 0 {
				fmt.Fprintln(os.Stdout)
			}
			fmt.Fprintln(os.Stderr, u.red("error: "+err.Error()))
			if last != '\n' {
				fmt.Fprintln(os.Stdout)
			}
			continue
		}
		if c.transcriptsOn() {
			appendTranscript(c.cfgPath, turnSession, "assistant", replyBuf.String())
		}
		if filtered {
			// The filter cut the reply off; whatever streamed before it is the
			// seed for /resume.
			if replyBuf.Len() > 0 {
				lastPartial = replyBuf.String()
				fmt.Fprintln(os.Stderr, u.note("hint: /resume continues from the partial reply (kept as context)"))
			} else {
				lastPartial = ""
			}
		} else {
			lastPartial = ""
		}
		if last != '\n' {
			fmt.Fprintln(os.Stdout)
		}
		fmt.Fprintln(os.Stdout) // blank line before the next prompt
		renderSources(os.Stderr, sources)
		conversation = convID
		persistConversation(c.cfgPath, c.NoPersist, conversation)
		turns++
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if turns > 0 {
		fmt.Fprintf(os.Stderr, "conversation: %s\n", conversation)
	}
	return nil
}

// hasContinuation reports whether a (possibly multi-line) prompt ends in a
// single, unescaped backslash — the marker that the message continues on the
// next line. "\\" at the end is an escaped backslash, not a continuation.
func hasContinuation(s string) bool {
	return strings.HasSuffix(s, "\\") && !strings.HasSuffix(s, "\\\\")
}

// onoff renders a boolean as "on"/"off".
func onoff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

func printReplHelp(u ui) {
	fmt.Fprintln(os.Stderr, u.dim(`commands:
  /exit, /quit                leave the session
  /new                        start a fresh conversation
  /model <default|expert>     switch model (starts a fresh conversation)
  /thinking [on|off]          toggle DeepThink reasoning
  /search [on|off]            toggle web search
  /resume [instruction]       continue a reply the filter cut off, from its partial text
  /session [id]               show the current conversation; select a saved session to resume
  /sessions                   list sessions with saved texts
  /help                       this help

multiline: end a line with \ to continue it on the next line; a lone \ line
inserts a blank line and keeps going. A trailing \\ (two backslashes) does not
continue — the line is sent literally.`))
}

// toggleState updates a boolean from a slash command: a bare "/cmd" flips the
// current value, "/cmd on|off|true|false|1|0|yes|no" sets it explicitly, and
// anything else leaves it unchanged.
func toggleState(line, cmd string, current bool) bool {
	arg := strings.TrimSpace(strings.TrimPrefix(line, cmd))
	if arg == "" {
		return !current
	}
	switch arg {
	case "on", "1", "true", "yes":
		return true
	case "off", "0", "false", "no":
		return false
	}
	return current
}

// effectiveModel maps an empty --model to the site's fast default.
func effectiveModel(m string) string {
	if m == "" {
		return "default"
	}
	return m
}

// splitConversation parses a conversation id ("<session_id>[:<message_id>]")
// into a chat session id and the parent message id. An empty session part is
// treated as "start a new conversation".
func splitConversation(id string) (sessionID string, parentID *int64) {
	if id == "" {
		return "", nil
	}
	session, after, found := strings.Cut(id, ":")
	if !found {
		return id, nil
	}
	if session == "" {
		return "", nil
	}
	if n, err := strconv.ParseInt(after, 10, 64); err == nil {
		return session, &n
	}
	return session, nil
}

// conversationID renders the id to pass on the NEXT turn: the freshly
// produced assistant message id when available (msgID), else the id used to
// ask (parentID), else the bare session.
func conversationID(sessionID string, parentID *int64, msgID int64) string {
	if msgID != 0 {
		return fmt.Sprintf("%s:%d", sessionID, msgID)
	}
	if parentID != nil {
		return fmt.Sprintf("%s:%d", sessionID, *parentID)
	}
	return sessionID
}
