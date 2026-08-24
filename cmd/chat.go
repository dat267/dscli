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
	"github.com/dat267/dscli/internal/filetools"
	"github.com/dat267/dscli/internal/translate"
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

	FileTools    bool   `help:"Let the model inspect and edit files in the working directory and fetch URLs (writes always ask for confirmation first)"`
	NoPersist    bool   `help:"Do not persist or reuse the default session; the session is deleted when the run ends"`
	Instructions string `help:"Custom translation instructions file for translate_file (default: translate/<from>-<to>.md, then a built-in general style)"`
	ChatStyle    string `help:"Instruction file prepended to every chat turn (default: chat/chat.md, then chat/default.md, then a built-in mature-audience style)"`
	Workdir      string `help:"Working directory for file tools" default:"."`
	MaxRead      int    `help:"Max bytes read_file and fetch_url will return; oversized or binary files/responses are rejected (default: 512 KiB)" default:"0"`

	// chatStyle and chatStyleResolved hold the lazily-resolved general-chat
	// instruction (see oneTurn).
	chatStyle         string
	chatStyleResolved bool

	// confirm overrides the write-confirmation prompt (used by `do -y` to
	// auto-approve every write, and the TUI to prompt in-app); nil falls back
	// to the global confirmWrite.
	confirm func(string) bool
	// answer overrides the reply-text writer (the TUI renders it into its
	// scrollback); nil falls back to deltaWriter (stdout / NDJSON).
	answer func(string) error
	// preview overrides where plan previews and progress lines go (the TUI
	// renders them into its scrollback); nil falls back to stderr.
	preview func(string)

	// cfgPath is the config file path used for session persistence; set from
	// app in Run.
	cfgPath string
}

// confirmOp asks the user to approve a file write, honouring a per-command
// override (e.g. `do -y`) before the global terminal prompt.
func (c *ChatCmd) confirmOp(msg string) bool {
	if c != nil && c.confirm != nil {
		return c.confirm(msg)
	}
	return confirmWrite(msg)
}

// confirmWrite asks the user to approve a file write. It reads from the
// controlling terminal (/dev/tty) so it never clashes with the REPL's stdin
// scanner; when that is unavailable it falls back to a terminal stdin, and
// denies the write if no terminal exists at all. Overridable in tests.
var confirmWrite = func(msg string) bool {
	prompt := fmt.Sprintf("%s [y/N] ", msg)
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err == nil {
		defer tty.Close()
		fmt.Fprint(os.Stderr, prompt)
		var answer string
		_, _ = fmt.Fscanln(tty, &answer)
		return yes(answer)
	}
	if isTerminal(os.Stdin) {
		fmt.Fprint(os.Stderr, prompt)
		var answer string
		_, _ = fmt.Scanln(&answer)
		return yes(answer)
	}
	fmt.Fprintln(os.Stderr, "edit denied: no terminal available for confirmation")
	return false
}

func yes(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes":
		return true
	}
	return false
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
	if c.MaxRead < 0 {
		return errors.New("--max-read cannot be negative")
	}
	if c.MaxRead > 0 {
		filetools.MaxReadBytes = c.MaxRead
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
	}, c.Timeout)
}

// oneTurn asks one question in the given conversation, feeding every reply
// delta to write, and returns the conversation id to use on the NEXT turn
// ("<session_id>:<message_id>").
//
// The chat style is only used as a fallback: the prompt is sent as-is first,
// and if the reply is cut off / content-filtered (a refusal), it is retried
// once with the style prepended. Normal conversations never carry the style
// block.
func (c *ChatCmd) oneTurn(ctx context.Context, client *deepseek.Client, conversation, prompt, model string, write func(string) error, sources *[]deepseek.Source) (string, error) {
	if !c.chatStyleResolved {
		c.chatStyleResolved = true
		s, err := ResolveChatStyle(c.ChatStyle)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v; using the built-in chat style\n", err)
			s = DefaultChatStyle()
		}
		c.chatStyle = s
	}
	convID, rejected, err := c.completion(ctx, client, conversation, prompt, model, write, sources)
	if err != nil {
		return "", err
	}
	if rejected && c.chatStyle != "" {
		c.showPreview("reply was filtered; retrying with the chat style")
		convID, _, err = c.completion(ctx, client, convID, c.chatStyle+"\n\n"+prompt, model, write, sources)
	}
	return convID, err
}

// completion sends one completion request and returns the next conversation
// id, whether the reply was cut off / content-filtered, and any error.
func (c *ChatCmd) completion(ctx context.Context, client *deepseek.Client, conversation, prompt, model string, write func(string) error, sources *[]deepseek.Source) (string, bool, error) {
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
	return conversationID(sessionID, parentID, reply.MessageID), reply.Truncated, nil
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

// showPreview renders a plan preview or progress line, honouring the TUI
// override (nil falls back to stderr).
func (c *ChatCmd) showPreview(text string) {
	if c.preview != nil {
		c.preview(text)
		return
	}
	fmt.Fprintln(os.Stderr, text)
}

// turn answers one user message and returns the conversation id for the next
// turn. With fileTools enabled it runs the model↔file loop: a reply that is a
// single file-tool JSON object is executed (writes always confirm first) and
// the <tool_result> fed back as the next turn, until the model answers in
// prose. Tool turns print a dim note instead of the raw JSON; the final prose
// is rendered when it arrives.
func (c *ChatCmd) turn(ctx context.Context, client *deepseek.Client, conversation, prompt, model string, fileTools bool, note func(string), sources *[]deepseek.Source) (string, error) {
	if !fileTools {
		return c.oneTurn(ctx, client, conversation, prompt, model, c.answerWriter(), sources)
	}

	workdir, err := filepath.Abs(c.Workdir)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	write := c.answerWriter()

	cur := conversation
	curPrompt := filetools.Instructions(workdir) + prompt
	var prevCall string // identity of the last executed read-only tool call
	for i := 0; i < filetools.MaxIterations; i++ {
		var buf strings.Builder
		convID, err := c.oneTurn(ctx, client, cur, curPrompt, model, func(d string) error {
			buf.WriteString(d)
			return nil
		}, sources)
		if err != nil {
			return "", err
		}
		cur = convID

		call, ok := filetools.Extract(buf.String())
		if !ok {
			// Final answer: render the buffered text now, terminated by a
			// newline so callers can add a clean blank separator.
			text := buf.String()
			if !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			if err := write(text); err != nil {
				return "", err
			}
			return cur, nil
		}

		// Tool turn: never prints the raw JSON. Writes and deletions are planned,
		// previewed, confirmed, then applied — the preview and the write derive
		// from the same read, so what the user approves is exactly what happens.
		display := ""
		switch {
		case call.Tool == "fetch_url":
			display = call.URL
		case call.Path == "":
			display = "."
		default:
			display = filetools.Display(workdir, call.Path)
		}

		// A DeepThink model occasionally re-issues the exact deterministic
		// read-only call it was just shown (e.g. list_directory . twice). Skip
		// the duplicate and point the model at the result it already has,
		// instead of burning a tool-call budget slot on a re-run.
		if key := callKey(call); key != "" && key == prevCall {
			note(fmt.Sprintf("%s %s (duplicate, skipped)", call.Tool, display))
			curPrompt = filetools.FormatResult(call.Tool, call.Path,
				"WARNING: you already requested this exact tool call in the previous turn and its <tool_result> is in the conversation above. Do not repeat it: re-read that result and continue, or give your final answer.")
			continue
		}
		prevCall = callKey(call)

		note(fmt.Sprintf("%s %s", call.Tool, display))
		switch call.Tool {
		case "read_file", "list_directory", "file_meta", "grep", "fetch_url":
			curPrompt = call.Run(workdir)
			continue
		case "edit_file":
			plan := filetools.PlanEdit(workdir, call)
			if plan.Result != "" {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, plan.Result)
				continue
			}
			c.showPreview(plan.Preview)
			if !c.confirmOp(fmt.Sprintf("apply edit to %s?", display)) {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, "ERROR: edit rejected by user; do not retry it")
				continue
			}
			if err := filetools.ApplyEdit(workdir, call, plan); err != nil {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, "ERROR: "+err.Error())
				continue
			}
			curPrompt = filetools.FormatResult(call.Tool, call.Path, filetools.EditSummary(plan.Count, call.Old, call.New))
			continue
		case "create_file":
			plan := filetools.PlanCreate(workdir, call)
			if plan.Result != "" {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, plan.Result)
				continue
			}
			c.showPreview(plan.Preview)
			if !c.confirmOp(fmt.Sprintf("create %s?", display)) {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, "ERROR: create rejected by user; do not retry it")
				continue
			}
			if err := filetools.ApplyCreate(workdir, call, plan.NewContent); err != nil {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, "ERROR: "+err.Error())
				continue
			}
			curPrompt = filetools.FormatResult(call.Tool, call.Path, fmt.Sprintf("created %s (%d bytes)", display, len(plan.NewContent)))
			continue
		case "rename_file":
			plan := filetools.PlanRename(workdir, call)
			if plan.Result != "" {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, plan.Result)
				continue
			}
			c.showPreview(plan.Preview)
			if !c.confirmOp(fmt.Sprintf("rename %s → %s?", plan.OldName, plan.NewName)) {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, "ERROR: rename rejected by user; do not retry it")
				continue
			}
			if err := filetools.ApplyRename(workdir, call); err != nil {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, "ERROR: "+err.Error())
				continue
			}
			curPrompt = filetools.FormatResult(call.Tool, call.Path, fmt.Sprintf("renamed %s → %s", plan.OldName, plan.NewName))
			continue
		case "delete_file":
			plan := filetools.PlanDelete(workdir, call)
			if plan.Result != "" {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, plan.Result)
				continue
			}
			c.showPreview(plan.Preview)
			if !c.confirmOp(fmt.Sprintf("delete %s?", display)) {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, "ERROR: delete rejected by user; do not retry it")
				continue
			}
			if err := filetools.ApplyDelete(workdir, call); err != nil {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, "ERROR: "+err.Error())
				continue
			}
			curPrompt = filetools.FormatResult(call.Tool, call.Path, fmt.Sprintf("deleted %s (%d bytes)", display, plan.Bytes))
			continue
		case "translate_file":
			curPrompt = c.applyTranslateFile(ctx, client, workdir, call, model, display)
			continue
		}
		curPrompt = filetools.FormatResult(call.Tool, call.Path, "ERROR: unknown tool")
	}
	// The model spent its whole tool budget without reaching prose (e.g. it
	// kept exploring a very large tree). Force one final answer so the user
	// always gets a conclusion instead of an abrupt limit error.
	var buf strings.Builder
	convID, err := c.oneTurn(ctx, client, cur, filetools.CapPrompt, model, func(d string) error {
		buf.WriteString(d)
		return nil
	}, sources)
	if err != nil {
		return "", err
	}
	text := buf.String()
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if err := write(text); err != nil {
		return "", err
	}
	return convID, nil
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
	_, err := recoverStaleSession(ctx, client, c.cfgPath, conversation, trusted, func(sid string) error {
		cid, e := c.turn(ctx, client, sid, prompt, effectiveModel(c.Model), c.FileTools, func(s string) {
			fmt.Fprintln(os.Stderr, s)
		}, &sources)
		if e == nil {
			convID = cid
		}
		return e
	})
	if err != nil {
		return err
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
	ansiCyan  = "\x1b[36m"
	ansiRed   = "\x1b[31m"
	ansiReset = "\x1b[0m"
)

func (u ui) dim(s string) string  { return u.wrap(ansiDim, s) }
func (u ui) bold(s string) string { return u.wrap(ansiBold, s) }
func (u ui) cyan(s string) string { return u.wrap(ansiCyan, s) }
func (u ui) red(s string) string  { return u.wrap(ansiRed, s) }
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
	fileTools := c.FileTools
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
	// every /thinking, /search, /tools or /model change.
	status := func() {
		fmt.Fprintf(os.Stderr, "%s\n", u.dim(fmt.Sprintf(
			"DeepSeek · model %s · thinking %s · search %s · tools %s · %s",
			model, onoff(thinking), onoff(search), onoff(fileTools), mode,
		)))
	}
	status()
	fmt.Fprintln(os.Stderr, u.dim("one question per line · /help for commands"))

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var turns int
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
			fmt.Fprintln(os.Stderr, u.dim("new conversation"))
			continue
		case line == "/help":
			printReplHelp(u)
			continue
		case line == "/model" || strings.HasPrefix(line, "/model "):
			m := strings.TrimSpace(strings.TrimPrefix(line, "/model"))
			if m == "" {
				status()
				fmt.Fprintln(os.Stderr, u.dim("model: "+model+" (fixed per thread; /model <default|expert> starts a new conversation)"))
				continue
			}
			if m != "default" && m != "expert" {
				fmt.Fprintf(os.Stderr, "unknown model %q (want default or expert)\n", m)
				continue
			}
			model = m
			conversation = ""
			status()
			fmt.Fprintln(os.Stderr, u.dim("new conversation"))
			continue
		case line == "/thinking" || strings.HasPrefix(line, "/thinking "):
			thinking = toggleState(line, "/thinking", thinking)
			status()
			continue
		case line == "/search" || strings.HasPrefix(line, "/search "):
			search = toggleState(line, "/search", search)
			status()
			continue
		case line == "/tools" || strings.HasPrefix(line, "/tools ") || line == "/files" || strings.HasPrefix(line, "/files "):
			cmd := "/tools"
			if strings.HasPrefix(line, "/files") {
				cmd = "/files"
			}
			fileTools = toggleState(line, cmd, fileTools)
			status()
			continue
		case strings.HasPrefix(line, "/"):
			fmt.Fprintln(os.Stderr, "unknown command (/help for commands)")
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

		// With file tools off the reply streams live and the final character is
		// tracked so the next prompt starts on a fresh line. With file tools on
		// turn() buffers and sieves tool calls, so last-char tracking is moot.
		if fileTools {
			var sources []deepseek.Source
			var convID string
			_, err := recoverStaleSession(ctx, client, c.cfgPath, conversation, firstTurn, func(sid string) error {
				cid, e := c.turn(ctx, client, sid, line, model, true, func(s string) {
					fmt.Fprintln(os.Stderr, u.dim(s))
				}, &sources)
				if e == nil {
					convID = cid
				}
				return e
			})
			firstTurn = false
			if err != nil {
				fmt.Fprintln(os.Stderr, u.red("error: "+err.Error()))
				fmt.Fprintln(os.Stdout)
				continue
			}
			conversation = convID
			persistConversation(c.cfgPath, c.NoPersist, conversation)
			renderSources(os.Stderr, sources)
			fmt.Fprintln(os.Stdout) // blank line before the next prompt
			turns++
			continue
		}

		var last byte
		var sources []deepseek.Source
		var convID string
		write := func(delta string) error {
			if len(delta) > 0 {
				last = delta[len(delta)-1]
			}
			_, err := os.Stdout.WriteString(delta)
			return err
		}
		_, err := recoverStaleSession(ctx, client, c.cfgPath, conversation, firstTurn, func(sid string) error {
			cid, e := c.oneTurn(ctx, client, sid, line, model, write, &sources)
			if e == nil {
				convID = cid
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

// callKey returns a deterministic identity for a read-only tool call, used to
// skip consecutive duplicates in the tool loop. It returns "" for calls that
// must never be deduplicated: writes/renames/deletes/translations (a repeat
// may be a legitimate retry after a rejection) and fetch_url (a network
// response can legitimately change between calls).
func callKey(c filetools.Call) string {
	switch c.Tool {
	case "read_file":
		return "read:" + c.Path
	case "file_meta":
		return "meta:" + c.Path
	case "list_directory":
		rec := 0
		if c.Recursive {
			rec = 1
		}
		return fmt.Sprintf("list:%s:%d", c.Path, rec)
	case "grep":
		return "grep:" + c.Path + ":" + c.Pattern
	}
	return ""
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

// applyTranslateFile runs the translate_file tool: preview + confirm, then a
// chunked translation in a dedicated ephemeral session (never the chat
// thread, so the conversation context stays clean). Returns the <tool_result>
// body for the model.
func (c *ChatCmd) applyTranslateFile(ctx context.Context, client *deepseek.Client, workdir string, call filetools.Call, model, display string) string {
	inPath, err := filetools.ResolvePath(workdir, call.Path)
	if err != nil {
		return filetools.FormatResult(call.Tool, call.Path, "ERROR: "+err.Error())
	}
	out := call.Output
	if out == "" {
		out = translate.DefaultOutput(call.Path, call.To)
	}
	outPath, err := filetools.ResolvePath(workdir, out)
	if err != nil {
		return filetools.FormatResult(call.Tool, call.Path, "ERROR: "+err.Error())
	}
	if outPath == inPath {
		return filetools.FormatResult(call.Tool, call.Path, "ERROR: input and output are the same path")
	}
	if _, err := os.Stat(outPath); err == nil {
		return filetools.FormatResult(call.Tool, call.Path, "ERROR: output already exists; choose another file or delete it first")
	}
	content, format, err := translate.Load(inPath, translate.MaxChatInputBytes)
	if err != nil {
		return filetools.FormatResult(call.Tool, call.Path, "ERROR: "+err.Error())
	}
	style, err := translate.ResolveStyle(c.Instructions, call.From, call.To)
	if err != nil {
		return filetools.FormatResult(call.Tool, call.Path, "ERROR: "+err.Error())
	}
	outDisplay := filetools.Display(workdir, out)
	preview := fmt.Sprintf("%s → %s\n(%s, to %s)", display, outDisplay, format, call.To)
	c.showPreview(preview)
	if !c.confirmOp(fmt.Sprintf("translate %s → %s to %s?", display, outDisplay, call.To)) {
		return filetools.FormatResult(call.Tool, call.Path, "ERROR: translate rejected by user; do not retry it")
	}

	sessionID, err := client.CreateChatSession(ctx)
	if err != nil {
		return filetools.FormatResult(call.Tool, call.Path, "ERROR: create session: "+err.Error())
	}
	cleanup := func() {
		if err := client.DeleteSessions(context.Background(), []string{sessionID}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to delete session: %v\n", err)
		}
	}
	var failed string
	defer func() {
		if failed != "" {
			_ = os.Remove(outPath) // never leave a partial output behind
		}
		cleanup()
	}()

	result, _, err := translate.Translate(ctx, client, sessionID, content, format, translate.Options{
		To:    call.To,
		Model: model,
		Style: style,
		OnChunk: func(chunk, total int) {
			c.showPreview(fmt.Sprintf("  chunk %d/%d ok", chunk, total))
		},
	})
	if err != nil {
		failed = err.Error()
		return filetools.FormatResult(call.Tool, call.Path, "ERROR: "+err.Error())
	}
	if err := os.WriteFile(outPath, []byte(result), 0644); err != nil {
		failed = err.Error()
		return filetools.FormatResult(call.Tool, call.Path, "ERROR: "+err.Error())
	}
	return filetools.FormatResult(call.Tool, call.Path,
		fmt.Sprintf("translated %s → %s (%s, %d bytes)", display, outDisplay, format, len(result)))
}

func printReplHelp(u ui) {
	fmt.Fprintln(os.Stderr, u.dim(`commands:
  /exit, /quit                leave the session
  /new                        start a fresh conversation
  /model <default|expert>     switch model (starts a fresh conversation)
  /thinking [on|off]          toggle DeepThink reasoning
  /search [on|off]            toggle web search
  /tools [on|off]            toggle file tools (/files still works) (list/read/meta/grep/fetch_url/create/edit/rename/delete/translate; writes ask first)
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
