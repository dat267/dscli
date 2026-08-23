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

	FileTools bool   `help:"Let the model read and edit files in the working directory (writes always ask for confirmation first)"`
	Workdir   string `help:"Working directory for file tools" default:"."`
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
	}, c.Timeout)
}

// oneTurn asks one question in the given conversation, feeding every reply
// delta to write, and returns the conversation id to use on the NEXT turn
// ("<session_id>:<message_id>").
func (c *ChatCmd) oneTurn(ctx context.Context, client *deepseek.Client, conversation, prompt, model string, write func(string) error) (string, error) {
	sessionID, parentID := splitConversation(conversation)
	if sessionID == "" {
		var err error
		sessionID, err = client.CreateChatSession(ctx)
		if err != nil {
			return "", fmt.Errorf("create chat session: %w", err)
		}
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
		return "", err
	}
	return conversationID(sessionID, parentID, reply.MessageID), nil
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

// turn answers one user message and returns the conversation id for the next
// turn. With fileTools enabled it runs the model↔file loop: a reply that is a
// single file-tool JSON object is executed (writes always confirm first) and
// the <tool_result> fed back as the next turn, until the model answers in
// prose. Tool turns print a dim note instead of the raw JSON; the final prose
// is rendered when it arrives.
func (c *ChatCmd) turn(ctx context.Context, client *deepseek.Client, conversation, prompt, model string, fileTools bool, note func(string)) (string, error) {
	if !fileTools {
		return c.oneTurn(ctx, client, conversation, prompt, model, c.deltaWriter())
	}

	workdir, err := filepath.Abs(c.Workdir)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	write := func(delta string) error {
		_, err := os.Stdout.WriteString(delta)
		return err
	}

	cur := conversation
	curPrompt := filetools.Instructions(workdir) + prompt
	for i := 0; i < filetools.MaxIterations; i++ {
		var buf strings.Builder
		convID, err := c.oneTurn(ctx, client, cur, curPrompt, model, func(d string) error {
			buf.WriteString(d)
			return nil
		})
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
		display := filetools.Display(workdir, call.Path)
		note(fmt.Sprintf("%s %s", call.Tool, display))
		switch call.Tool {
		case "read_file", "list_directory":
			curPrompt = call.Run(workdir)
			continue
		case "edit_file":
			plan := filetools.PlanEdit(workdir, call)
			if plan.Result != "" {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, plan.Result)
				continue
			}
			fmt.Fprintln(os.Stderr, plan.Preview)
			if !confirmWrite(fmt.Sprintf("apply edit to %s?", display)) {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, "ERROR: edit rejected by user; do not retry it")
				continue
			}
			if err := filetools.ApplyEdit(workdir, call, plan.NewContent); err != nil {
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
			fmt.Fprintln(os.Stderr, plan.Preview)
			if !confirmWrite(fmt.Sprintf("create %s?", display)) {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, "ERROR: create rejected by user; do not retry it")
				continue
			}
			if err := filetools.ApplyCreate(workdir, call, plan.NewContent); err != nil {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, "ERROR: "+err.Error())
				continue
			}
			curPrompt = filetools.FormatResult(call.Tool, call.Path, fmt.Sprintf("created %s (%d bytes)", display, len(plan.NewContent)))
			continue
		case "delete_file":
			plan := filetools.PlanDelete(workdir, call)
			if plan.Result != "" {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, plan.Result)
				continue
			}
			fmt.Fprintln(os.Stderr, plan.Preview)
			if !confirmWrite(fmt.Sprintf("delete %s?", display)) {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, "ERROR: delete rejected by user; do not retry it")
				continue
			}
			if err := filetools.ApplyDelete(workdir, call); err != nil {
				curPrompt = filetools.FormatResult(call.Tool, call.Path, "ERROR: "+err.Error())
				continue
			}
			curPrompt = filetools.FormatResult(call.Tool, call.Path, fmt.Sprintf("deleted %s (%d bytes)", display, plan.Bytes))
			continue
		}
		curPrompt = filetools.FormatResult(call.Tool, call.Path, "ERROR: unknown tool")
	}
	return "", fmt.Errorf("file tool loop exceeded %d turns", filetools.MaxIterations)
}

// ask answers a single question and exits.
func (c *ChatCmd) ask(ctx context.Context, prompt string) error {
	client := c.newClient()
	convID, err := c.turn(ctx, client, c.Conversation, prompt, effectiveModel(c.Model), c.FileTools, func(s string) {
		fmt.Fprintln(os.Stderr, s)
	})
	if err != nil {
		return err
	}
	if c.JSONOut {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"done": true, "conversation_id": convID})
	}
	fmt.Fprintf(os.Stderr, "\nconversation: %s\n", convID)
	return nil
}

// repl runs an interactive multi-turn session.
//
// Stateless by default: with no --conversation it creates a fresh session at
// launch, keeps every turn in it, and deletes it (plus any sessions spawned
// by /new or /model) when the session ends — however it ends, including
// Ctrl-C.
func (c *ChatCmd) repl(ctx context.Context) error {
	client := c.newClient()

	// Track the sessions this launch created so none of them outlive it.
	var owned []string
	if c.Conversation == "" {
		sid, err := client.CreateChatSession(ctx)
		if err != nil {
			return fmt.Errorf("create chat session: %w", err)
		}
		owned = append(owned, sid)
		return c.replLoop(ctx, client, sid, owned)
	}
	return c.replLoop(ctx, client, c.Conversation, owned)
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

func (c *ChatCmd) replLoop(ctx context.Context, client *deepseek.Client, conversation string, owned []string) error {
	model := effectiveModel(c.Model)
	thinking := c.Thinking
	search := c.Search
	fileTools := c.FileTools
	u := ui{color: isTerminal(os.Stderr)}
	interactive := isTerminal(os.Stdin)

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

	mode := "ephemeral (deleted on close)"
	if c.Conversation != "" {
		mode = "continuing conversation"
	}
	// status redraws the live settings line; it is shown at launch and after
	// every /thinking, /search, /files or /model change.
	status := func() {
		fmt.Fprintf(os.Stderr, "%s\n", u.dim(fmt.Sprintf(
			"DeepSeek · model %s · thinking %s · search %s · files %s · %s",
			model, onoff(thinking), onoff(search), onoff(fileTools), mode,
		)))
	}
	status()
	fmt.Fprintln(os.Stderr, u.dim("one question per line · /help for commands"))

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var turns int
	for {
		if interactive {
			fmt.Fprint(os.Stderr, u.bold(u.cyan("you> ")))
		}
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
		case line == "/files" || strings.HasPrefix(line, "/files "):
			fileTools = toggleState(line, "/files", fileTools)
			status()
			continue
		case strings.HasPrefix(line, "/"):
			fmt.Fprintln(os.Stderr, "unknown command (/help for commands)")
			continue
		}

		// A reset (/new, /model) leaves conversation empty; the next turn
		// spawns a fresh session that is also deleted at close.
		if conversation == "" {
			sid, err := client.CreateChatSession(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: create chat session: %v\n", err)
				continue
			}
			conversation = sid
			owned = append(owned, sid)
		}

		// With file tools off the reply streams live and the final character is
		// tracked so the next prompt starts on a fresh line. With file tools on
		// turn() buffers and sieves tool calls, so last-char tracking is moot.
		if fileTools {
			convID, err := c.turn(ctx, client, conversation, line, model, true, func(s string) {
				fmt.Fprintln(os.Stderr, u.dim(s))
			})
			if err != nil {
				fmt.Fprintln(os.Stderr, u.red("error: "+err.Error()))
				fmt.Fprintln(os.Stdout)
				continue
			}
			conversation = convID
			fmt.Fprintln(os.Stdout) // blank line before the next prompt
			turns++
			continue
		}

		var last byte
		write := func(delta string) error {
			if len(delta) > 0 {
				last = delta[len(delta)-1]
			}
			_, err := os.Stdout.WriteString(delta)
			return err
		}
		convID, err := c.oneTurn(ctx, client, conversation, line, model, write)
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
		conversation = convID
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
  /files [on|off]             toggle file tools (list/read/create/edit/delete in the CWD; writes ask first)
  /help                       this help`))
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
