// Package translate implements the chunked, format-aware translation engine
// shared by the `dscli translate` command and the translate_file tool call,
// so the command and the chat mode can never drift apart.
package translate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dat267/dscli/internal/deepseek"
	"github.com/dat267/dscli/internal/filetools"
)

const (
	// MaxInputBytes caps a full translate job on the CLI.
	MaxInputBytes = 64 << 20 // 64 MiB
	// MaxChatInputBytes caps what the chat translate_file tool accepts —
	// one tool call is a deliberately long unit, so keep it sane.
	MaxChatInputBytes = 1 << 20 // 1 MiB
	// DefaultChunkBytes is the approximate per-request chunk size: small
	// enough to stay comfortably inside the model context per turn, large
	// enough to keep translation context coherent across lines.
	DefaultChunkBytes = 24 * 1024
)

// Options configures one translation run over an existing session.
type Options struct {
	From       string // source language label ("" → "auto")
	To         string // target language label ("" → "English")
	Model      string // "" → "default"
	ChunkBytes int    // <= 0 → DefaultChunkBytes
	// Style is custom per-pair translation instructions appended to every
	// chunk prompt (see ResolveStyle).
	Style string
	// OnChunk, when set, reports progress: 1-based chunk index and total.
	OnChunk func(chunk, total int)
}

// Load reads and classifies an input file: plain text (txt/md/lrc/srt/vtt/
// ass/ttml) or EPUB text extraction. maxBytes caps the accepted input size.
func Load(path string, maxBytes int64) ([]byte, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read input: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("input is %d bytes, over the %d byte translate limit", len(data), maxBytes)
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
		return nil, "", fmt.Errorf("input is not a text file; translate supports txt/md/lrc/srt/vtt/ass/ttml (and epub)")
	}
	return data, format, nil
}

// DefaultOutput derives the output path using the i18n name-coding
// convention: <base>.translated.<lang>.<ext>, e.g.
// chapter-012.translated.en.md — original and translation share the base
// name, so any tool can group the pair and multiple target languages can
// coexist in one directory. An existing ".translated[.<lang>]" suffix in the
// input is stripped first, so re-translating a translation never stacks
// suffixes (a.translated.en.md → a.translated.zh.md). EPUB books default to
// a .txt output.
func DefaultOutput(input, toLabel string) string {
	ext := filepath.Ext(input)
	base := strings.TrimSuffix(input, ext)
	base = translatedSuffixRE.ReplaceAllString(base, "")
	if strings.EqualFold(ext, ".epub") {
		ext = ".txt"
	}
	code := LangCode(toLabel)
	if code == "" {
		return base + ".translated" + ext
	}
	return base + ".translated." + code + ext
}

// translatedSuffixRE matches an existing ".translated" or
// ".translated.<lang>" suffix for stripping before re-naming.
var translatedSuffixRE = regexp.MustCompile(`\.translated(\.[a-z0-9]{1,8})?$`)

// langCodes maps common target-language labels to ISO 639-1 codes used in
// output file names.
var langCodes = map[string]string{
	"english": "en", "japanese": "ja", "chinese": "zh", "korean": "ko",
	"spanish": "es", "french": "fr", "german": "de", "italian": "it",
	"portuguese": "pt", "russian": "ru", "arabic": "ar", "hindi": "hi",
	"thai": "th", "vietnamese": "vi", "indonesian": "id", "dutch": "nl",
	"polish": "pl", "turkish": "tr", "ukrainian": "uk", "romanian": "ro",
}

// LangCode returns the code for a target-language label: ISO 639-1 when
// known, else the lowercased alphanumeric label ("English" → "en",
// "日本語" → "").
func LangCode(label string) string {
	key := pairKey(label)
	if c, ok := langCodes[key]; ok {
		return c
	}
	return key
}

// ChunkText splits text into chunks of roughly approxBytes, keeping lines
// intact and preserving the source bytes exactly (the final newline, or its
// absence, is reproduced).
func ChunkText(text string, approxBytes int) []string {
	if approxBytes <= 0 {
		approxBytes = DefaultChunkBytes
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

// Prompt builds the format-aware instruction for one chunk. reminder adds
// the strict structural rules for the verification retry.
func Prompt(format, from, to string, reminder bool, style string) string {
	if from == "" {
		from = "auto"
	}
	if to == "" {
		to = "English"
	}
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
	if style != "" {
		b.WriteString(style)
		if !strings.HasSuffix(style, "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString("Reply with ONLY the translated content — no preamble, no commentary, no code fences.\n\n")
	return b.String()
}

// Translate runs the chunked translation over sessionID (which must already
// exist), carrying the conversation id turn by turn. Structural formats are
// verified per chunk and retried once when corrupted. Returns the assembled
// translated text, always newline-terminated.
func Translate(ctx context.Context, client *deepseek.Client, sessionID string, content []byte, format string, opts Options) (string, error) {
	chunkBytes := opts.ChunkBytes
	if chunkBytes <= 0 {
		chunkBytes = DefaultChunkBytes
	}
	model := opts.Model
	if model == "" {
		model = "default"
	}
	chunks := ChunkText(string(content), chunkBytes)

	conversation := sessionID
	var translated []string
	for i, chunk := range chunks {
		prompt := Prompt(format, opts.From, opts.To, false, opts.Style) + chunk
		text, convID, err := translateChunk(ctx, client, conversation, prompt, model)
		if err != nil {
			return "", fmt.Errorf("chunk %d/%d: %w", i+1, len(chunks), err)
		}
		conversation = convID

		// Structural formats must keep their timestamps/header markup
		// byte-for-byte (ProtectedLines is empty for text/markdown, so the
		// verification passes trivially there).
		if err := filetools.VerifyProtected(format, chunk, text); err != nil {
			strict := Prompt(format, opts.From, opts.To, true, opts.Style) +
				"The previous attempt changed a protected (timestamps/header) line.\n" +
				"Keep every line with a timestamp or the WEBVTT/header syntax EXACTLY as in the original. Retry the chunk:\n\n" + chunk
			text2, convID2, err2 := translateChunk(ctx, client, conversation, strict, model)
			if err2 != nil {
				return "", fmt.Errorf("chunk %d/%d: %w", i+1, len(chunks), err2)
			}
			conversation = convID2
			if err := filetools.VerifyProtected(format, chunk, text2); err != nil {
				return "", fmt.Errorf("chunk %d/%d: %w", i+1, len(chunks), err)
			}
			text = text2
		}
		translated = append(translated, text)
		if opts.OnChunk != nil {
			opts.OnChunk(i+1, len(chunks))
		}
	}
	return strings.TrimSpace(strings.Join(translated, "\n")) + "\n", nil
}

// translateChunk sends one chunk in the session thread and returns the
// translated text plus the conversation id for the next chunk.
func translateChunk(ctx context.Context, client *deepseek.Client, conversation, prompt, model string) (text, convID string, err error) {
	sessionID := conversation
	parentID := int64(-1) // sentinel: no parent
	if i := strings.IndexByte(conversation, ':'); i >= 0 {
		sessionID = conversation[:i]
		if n, perr := strconv.ParseInt(conversation[i+1:], 10, 64); perr == nil {
			parentID = n
		} else {
			parentID = -1
		}
	}
	target := parentID
	var modelType string
	if parentID < 0 {
		target = -1
		modelType = model
	}
	var parent *int64
	if target >= 0 {
		parent = &target
	}

	var buf strings.Builder
	reply, err := client.StreamCompletion(ctx, deepseek.CompletionRequest{
		ChatSessionID:   sessionID,
		ParentMessageID: parent,
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
	next := sessionID
	if reply.MessageID != 0 {
		next = fmt.Sprintf("%s:%d", sessionID, reply.MessageID)
	} else if parent != nil {
		next = fmt.Sprintf("%s:%d", sessionID, *parent)
	}
	return strings.TrimSpace(buf.String()), next, nil
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

// DefaultStyle is the built-in, pair-agnostic instruction set: a
// generalisation of the Japanese→English principles that apply to any
// source/target pair.
func DefaultStyle() string {
	return `[STYLE: GENERAL]
Translate into natural, unambiguous, and contextually accurate target-language text.
1. Resolve dropped or ambiguous subjects/objects from context; never default to "he" or random pronouns.
2. If true ambiguity remains, stay faithful or append a bracketed [TN: ...] note rather than guessing.
3. Prefer active voice; keep the passive only when the agent is unknown or the focus must stay on the receiver.
4. Match the source's register: honorifics, casual speech and business distance map to equivalent constructions in the target language.
5. Beware false friends, coined loanwords and direct calques — translate meaning, not literal form.
6. Avoid mechanical connectors; vary transitions or omit them when the logical flow is clear.
7. Follow the source's structure: headings, lists, blank lines, scene breaks and unlocalisable lines (URLs, tags) preserved exactly.
8. Keep names and terminology consistent throughout the document.
9. Output only the translation — no commentary, no meta text, no code fences.
`
}

// styleDirs lists directories searched for per-pair instruction files,
// in order: <pair>.md, then default.md. Overridable in tests.
var styleDirs = func() []string {
	dirs := []string{"translate"}
	if cd, err := os.UserConfigDir(); err == nil {
		dirs = append(dirs, filepath.Join(cd, "dscli", "translate"))
	}
	return dirs
}()

// pairKey normalises a language label for file lookup: lowercase, letters and
// digits only ("Japanese" → "japanese", "ja" → "ja").
func pairKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// FindStyleFile searches styleDirs for <pair>.md and default.md.
func FindStyleFile(from, to string) string {
	key := pairKey(from) + "-" + pairKey(to)
	for _, dir := range styleDirs {
		for _, name := range []string{key + ".md", "default.md"} {
			p := filepath.Join(dir, name)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// ResolveStyle returns the instructions for a pair: an explicit file when
// given, else a discovered per-pair file, else the built-in default.
func ResolveStyle(explicit, from, to string) (string, error) {
	if explicit != "" {
		data, err := os.ReadFile(explicit)
		if err != nil {
			return "", fmt.Errorf("read instructions file: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	if p := FindStyleFile(from, to); p != "" {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("read instructions file: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return strings.TrimSpace(DefaultStyle()), nil
}
