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
// internal/translate.
type TranslateCmd struct {
	File         []string      `arg:"" optional:"" help:"File(s) to translate (txt, md, lrc, srt, vtt, ass, ttml, epub); omit to read from stdin"`
	From         string        `help:"Source language (defaults to auto-detect)" default:"auto"`
	To           string        `help:"Target language" default:"English"`
	Output       string        `short:"o" help:"Output path (default: <input>.translated.<ext>, .txt for epub)"`
	Force        bool          `short:"f" help:"Overwrite the output file if it exists"`
	ChunkBytes   int           `help:"Upper bound on chunk size in bytes; 0 uses a 1 MiB cap. Chunks are sized adaptively to fit the model's output limit, so this is a maximum, not a fixed size" default:"0"`
	Instructions string        `help:"File with custom translation instructions for this run (default: translate/<from>-<to>.md, then a built-in general style)"`
	Glossary     string        `help:"File with a project-specific name/term glossary (appended to every chunk prompt)"`
	Timeout      time.Duration `help:"Overall budget per file (0 = no limit)" default:"15m"`

	Token     string `env:"DS_TOKEN" help:"DeepSeek user token (localStorage.userToken). Alternatively: config set token"`
	Cookie    string `env:"DS_COOKIE" help:"DeepSeek ds_session_id cookie value. Alternatively: config set cookie"`
	UserAgent string `env:"DS_USER_AGENT" help:"Browser user-agent; some deployments reject non-browser UAs"`

	Model string `short:"m" help:"Model: default (Instant) or expert" default:""`

	Thinking bool `short:"t" help:"Enable DeepThink reasoning for each chunk (the reasoning model allows longer replies, so chunks are sized bigger and fewer are needed)"`

	Parallel bool `short:"p" help:"Translate multiple files concurrently (each in its own session). No shared context between files — terminology may drift"`

	NoPersist bool `help:"Do not persist or reuse the default session; the session is deleted when the run ends (default: on)" default:"true"`

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
	if c.Output != "" && len(c.File) > 1 {
		return errors.New("-o (output path) cannot be used with multiple files; each file gets its own default path")
	}
	if c.Model != "" && c.Model != "default" && c.Model != "expert" {
		return fmt.Errorf("unknown model %q (want default or expert)", c.Model)
	}
	if len(c.File) == 0 {
		// TODO: stdin reading for single-file mode
		return errors.New("give at least one file to translate")
	}

	style, err := translate.ResolveStyle(c.Instructions, c.From, c.To)
	if err != nil {
		return err
	}
	if c.Glossary != "" {
		gloss, err := os.ReadFile(c.Glossary)
		if err != nil {
			return fmt.Errorf("read glossary: %w", err)
		}
		style = style + "\n\n## Project glossary\n" + string(gloss)
	}

	return runFiles(ctx, c.File, c.Parallel,
		func() *deepseek.Client {
			return deepseek.NewClient(deepseek.Session{
				Token:     c.Token,
				Cookie:    c.Cookie,
				UserAgent: c.UserAgent,
			}, c.Timeout, c.clientBase)
		},
		func(ctx context.Context, client *deepseek.Client, file string) error {
			return c.translateFile(ctx, client, file, style)
		})
}

// translateFile translates one file and writes the output.
func (c *TranslateCmd) translateFile(ctx context.Context, client *deepseek.Client, file, style string) error {
	content, format, err := translate.Load(file, translate.MaxInputBytes)
	if err != nil {
		return fmt.Errorf("load %s: %w", file, err)
	}

	out := c.Output
	if out == "" {
		out = translate.DefaultOutput(file, c.To)
	}
	if !c.Force {
		if _, err := os.Stat(out); err == nil {
			return fmt.Errorf("output %s already exists (use -f to overwrite)", out)
		}
	}

	sessionID, trusted, cleanup, err := resolveDefaultSession(ctx, client, c.cfgPath, c.NoPersist)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	fmt.Fprintf(os.Stderr, "translating %s → %s (%s from %s to %s)\n",
		file, out, format, c.From, c.To)

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
