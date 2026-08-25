package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dat267/dscli/internal/deepseek"
	"github.com/dat267/dscli/internal/translate"
)

// TranslateCmd implements `dscli translate`: a format-aware, chunked file
// translation driven by the DeepSeek session. The heavy lifting lives in
// internal/translate so the CLI command and the translate_file tool call
// share one engine.
type TranslateCmd struct {
	File         string        `arg:"" help:"File to translate (txt, md, lrc, srt, vtt, ass, ttml, epub)"`
	From         string        `help:"Source language (defaults to auto-detect)" default:"auto"`
	To           string        `help:"Target language" default:"English"`
	Output       string        `short:"o" help:"Output path (default: <input>.translated.<ext>, .txt for epub)"`
	Force        bool          `short:"f" help:"Overwrite the output file if it exists"`
	ChunkBytes   int           `help:"Upper bound on chunk size in bytes; 0 uses a 1 MiB cap. Chunks are sized adaptively to fit the model's output limit, so this is a maximum, not a fixed size" default:"0"`
	Instructions string        `help:"File with custom translation instructions for this run (default: translate/<from>-<to>.md, then a built-in general style)"`
	Timeout      time.Duration `help:"Overall budget (0 = no limit)" default:"15m"`

	Token     string `env:"DS_TOKEN" help:"DeepSeek user token (localStorage.userToken). Alternatively: config set token"`
	Cookie    string `env:"DS_COOKIE" help:"DeepSeek ds_session_id cookie value. Alternatively: config set cookie"`
	UserAgent string `env:"DS_USER_AGENT" help:"Browser user-agent; some deployments reject non-browser UAs"`

	Model string `short:"m" help:"Model: default (Instant) or expert" default:""`

	Thinking bool `short:"t" help:"Enable DeepThink reasoning for each chunk (the reasoning model allows longer replies, so chunks are sized bigger and fewer are needed)"`

	NoPersist bool `help:"Do not persist or reuse the default session; the session is deleted when the run ends"`

	// clientBase overrides the API base URL for tests.
	clientBase string

	// cfgPath is the config file path used for session persistence; set from
	// app in Run.
	cfgPath string
}

func (c *TranslateCmd) Run(app *App, ctx context.Context) error {
	if app != nil {
		c.cfgPath = app.CfgPath()
	}
	if c.Token == "" {
		return errors.New("no DeepSeek session configured: pass --token/--cookie (or DS_TOKEN/DS_COOKIE) or run 'dscli login' and save the values with 'dscli config set'")
	}
	if c.Output != "" && !c.Force {
		if _, err := os.Stat(c.Output); err == nil {
			return fmt.Errorf("output file %s already exists (use -f to overwrite)", c.Output)
		}
	}
	if c.Model != "" && c.Model != "default" && c.Model != "expert" {
		return fmt.Errorf("unknown model %q (want default or expert)", c.Model)
	}

	content, format, err := translate.Load(c.File, translate.MaxInputBytes)
	if err != nil {
		return err
	}
	out := c.Output
	if out == "" {
		out = translate.DefaultOutput(c.File, c.To)
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

	style, err := translate.ResolveStyle(c.Instructions, c.From, c.To)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "translating %s → %s (%s from %s to %s)\n",
		c.File, out, format, c.From, c.To)

	var result, convID string
	_, err = recoverStaleSession(ctx, client, c.cfgPath, sessionID, trusted, func(sid string) error {
		r, cid, e := translate.Translate(ctx, client, sid, content, format, translate.Options{
			From:       c.From,
			To:         c.To,
			Model:      effectiveModel(c.Model),
			ChunkBytes: c.ChunkBytes,
			Style:      style,
			Thinking:   c.Thinking,
			OnChunk: func(chunk, total int) {
				fmt.Fprintf(os.Stderr, "  chunk %d/%d ok\n", chunk, total)
			},
		})
		if e == nil {
			result = r
			convID = cid
		}
		return e
	})
	if err != nil {
		return err
	}
	persistConversation(c.cfgPath, c.NoPersist, convID)
	if err := os.WriteFile(out, []byte(result), 0644); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	fmt.Fprintf(os.Stderr, "done → %s (%d bytes)\n", out, len(result))
	return nil
}
