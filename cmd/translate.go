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
	"github.com/dat267/dscli/internal/filetools"
)

// maxTranslateBytes caps the total input size a translate job reads locally.
const maxTranslateBytes = 64 << 20 // 64 MiB

// defaultChunkBytes is the approximate per-request chunk size: small enough
// to stay comfortably inside the model context per turn, large enough to keep
// translation context coherent across lines.
const defaultChunkBytes = 24 * 1024

// TranslateCmd implements `dscli translate`: a format-aware, chunked file
// translation driven by the DeepSeek session.
type TranslateCmd struct {
	File       string        `arg:"" help:"File to translate (txt, md, lrc, srt, vtt, ass, ttml, epub)"`
	From       string        `help:"Source language (defaults to auto-detect)" default:"auto"`
	To         string        `help:"Target language" default:"English"`
	Output     string        `short:"o" help:"Output path (default: <input>.translated.<ext>, .txt for epub)"`
	Force      bool          `short:"f" help:"Overwrite the output file if it exists"`
	ChunkBytes int           `help:"Approximate chunk size in bytes (line boundaries preserved)" default:"24576"`
	Timeout    time.Duration `help:"Overall budget (0 = no limit)" default:"15m"`

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
	if c.ChunkBytes <= 0 {
		c.ChunkBytes = defaultChunkBytes
	}
	if c.Model != "" && c.Model != "default" && c.Model != "expert" {
		return fmt.Errorf("unknown model %q (want default or expert)", c.Model)
	}

	content, format, err := readInput(c.File)
	if err != nil {
		return err
	}
	out := c.Output
	if out == "" {
		out = defaultOutput(c.File)
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

	chunks := chunkText(string(content), c.ChunkBytes)
	fmt.Fprintf(os.Stderr, "translating %s → %s (%s, %d chunks from %s to %s)\n",
		c.File, out, format, len(chunks), c.From, c.To)

	var translated []string
	conversation := sessionID
	for i, chunk := range chunks {
		prompt := translatePrompt(format, c.From, c.To, false) + chunk
		text, convID, err := c.translateChunk(ctx, client, conversation, prompt)
		if err != nil {
			return fmt.Errorf("chunk %d/%d: %w", i+1, len(chunks), err)
		}
		conversation = convID

		// Structural formats must keep their timestamps/header markup
		// byte-for-byte (ProtectedLines is empty for text/markdown, so the
		// verification passes trivially there).
		if err := filetools.VerifyProtected(format, chunk, text); err != nil {
			strict := translatePrompt(format, c.From, c.To, true) + "The previous attempt changed a protected (timestamps/header) line.\n" +
				"Keep every line with a timestamp or the WEBVTT/header syntax EXACTLY as in the original. Retry the chunk:\n\n" + chunk
			text2, convID2, err2 := c.translateChunk(ctx, client, conversation, strict)
			if err2 != nil {
				return fmt.Errorf("chunk %d/%d: %w", i+1, len(chunks), err2)
			}
			conversation = convID2
			if err := filetools.VerifyProtected(format, chunk, text2); err != nil {
				return fmt.Errorf("chunk %d/%d: %w", i+1, len(chunks), err)
			}
			text = text2
		}
		translated = append(translated, text)
		fmt.Fprintf(os.Stderr, "  chunk %d/%d ok\n", i+1, len(chunks))
	}

	result := strings.TrimSpace(strings.Join(translated, "\n")) + "\n"
	if err := os.WriteFile(out, []byte(result), 0644); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	fmt.Fprintf(os.Stderr, "done → %s (%d bytes)\n", out, len(result))
	return nil
}

// modelType is "default" on the first turn of the thread; the model is fixed
// per thread, so resuming chunks omit it.
func (c *TranslateCmd) translateChunk(ctx context.Context, client *deepseek.Client, conversation, prompt string) (text, convID string, err error) {
	sessionID, parentID := splitConversation(conversation)
	modelType := ""
	if parentID == nil {
		modelType = effectiveModel(c.Model)
	}
	var buf strings.Builder
	reply, err := client.StreamCompletion(ctx, deepseek.CompletionRequest{
		ChatSessionID:   sessionID,
		ParentMessageID: parentID,
		Prompt:          prompt,
		ModelType:       modelType,
		ThinkingEnabled: false,
		SearchEnabled:   false,
	}, func(d string) error {
		buf.WriteString(d)
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(buf.String()), conversationID(sessionID, parentID, reply.MessageID), nil
}

// readInput loads and classifies the input file: plain text (txt/md/lrc/vtt)
// or EPUB text extraction.
func readInput(path string) ([]byte, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read input: %w", err)
	}
	if int64(len(data)) > maxTranslateBytes {
		return nil, "", fmt.Errorf("input is %d bytes, over the %d byte translate limit", len(data), maxTranslateBytes)
	}
	format := filetools.DetectFormat(path, data)
	if strings.EqualFold(filepath.Ext(path), ".epub") {
		text, err := filetools.ReadEpub(path)
		if err != nil {
			return nil, "", err
		}
		return []byte(text), "text", nil
	}
	if !filetools.IsText(data) {
		return nil, "", fmt.Errorf("input is not a text file; translate supports txt/md/lrc/vtt (and epub)")
	}
	return data, format, nil
}

// defaultOutput derives the output path: <base>.translated.<ext>, or
// <base>.translated.txt for EPUB books.
func defaultOutput(input string) string {
	ext := filepath.Ext(input)
	base := strings.TrimSuffix(input, ext)
	if strings.EqualFold(ext, ".epub") {
		return base + ".translated.txt"
	}
	return base + ".translated" + ext
}

// chunkText splits text into chunks of roughly approxBytes, keeping lines
// intact.
func chunkText(text string, approxBytes int) []string {
	if approxBytes <= 0 {
		approxBytes = defaultChunkBytes
	}
	endsWithNewline := strings.HasSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	var chunks []string
	var cur []string
	curBytes := 0
	for i, line := range lines {
		l := line
		if i < len(lines)-1 || endsWithNewline {
			l += "\n"
		}
		if curBytes > 0 && curBytes+len(l) > approxBytes {
			chunks = append(chunks, strings.Join(cur, ""))
			cur, curBytes = nil, 0
		}
		cur = append(cur, l)
		curBytes += len(l)
	}
	if len(cur) > 0 {
		chunks = append(chunks, strings.Join(cur, ""))
	}
	if len(chunks) == 0 {
		chunks = []string{""}
	}
	return chunks
}

// translatePrompt builds the format-aware instruction for one chunk.
// reminder adds the strict structural rules for the verification retry.
func translatePrompt(format, from, to string, reminder bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Translate the following %s from %s to %s.\n", formatName(format), from, to)
	switch format {
	case "lrc":
		b.WriteString("This is an LRC lyrics file. Translate ONLY the lyric text after the [mm:ss.xx] timestamps; keep every timestamp and the [ti:][ar:][al:] metadata tags EXACTLY as they are (character for character). Never merge, drop or alter timestamp lines.\n")
	case "srt":
		b.WriteString("This is an SRT subtitle file. Translate ONLY the cue text lines; keep the cue index numbers and every 'HH:MM:SS,mmm --> HH:MM:SS,mmm' timing line EXACTLY as they are. Never merge, drop or alter timing lines.\n")
	case "vtt":
		b.WriteString("This is a WebVTT subtitle file. Translate ONLY the cue text lines; keep the WEBVTT header, NOTE blocks, cue identifiers and every 'hh:mm:ss.mmm --> hh:mm:ss.mmm' timing line EXACTLY as they are. Never merge, drop or alter timing lines.\n")
	case "ass":
		b.WriteString("This is an ASS/SSA subtitle file. Translate ONLY the text after the 9th comma (the text field) of each Dialogue:/Comment: line. Keep ALL other lines (Script Info, Style headers, Format:, Style:) and every Dialogue: line's prefix fields (layer, start, end, style, name, margins, effect) EXACTLY as they are — byte for byte, including punctuation. Keep \\N and \\h override tags inside the translated text.\n")
	case "ttml":
		b.WriteString("This is a TTML XML subtitle file. Translate ONLY the text content inside the <p> elements. Keep every XML tag and its attributes (begin/end/dur etc.) EXACTLY as they are; never add, remove or reorder elements; the result must remain valid XML.\n")
	case "markdown":
		b.WriteString("This is a Markdown file. Translate the prose; keep code blocks, URLs, link/image syntax, heading and list markers intact (translate their visible text where appropriate).\n")
	default:
		b.WriteString("Plain text.\n")
	}
	if reminder {
		b.WriteString("TRANSLATION VERIFICATION FAILED LAST TIME because structural lines were altered. They must stay byte-for-byte identical.\n")
	}
	b.WriteString("Reply with ONLY the translated content — no preamble, no commentary, no code fences.\n\n")
	return b.String()
}

func formatName(format string) string {
	switch format {
	case "lrc":
		return "LRC lyrics"
	case "srt":
		return "SRT subtitles"
	case "vtt":
		return "WebVTT subtitles"
	case "ass":
		return "ASS/SSA subtitles"
	case "ttml":
		return "TTML subtitles"
	case "markdown":
		return "Markdown document"
	}
	return "text file"
}
