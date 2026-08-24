package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/dat267/dscli/internal/deepseek"
)

// AskCmd implements `dscli ask`: a pure one-shot call — send the model an
// input, print the answer, done. The session is created per call and deleted
// afterwards, so nothing persists and no conversation-id noise is printed.
// The input may be positional args or piped stdin.
type AskCmd struct {
	Prompt    []string      `arg:"" optional:"" help:"Input to send (omit to read from stdin; use -- before a prompt that starts with -)"`
	Model     string        `short:"m" help:"Model: default (Instant) or expert" default:""`
	Thinking  bool          `short:"t" help:"Enable DeepThink reasoning"`
	Search    bool          `short:"s" help:"Enable web search"`
	NoPersist bool          `help:"Do not persist or reuse the default session; the session is deleted when the run ends"`
	JSONOut   bool          `help:"Emit NDJSON: one {\"delta\":...} line per chunk, then a {\"sources\":[...]} line when search returned citations"`
	Timeout   time.Duration `help:"Overall budget (0 = no limit)" default:"15m"`

	ChatStyle string `help:"Instruction file prepended to the prompt (default: chat/chat.md, then chat/default.md, then a built-in mature-audience style)"`

	Token     string `env:"DS_TOKEN" help:"DeepSeek user token (localStorage.userToken). Alternatively: config set token"`
	Cookie    string `env:"DS_COOKIE" help:"DeepSeek ds_session_id cookie value. Alternatively: config set cookie"`
	UserAgent string `env:"DS_USER_AGENT" help:"Browser user-agent; some deployments reject non-browser UAs"`

	// clientBase overrides the API base URL for tests.
	clientBase string

	// cfgPath is the config file path used for session persistence; set from
	// app in Run.
	cfgPath string
}

// renderSources prints citation footnotes (matching the inline [citation:N]
// markers) to w, numbered 1..N in order. A no-op when there are none.
func renderSources(w io.Writer, sources []deepseek.Source) {
	if len(sources) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Sources:")
	for i, s := range sources {
		if s.Title != "" {
			fmt.Fprintf(w, "  [%d] %s — %s\n", i+1, s.Title, s.URL)
		} else {
			fmt.Fprintf(w, "  [%d] %s\n", i+1, s.URL)
		}
	}
}

func (c *AskCmd) Run(app *App, ctx context.Context) error {
	if app != nil {
		c.cfgPath = app.CfgPath()
	}
	if c.Token == "" {
		return errors.New("no DeepSeek session configured: pass --token/--cookie (or DS_TOKEN/DS_COOKIE) or run 'dscli login' and save the values with 'dscli config set'")
	}
	if c.Model != "" && c.Model != "default" && c.Model != "expert" {
		return fmt.Errorf("unknown model %q (want default or expert)", c.Model)
	}

	prompt := strings.TrimSpace(strings.Join(c.Prompt, " "))
	if prompt == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		prompt = strings.TrimSpace(string(data))
	}
	if prompt == "" {
		return errors.New("nothing to ask: pass a prompt or pipe input on stdin")
	}
	style, err := ResolveChatStyle(c.ChatStyle)
	if err != nil {
		return err
	}

	client := deepseek.NewClient(deepseek.Session{
		Token:     c.Token,
		Cookie:    c.Cookie,
		UserAgent: c.UserAgent,
	}, c.Timeout, c.clientBase)

	// By default the persisted default session is resumed (created + saved on
	// first use); --no-persist runs in a fresh session deleted afterwards.
	sessionID, trusted, cleanup, err := resolveDefaultSession(ctx, client, c.cfgPath, c.NoPersist)
	if err != nil {
		return fmt.Errorf("create chat session: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	var last byte
	write := func(delta string) error {
		if len(delta) > 0 {
			last = delta[len(delta)-1]
		}
		if c.JSONOut {
			return json.NewEncoder(os.Stdout).Encode(map[string]string{"delta": delta})
		}
		_, err := os.Stdout.WriteString(delta)
		return err
	}
	var reply deepseek.Reply
	var usedSession string
	usedSession, err = recoverStaleSession(ctx, client, c.cfgPath, sessionID, trusted, func(sid string) error {
		sess, parent := splitConversation(sid)
		r, e := client.StreamCompletion(ctx, deepseek.CompletionRequest{
			ChatSessionID:   sess,
			ParentMessageID: parent,
			Prompt:          prompt,
			ModelType:       effectiveModel(c.Model),
			ThinkingEnabled: c.Thinking,
			SearchEnabled:   c.Search,
		}, write)
		if e != nil {
			return e
		}
		reply = r
		// The style is a fallback: only when the reply is cut off /
		// content-filtered is it retried once with the style prepended.
		if reply.Truncated && style != "" {
			next := reply.MessageID
			r2, e2 := client.StreamCompletion(ctx, deepseek.CompletionRequest{
				ChatSessionID:   sess,
				ParentMessageID: &next,
				Prompt:          style + "\n\n" + prompt,
				ModelType:       effectiveModel(c.Model),
				ThinkingEnabled: c.Thinking,
				SearchEnabled:   c.Search,
			}, write)
			if e2 != nil {
				return e2
			}
			reply = r2
		}
		return nil
	})
	if err != nil {
		return err
	}
	persistConversation(c.cfgPath, c.NoPersist, advanceConversation(usedSession, reply.MessageID))
	if c.JSONOut {
		if len(reply.Sources) > 0 {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"sources": reply.Sources})
		}
		return nil
	}
	if last != '\n' && last != 0 {
		fmt.Fprintln(os.Stdout)
	}
	renderSources(os.Stderr, reply.Sources)
	return nil
}
