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
	ChunkBytes   int           `help:"Approximate chunk size in bytes (line boundaries preserved)" default:"24576"`
	Instructions string        `help:"File with custom translation instructions for this run (default: translate/<from>-<to>.md, then a built-in general style)"`
	Timeout      time.Duration `help:"Overall budget (0 = no limit)" default:"15m"`

	Token     string `env:"DS_TOKEN" help:"DeepSeek user token (localStorage.userToken). Alternatively: config set token"`
	Cookie    string `env:"DS_COOKIE" help:"DeepSeek ds_session_id cookie value. Alternatively: config set cookie"`
	UserAgent string `env:"DS_USER_AGENT" help:"Browser user-agent; some deployments reject non-browser UAs"`

	Model string `short:"m" help:"Model: default (Instant) or expert" default:""`

	// clientBase overrides the API base URL for tests.
	clientBase string
}

func (c *TranslateCmd) Run(app *App, ctx context.Context) error {
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
		out = translate.DefaultOutput(c.File)
	}

	client := deepseek.NewClient(deepseek.Session{
		Token:     c.Token,
		Cookie:    c.Cookie,
		UserAgent: c.UserAgent,
	}, c.Timeout, c.clientBase)

	sessionID, err := client.CreateChatSession(ctx)
	if err != nil {
		return fmt.Errorf("create chat session: %w", err)
	}
	defer func() {
		if err := client.DeleteSessions(context.Background(), []string{sessionID}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to delete session: %v\n", err)
		}
	}()

	style, err := translate.ResolveStyle(c.Instructions, c.From, c.To)
	if err != nil {
		return err
	}
	chunks := translate.ChunkText(string(content), c.ChunkBytes)
	fmt.Fprintf(os.Stderr, "translating %s → %s (%s, %d chunks from %s to %s)\n",
		c.File, out, format, len(chunks), c.From, c.To)

	result, err := translate.Translate(ctx, client, sessionID, content, format, translate.Options{
		From:       c.From,
		To:         c.To,
		Model:      effectiveModel(c.Model),
		ChunkBytes: c.ChunkBytes,
		Style:      style,
		OnChunk: func(chunk, total int) {
			fmt.Fprintf(os.Stderr, "  chunk %d/%d ok\n", chunk, total)
		},
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, []byte(result), 0644); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	fmt.Fprintf(os.Stderr, "done → %s (%d bytes)\n", out, len(result))
	return nil
}
