package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dat267/dscli/internal/deepseek"
	"github.com/dat267/dscli/internal/translate"
)

// ImproveWritingCmd implements `dscli improve-writing`: it runs an already
// written file (typically a translation) back through the model to polish the
// prose in place — fixing grammar, flow and clarity without changing meaning
// or language. The engine is shared with `translate` (format-aware chunking,
// structural-line protection, adaptive sizing); only the prompt and the
// write-back differ.
type ImproveWritingCmd struct {
	File         []string      `arg:"" optional:"" help:"File(s) to improve (txt, md, lrc, srt, vtt, ass, ttml); omit to read from stdin"`
	InPlace      bool          `short:"i" help:"Rewrite each file in place with the improved text. Required: improve-writing replaces the original instead of writing a separate output file"`
	ChunkBytes   int           `help:"Upper bound on chunk size in bytes; 0 uses a 1 MiB cap. Chunks are sized adaptively to fit the model's output limit, so this is a maximum, not a fixed size" default:"0"`
	Instructions string        `help:"File with custom improvement instructions for this run (default: improve-writing/default.md, then a built-in general style)"`
	Glossary     string        `help:"File with a project-specific name/term glossary (appended to every chunk prompt)"`
	Timeout      time.Duration `help:"Overall budget per file (0 = no limit)" default:"15m"`

	Token     string `env:"DS_TOKEN" help:"DeepSeek user token (localStorage.userToken). Alternatively: config set token"`
	Cookie    string `env:"DS_COOKIE" help:"DeepSeek ds_session_id cookie value. Alternatively: config set cookie"`
	UserAgent string `env:"DS_USER_AGENT" help:"Browser user-agent; some deployments reject non-browser UAs"`

	Model string `short:"m" help:"Model: default (Instant) or expert" default:""`

	Thinking bool `short:"t" help:"Enable DeepThink reasoning for each chunk (the reasoning model allows longer replies, so chunks are sized bigger and fewer are needed)"`

	Parallel bool `short:"p" help:"Improve multiple files concurrently (each in its own session). No shared context between files — terminology may drift"`

	NoPersist bool `help:"Do not persist or reuse the default session; the session is deleted when the run ends (default: on)" default:"true"`

	// clientBase overrides the API base URL for tests.
	clientBase string

	// cfgPath is the config file path used for session persistence; set from
	// app in Run.
	cfgPath string
}

func (c *ImproveWritingCmd) Run(app *App, ctx context.Context) error {
	if app != nil {
		c.cfgPath = app.CfgPath()
	}
	if c.Token == "" {
		return errors.New("no DeepSeek session configured: pass --token/--cookie (or DS_TOKEN/DS_COOKIE) or run 'dscli login' and save the values with 'dscli config set'")
	}
	if !c.InPlace {
		return errors.New("use --in-place to rewrite files; improve-writing does not write a separate output file")
	}
	if c.Model != "" && c.Model != "default" && c.Model != "expert" {
		return fmt.Errorf("unknown model %q (want default or expert)", c.Model)
	}
	if len(c.File) == 0 {
		// TODO: stdin reading for single-file mode
		return errors.New("give at least one file to improve")
	}

	style, err := translate.ResolveImproveStyle(c.Instructions)
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

	if c.Parallel {
		return c.improveParallel(ctx, style)
	}
	return c.improveSequential(ctx, style)
}

// improveFile improves one file and rewrites it in place.
func (c *ImproveWritingCmd) improveFile(ctx context.Context, client *deepseek.Client, file, style string) error {
	if strings.EqualFold(filepath.Ext(file), ".epub") {
		return fmt.Errorf("improve-writing --in-place does not support epub (Load returns extracted text, which cannot be written back as a binary epub); improve the extracted .txt instead")
	}

	content, format, err := translate.Load(file, translate.MaxInputBytes)
	if err != nil {
		return fmt.Errorf("load %s: %w", file, err)
	}

	sessionID, trusted, cleanup, err := resolveDefaultSession(ctx, client, c.cfgPath, c.NoPersist)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	fmt.Fprintf(os.Stderr, "improving %s → %s (%s)\n", file, file, format)

	var result, convID string
	_, err = recoverStaleSession(ctx, client, c.cfgPath, sessionID, trusted, func(sid string) error {
		r, cid, e := translate.Translate(ctx, client, sid, content, format, translate.Options{
			Model:      effectiveModel(c.Model),
			ChunkBytes: c.ChunkBytes,
			Style:      style,
			Thinking:   c.Thinking,
			Improve:    true,
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
	if err := os.WriteFile(file, []byte(result), 0644); err != nil {
		return fmt.Errorf("write %s: %w", file, err)
	}
	fmt.Fprintf(os.Stderr, "done → %s (%d bytes)\n", file, len(result))
	return nil
}

// improveSequential improves files one at a time, reusing the session.
func (c *ImproveWritingCmd) improveSequential(ctx context.Context, style string) error {
	client := deepseek.NewClient(deepseek.Session{
		Token:     c.Token,
		Cookie:    c.Cookie,
		UserAgent: c.UserAgent,
	}, c.Timeout, c.clientBase)

	for _, file := range c.File {
		if err := c.improveFile(ctx, client, file, style); err != nil {
			return err
		}
	}
	return nil
}

// improveParallel improves all files concurrently, each in its own session.
func (c *ImproveWritingCmd) improveParallel(ctx context.Context, style string) error {
	type fileResult struct {
		file string
		err  error
	}
	results := make(chan fileResult, len(c.File))

	for _, file := range c.File {
		go func(file string) {
			client := deepseek.NewClient(deepseek.Session{
				Token:     c.Token,
				Cookie:    c.Cookie,
				UserAgent: c.UserAgent,
			}, c.Timeout, c.clientBase)
			err := c.improveFile(ctx, client, file, style)
			results <- fileResult{file, err}
		}(file)
	}

	var firstErr error
	for range c.File {
		r := <-results
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
	}
	return firstErr
}
