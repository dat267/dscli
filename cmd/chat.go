package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
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

// ask answers a single question and exits.
func (c *ChatCmd) ask(ctx context.Context, prompt string) error {
	client := c.newClient()
	write := func(delta string) error {
		if c.JSONOut {
			return json.NewEncoder(os.Stdout).Encode(map[string]string{"delta": delta})
		}
		_, err := os.Stdout.WriteString(delta)
		return err
	}
	convID, err := c.oneTurn(ctx, client, c.Conversation, prompt, effectiveModel(c.Model), write)
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

	if c.Conversation == "" {
		fmt.Fprintln(os.Stderr, u.dim("DeepSeek · model "+model+" · ephemeral session (deleted on close)"))
	} else {
		fmt.Fprintln(os.Stderr, u.dim("DeepSeek · model "+model+" · continuing conversation"))
	}
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
		case strings.HasPrefix(line, "/model "):
			m := strings.TrimSpace(strings.TrimPrefix(line, "/model "))
			if m != "default" && m != "expert" {
				fmt.Fprintf(os.Stderr, "unknown model %q (want default or expert)\n", m)
				continue
			}
			model = m
			conversation = ""
			fmt.Fprintf(os.Stderr, "%s\n", u.dim("model: "+m+" · new conversation"))
			continue
		case strings.HasPrefix(line, "/thinking "):
			thinking = parseToggle(line, "/thinking ", thinking)
			fmt.Fprintf(os.Stderr, "%s\n", u.dim("thinking "+onoff(thinking)))
			continue
		case strings.HasPrefix(line, "/search "):
			search = parseToggle(line, "/search ", search)
			fmt.Fprintf(os.Stderr, "%s\n", u.dim("search "+onoff(search)))
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

		// Stream the reply to stdout while remembering its final character so
		// the next prompt always begins on a fresh line, with one blank line
		// separating turns.
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
  /exit, /quit             leave the session
  /new                     start a fresh conversation
  /model <default|expert>  switch model (starts a fresh conversation)
  /thinking <on|off>       toggle DeepThink reasoning
  /search <on|off>         toggle web search
  /help                    this help`))
}

func parseToggle(line, prefix string, current bool) bool {
	v := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	switch v {
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
