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
	"github.com/dat267/dscli/internal/filetools"
)

// DoCmd implements `dscli do`: a one-shot, tool-calling task runner — like
// `ask`, but with the file tools / fetch_url loop enabled and DeepThink
// reasoning on by default, so the model can inspect and edit files while it
// works. Writes still ask first unless -y auto-approves them, which makes the
// task fully unattended. The session is created per call and deleted
// afterwards, so nothing persists and no conversation-id noise is printed. The
// input may be positional args or piped stdin.
type DoCmd struct {
	Task      []string      `arg:"" optional:"" help:"Task to do; omit to read from stdin (use -- before a task that starts with -)"`
	Model     string        `short:"m" help:"Model: default (Instant) or expert" default:""`
	Thinking  bool          `short:"t" help:"Enable DeepThink reasoning" default:"true"`
	Search    bool          `short:"s" help:"Enable web search"`
	Yes       bool          `short:"y" help:"Assume yes for every file-write confirmation (no prompts)"`
	NoPersist bool          `help:"Do not persist or reuse the default session; the session is deleted when the run ends"`
	JSONOut   bool          `help:"Emit NDJSON: the final answer as {\"delta\":...}, then {\"done\":true}"`
	Timeout   time.Duration `help:"Overall budget (0 = no limit)" default:"15m"`

	Token     string `env:"DS_TOKEN" help:"DeepSeek user token (localStorage.userToken). Alternatively: config set token"`
	Cookie    string `env:"DS_COOKIE" help:"DeepSeek ds_session_id cookie value. Alternatively: config set cookie"`
	UserAgent string `env:"DS_USER_AGENT" help:"Browser user-agent; some deployments reject non-browser UAs"`

	Workdir      string `help:"Working directory for file tools" default:"."`
	MaxRead      int    `help:"Max bytes read_file and fetch_url will return; oversized or binary files/responses are rejected (default: 512 KiB)" default:"0"`
	Instructions string `help:"Custom translation instructions file for translate_file (default: translate/<from>-<to>.md, then a built-in general style)"`

	// clientBase overrides the API base URL for tests.
	clientBase string

	// cfgPath is the config file path used for session persistence; set from
	// app in Run.
	cfgPath string
}

func (c *DoCmd) Run(app *App, ctx context.Context) error {
	if app != nil {
		c.cfgPath = app.CfgPath()
	}
	if c.Token == "" {
		return errors.New("no DeepSeek session configured: pass --token/--cookie (or DS_TOKEN/DS_COOKIE) or run 'dscli login' and save the values with 'dscli config set'")
	}
	if c.Cookie == "" {
		fmt.Fprintln(os.Stderr, "warning: no ds_session_id cookie set; the site may reject requests without it")
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

	task := strings.TrimSpace(strings.Join(c.Task, " "))
	if task == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		task = strings.TrimSpace(string(data))
	}
	if task == "" {
		return errors.New("nothing to do: pass a task or pipe input on stdin")
	}

	client := deepseek.NewClient(deepseek.Session{
		Token:     c.Token,
		Cookie:    c.Cookie,
		UserAgent: c.UserAgent,
	}, c.Timeout, c.clientBase)

	// By default the persisted default session is resumed (created + saved on
	// first use) and kept; --no-persist runs in a fresh session deleted when
	// the task ends.
	sessionID, trusted, cleanup, err := resolveDefaultSession(ctx, client, c.cfgPath, c.NoPersist)
	if err != nil {
		return fmt.Errorf("create chat session: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	chat := &ChatCmd{
		Model:        c.Model,
		Thinking:     c.Thinking,
		Search:       c.Search,
		JSONOut:      c.JSONOut,
		Timeout:      c.Timeout,
		Token:        c.Token,
		Cookie:       c.Cookie,
		UserAgent:    c.UserAgent,
		Workdir:      c.Workdir,
		MaxRead:      c.MaxRead,
		Instructions: c.Instructions,
	}
	if c.Yes {
		chat.confirm = func(string) bool { return true }
	}
	var sources []deepseek.Source
	var convID string
	_, err = recoverStaleSession(ctx, client, c.cfgPath, sessionID, trusted, func(sid string) error {
		cid, e := chat.turn(ctx, client, sid, task, effectiveModel(c.Model), true, func(s string) {
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
		out := map[string]any{"done": true}
		if len(sources) > 0 {
			out["sources"] = sources
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	renderSources(os.Stderr, sources)
	return nil
}
