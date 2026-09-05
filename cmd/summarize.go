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

// SummarizeCmd implements `dscli summarize`: a format-aware, chunked summary
// of a file, printed to stdout (or saved with -o). It shares the translation
// engine (adaptive chunking, style files); each chunk is summarized, and when
// the document needed more than one chunk the per-chunk summaries are
// combined into one final summary.
type SummarizeCmd struct {
	File         []string      `arg:"" optional:"" help:"File(s) to summarize (txt, md, lrc, srt, vtt, ass, ttml, epub); omit to read from stdin"`
	Output       string        `short:"o" help:"Output path (default: print to stdout)"`
	Force        bool          `short:"f" help:"Overwrite the output file if it exists"`
	ChunkBytes   int           `help:"Upper bound on chunk size in bytes; 0 uses a 1 MiB cap. Chunks are sized adaptively to fit the model's output limit, so this is a maximum, not a fixed size" default:"0"`
	Instructions string        `help:"File with custom summarization instructions for this run (default: summarize/default.md, then a built-in general style)"`
	Timeout      time.Duration `help:"Overall budget per file (0 = no limit)" default:"15m"`

	Token     string `env:"DS_TOKEN" help:"DeepSeek user token (localStorage.userToken). Alternatively: config set token"`
	Cookie    string `env:"DS_COOKIE" help:"DeepSeek ds_session_id cookie value. Alternatively: config set cookie"`
	UserAgent string `env:"DS_USER_AGENT" help:"Browser user-agent; some deployments reject non-browser UAs"`

	Model string `short:"m" help:"Model: default (Instant) or expert" default:""`

	Thinking bool `short:"t" help:"Enable DeepThink reasoning for each chunk (the reasoning model allows longer replies, so chunks are sized bigger and fewer are needed)"`

	Parallel bool `short:"p" help:"Summarize multiple files concurrently (each in its own session). Summaries print as they finish, so they may interleave"`

	NoPersist bool `help:"Do not persist or reuse the default session; the session is deleted when the run ends (default: on)" default:"true"`

	// clientBase overrides the API base URL for tests.
	clientBase string

	// cfgPath is the config file path used for session persistence; set from
	// app in Run.
	cfgPath string
}

func (c *SummarizeCmd) Run(app *App, ctx context.Context) error {
	if app != nil {
		c.cfgPath = app.CfgPath()
	}
	if c.Token == "" {
		return errors.New("no DeepSeek session configured: pass --token/--cookie (or DS_TOKEN/DS_COOKIE) or run 'dscli login' and save the values with 'dscli config set'")
	}
	if c.Output != "" && len(c.File) > 1 {
		return errors.New("-o (output path) cannot be used with multiple files; each file gets its own summary on stdout")
	}
	if c.Model != "" && c.Model != "default" && c.Model != "expert" {
		return fmt.Errorf("unknown model %q (want default or expert)", c.Model)
	}
	if len(c.File) == 0 {
		// TODO: stdin reading for single-file mode
		return errors.New("give at least one file to summarize")
	}

	style, err := translate.ResolveSummarizeStyle(c.Instructions)
	if err != nil {
		return err
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
			return c.summarizeFile(ctx, client, file, style)
		})
}

// summarizeFile summarizes one file and prints (or writes) the result.
func (c *SummarizeCmd) summarizeFile(ctx context.Context, client *deepseek.Client, file, style string) error {
	content, format, err := translate.Load(file, translate.MaxInputBytes)
	if err != nil {
		return fmt.Errorf("load %s: %w", file, err)
	}

	if c.Output != "" && !c.Force {
		if _, err := os.Stat(c.Output); err == nil {
			return fmt.Errorf("output %s already exists (use -f to overwrite)", c.Output)
		}
	}

	sessionID, trusted, cleanup, err := resolveDefaultSession(ctx, client, c.cfgPath, c.NoPersist)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	if c.Output != "" {
		fmt.Fprintf(os.Stderr, "summarizing %s → %s (%s)\n", file, c.Output, format)
	} else {
		fmt.Fprintf(os.Stderr, "summarizing %s (%s)\n", file, format)
	}

	var result, convID string
	_, err = recoverStaleSession(ctx, client, c.cfgPath, sessionID, trusted, func(sid string) error {
		r, cid, e := translate.Summarize(ctx, client, sid, content, format, translate.Options{
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

	if c.Output != "" {
		if err := os.WriteFile(c.Output, []byte(result), 0644); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
		fmt.Fprintf(os.Stderr, "done → %s (%d bytes)\n", c.Output, len(result))
		return nil
	}
	fmt.Print(result)
	return nil
}
