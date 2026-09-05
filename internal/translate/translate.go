// Package translate implements the chunked, format-aware translation engine
// behind the `dscli translate` command.
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
	// MaxContextTokens is the model's maximum context window (1M tokens).
	// The site's long-text mode slides/truncates older history, so each
	// chunk can use the full window.
	MaxContextTokens = 1_000_000
	// bytesPerToken is conservative: English ≈ 4 chars/token, CJK ≈ 1–1.5
	// tokens/char at ~3 bytes/char, so 4 bytes/token never underestimates
	// the token count of typical text.
	bytesPerToken = 4
	// DefaultChunkBytes is the UPPER BOUND on a single chunk (1 MiB) for
	// non-think mode. The binding constraint on translation is the model's
	// per-response OUTPUT limit — the site cuts replies around 36 KiB (~9k
	// tokens) and flags them INCOMPLETE — so the engine does not just slice
	// the file at this size. Instead it probes a small first chunk, learns the
	// real output/input byte ratio, and sizes the rest to fill the output
	// budget (see Translate). In think mode, chunking uses a fixed 256 KB
	// input size (see thinkingChunkBytes) to match the expert model's ~300K
	// output token limit.
	DefaultChunkBytes = 1 << 20 // 1 MiB
	// initialChunkBytes is the probe chunk: small enough to almost always fit
	// within the output budget even for verbose models, so the engine can
	// learn the output density before sizing chunks up.
	initialChunkBytes = 8 * 1024
	// minChunkBytes floors how small a chunk can get before giving up.
	minChunkBytes = 1024
	// defaultCapBytes is the model's per-reply output cap used until a real
	// truncation is observed (the site cuts replies around 36 KiB).
	defaultCapBytes = 36 * 1024
	// thinkingCapBytes is the per-reply output cap assumed when DeepThink
	// reasoning is enabled, used until a real truncation is observed. The
	// expert model's output limit is ~300K tokens (~1.2 MiB at 4 bytes/token).
	// The real cap is still learned from the first truncation either way.
	thinkingCapBytes = 300_000 * bytesPerToken // 1.2 MiB
	// thinkingChunkBytes is the fixed input chunk size used when DeepThink is
	// enabled: the reasoning model handles long output, so chunks can be
	// large (bounded by --chunk-bytes); the truncation shrink still guards
	// against a reply that is cut off.
	thinkingChunkBytes = 256 * 1024 // 256 KB
	// outputCapMargin keeps a sized chunk's expected output safely short of
	// the cap, so it stops generating before the cut-off point.
	outputCapMargin = 0.85
)

// Task values for Options.Task, selecting what the model does with each
// chunk.
const (
	// TaskTranslate is the default: translate from From to To.
	TaskTranslate = ""
	// TaskImprove polishes the writing in place, preserving language.
	TaskImprove = "improve"
	// TaskSummarize condenses the text into a summary.
	TaskSummarize = "summarize"
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
	// Thinking enables DeepThink reasoning for each chunk. It does not raise
	// the per-reply output budget (the site still cuts replies around 36 KiB),
	// but some prefer it for complex prose.
	Thinking bool
	// Task selects the per-chunk instruction: TaskTranslate (the default)
	// translates, TaskImprove polishes the writing in place, TaskSummarize
	// condenses the text. Improve/summarize ignore From and To.
	Task string
	// OnChunk, when set, reports progress: chunks done and a live estimate
	// of the total (the total changes as the engine re-sizes chunks).
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

// FirstChunk splits off the first chunk of text (roughly approxBytes),
// keeping lines intact and preserving the source bytes exactly (the final
// newline, or its absence, is reproduced). A single line longer than
// approxBytes is hard-split into pieces so files with few or no newlines
// still chunk — otherwise the whole line becomes one oversized request whose
// output gets cut off. Returns "" only when text is empty.
func FirstChunk(text string, approxBytes int) string {
	if approxBytes <= 0 {
		approxBytes = DefaultChunkBytes
	}
	if approxBytes < 1 {
		approxBytes = 1
	}
	endsWithNewline := strings.HasSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	var b strings.Builder
	curBytes := 0
	for i, line := range lines {
		l := line
		if i < len(lines)-1 || endsWithNewline {
			l += "\n"
		}
		for len(l) > approxBytes {
			if curBytes > 0 {
				return b.String()
			}
			b.WriteString(l[:approxBytes])
			return b.String()
		}
		if curBytes > 0 && curBytes+len(l) > approxBytes {
			return b.String()
		}
		b.WriteString(l)
		curBytes += len(l)
		if curBytes >= approxBytes {
			return b.String()
		}
	}
	if curBytes > 0 {
		return b.String()
	}
	return ""
}

// ChunkText splits text into chunks of roughly approxBytes, keeping lines
// intact and preserving the source bytes exactly (the final newline, or its
// absence, is reproduced). A single line longer than approxBytes is
// hard-split into pieces so files with few or no newlines still chunk —
// otherwise the whole line becomes one oversized request whose output gets cut
// off.
func ChunkText(text string, approxBytes int) []string {
	var chunks []string
	for {
		c := FirstChunk(text, approxBytes)
		if c == "" {
			break
		}
		chunks = append(chunks, c)
		text = text[len(c):]
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
	b.WriteString(formatPreserveRules(format))
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

// formatPreserveRules returns the format-specific instruction telling the model
// which structural lines (timestamps, headers, markup) must stay byte-for-byte
// identical. Shared by the translation and improvement prompts.
func formatPreserveRules(format string) string {
	switch format {
	case "lrc":
		return "This is an LRC lyrics file. Preserve every timestamp and the [ti:][ar:][al:] metadata tags EXACTLY as they are (character for character). Never merge, drop or alter timestamp lines.\n"
	case "srt":
		return "This is an SRT subtitle file. Preserve the cue index numbers and every 'HH:MM:SS,mmm --> HH:MM:SS,mmm' timing line EXACTLY as they are. Never merge, drop or alter timing lines.\n"
	case "vtt":
		return "This is a WebVTT subtitle file. Preserve the WEBVTT header, NOTE blocks, cue identifiers and every 'hh:mm:ss.mmm --> hh:mm:ss.mmm' timing line EXACTLY as they are. Never merge, drop or alter timing lines.\n"
	case "ass":
		return "This is an ASS/SSA subtitle file. Preserve ALL other lines (Script Info, Style headers, Format:, Style:) and every Dialogue: line's prefix fields (layer, start, end, style, name, margins, effect) EXACTLY as they are — byte for byte, including punctuation. Keep \\N and \\h override tags inside the text.\n"
	case "ttml":
		return "This is a TTML XML subtitle file. Preserve every XML tag and its attributes (begin/end/dur etc.) EXACTLY as they are; never add, remove or reorder elements; the result must remain valid XML.\n"
	case "markdown":
		return "This is a Markdown file. Preserve code blocks, URLs, link/image syntax, heading and list markers; change only the visible prose text.\n"
	default:
		return "Plain text.\n"
	}
}

// improvePrompt builds the per-chunk instruction for writing improvement: fix
// grammar/flow/clarity while preserving meaning, tone, register, facts and
// names, and never translate. It reuses formatPreserveRules so structural
// formats keep their timestamps/markup intact.
func improvePrompt(format string, reminder bool, style string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Improve the writing of the following %s. Fix grammar, spelling, punctuation, clarity, flow and word choice; preserve meaning, tone, register, facts and names. Do not translate.\n", formatName(format))
	b.WriteString(formatPreserveRules(format))
	if reminder {
		b.WriteString("IMPROVEMENT VERIFICATION FAILED LAST TIME because structural lines were altered. They must stay byte-for-byte identical.\n")
	}
	if style != "" {
		b.WriteString(style)
		if !strings.HasSuffix(style, "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString("Reply with ONLY the improved content — no preamble, no commentary, no code fences.\n\n")
	return b.String()
}

// summarizePrompt builds the per-chunk instruction for summarization: capture
// the main points concisely, without translating, rewriting or reproducing the
// file's structural markup. It deliberately omits formatPreserveRules — a
// summary never reproduces the structural lines, so the chunk engine skips
// structural verification for this task (see Translate).
func summarizePrompt(format string, reminder bool, style string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Summarize the following %s. Capture the main points, events, names and conclusions; be concise; do not translate or rewrite the text.\n", formatName(format))
	if reminder {
		b.WriteString("THE PREVIOUS ATTEMPT FAILED. Retry the chunk.\n")
	}
	if style != "" {
		b.WriteString(style)
		if !strings.HasSuffix(style, "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString("Reply with ONLY the summary — no preamble, no commentary, no code fences.\n\n")
	return b.String()
}

// Translate runs the chunked translation over sessionID (which must already
// exist), carrying the conversation id turn by turn. Structural formats are
// verified per chunk and retried once when corrupted. Returns the assembled
// translated text (always newline-terminated), the final conversation id
// (session:message) for resuming the thread, and any error.
//
// Chunk sizes are adaptive: the binding limit is the model's per-response
// OUTPUT budget (the site cuts replies around 36 KiB and flags them
// INCOMPLETE), so a fixed chunk size either cuts off or wastes capacity. The
// engine probes a small first chunk, learns the real output/input byte ratio,
// then sizes the remaining chunks to fill the output budget — and shrinks the
// size whenever a reply is still truncated. Every completed chunk is kept, so
// nothing is discarded and re-translated.
func Translate(ctx context.Context, client *deepseek.Client, sessionID string, content []byte, format string, opts Options) (string, string, error) {
	maxChunk := opts.ChunkBytes
	if maxChunk <= 0 {
		maxChunk = DefaultChunkBytes
	}
	model := opts.Model
	if model == "" {
		model = "default"
	}
	src := string(content)

	conversation := sessionID
	var translated []string

	// promptFor builds the per-chunk instruction. In improve-writing mode it
	// preserves meaning and tone instead of switching language, and From/To
	// are ignored.
	promptFor := func(reminder bool) string {
		switch opts.Task {
		case TaskImprove:
			return improvePrompt(format, reminder, opts.Style)
		case TaskSummarize:
			return summarizePrompt(format, reminder, opts.Style)
		default:
			return Prompt(format, opts.From, opts.To, reminder, opts.Style)
		}
	}

	// The chunk-sizing strategy is the only difference between the modes:
	// adaptive probe/grow/shrink for Instant, one fixed size for DeepThink.
	var sizer chunkSizer
	if opts.Thinking {
		sizer = newFixedSizer(maxChunk)
	} else {
		sizer = newAdaptiveSizer(maxChunk)
	}

	offset := 0
	for offset < len(src) {
		chunk := FirstChunk(src[offset:], sizer.size())
		if chunk == "" {
			break
		}
		text, convID, truncated, err := translateChunk(ctx, client, conversation, promptFor(false)+chunk, model, opts.Thinking)
		if err != nil {
			if !truncated {
				return "", conversation, fmt.Errorf("chunk (%d bytes): %w", len(chunk), err)
			}
			// Reply hit the output limit: learn from the partial text, shrink
			// the chunk, and re-split the remaining text from this offset.
			// The sizer gives up (false) at the minimum chunk size.
			if !sizer.truncated(len(text)) {
				return "", conversation, fmt.Errorf("chunk (%d bytes): the reply hits the output limit even at the minimum chunk size", len(chunk))
			}
			continue
		}
		conversation = convID

		// Structural formats must keep their timestamps/header markup
		// byte-for-byte (ProtectedLines is empty for text/markdown, so the
		// verification passes trivially there). A summary never reproduces
		// the structural lines, so it skips verification entirely.
		if opts.Task != TaskSummarize {
			if err := filetools.VerifyProtected(format, chunk, text); err != nil {
				strict := promptFor(true) +
					"The previous attempt changed a protected (timestamps/header) line.\n" +
					"Keep every line with a timestamp or the WEBVTT/header syntax EXACTLY as in the original. Retry the chunk:\n\n" + chunk
				text2, convID2, _, err2 := translateChunk(ctx, client, conversation, strict, model, opts.Thinking)
				if err2 != nil {
					return "", conversation, fmt.Errorf("chunk (%d bytes): %w", len(chunk), err2)
				}
				conversation = convID2
				if err := filetools.VerifyProtected(format, chunk, text2); err != nil {
					return "", conversation, fmt.Errorf("chunk (%d bytes): %w", len(chunk), err)
				}
				text = text2
			}
		}

		sizer.success(len(chunk), len(text))
		translated = append(translated, text)
		offset += len(chunk)
		if opts.OnChunk != nil {
			remaining := len(src) - offset
			total := len(translated)
			if remaining > 0 {
				total += (remaining + sizer.size() - 1) / sizer.size()
			}
			opts.OnChunk(len(translated), total)
		}
	}
	return strings.TrimSpace(strings.Join(translated, "\n")) + "\n", conversation, nil
}

// idealChunk sizes a chunk so its expected output fills the per-reply output
// budget (capBytes × outputCapMargin), bounded by [minChunkBytes, maxChunk].
func idealChunk(capBytes int, ratio float64, maxChunk int) int {
	if ratio <= 0 {
		return maxChunk
	}
	n := int(float64(capBytes) * outputCapMargin / ratio)
	if n < minChunkBytes {
		n = minChunkBytes
	}
	if n > maxChunk {
		n = maxChunk
	}
	return n
}

// shrinkChunk reduces the chunk size after a truncation: halve it, but skip
// straight to the ratio-based estimate when that is smaller, so a verbose
// model converges quickly. Never returns below minChunkBytes.
func shrinkChunk(current, capBytes int, ratio float64) int {
	est := int(float64(capBytes) * outputCapMargin / 4) // assume verbose until learned
	if ratio > 0 {
		est = int(float64(capBytes) * outputCapMargin / ratio)
	}
	n := current / 2
	if est < n {
		n = est
	}
	if n < minChunkBytes {
		n = minChunkBytes
	}
	return n
}

// translateChunk sends one chunk in the session thread and returns the
// translated text plus the conversation id for the next chunk. When the reply
// was cut off at the output limit (truncated=true), text holds the partial
// reply and err is non-nil so the caller retries with a smaller chunk.
func translateChunk(ctx context.Context, client *deepseek.Client, conversation, prompt, model string, thinking bool) (text, convID string, truncated bool, err error) {
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
		ThinkingEnabled: thinking,
		SearchEnabled:   false,
	}, func(d string) error {
		buf.WriteString(d)
		return nil
	})
	if err != nil {
		return "", "", false, err
	}
	if reply.Filtered {
		// DeepSeek's content filter rejected the reply mid-stream. The content
		// produced before the filter is a genuine (if partial) translation, so
		// keep it rather than retrying: a smaller chunk cannot un-censor the
		// content, and retrying just re-triggers the filter (slow in think
		// mode, which made the run appear to hang). A reply with no content at
		// all is a hard failure.
		partial := strings.TrimSpace(buf.String())
		if partial == "" {
			return "", "", false, fmt.Errorf("reply was filtered by DeepSeek (content policy); no content produced")
		}
		next := sessionID
		if reply.MessageID != 0 {
			next = fmt.Sprintf("%s:%d", sessionID, reply.MessageID)
		} else if parent != nil {
			next = fmt.Sprintf("%s:%d", sessionID, *parent)
		}
		return partial, next, false, nil
	}
	if reply.Truncated {
		// The model stopped at its output limit: the reply is incomplete.
		// Return the partial text and an error so the caller shrinks the chunk
		// and retries, instead of writing a cut-off chunk.
		return buf.String(), "", true, fmt.Errorf("translation truncated (reply hit the output limit)")
	}
	next := sessionID
	if reply.MessageID != 0 {
		next = fmt.Sprintf("%s:%d", sessionID, reply.MessageID)
	} else if parent != nil {
		next = fmt.Sprintf("%s:%d", sessionID, *parent)
	}
	return strings.TrimSpace(buf.String()), next, false, nil
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

// DefaultImproveStyle returns the built-in writing-improvement instructions,
// used when neither --instructions nor improve-writing/default.md is found.
func DefaultImproveStyle() string {
	return `[STYLE: IMPROVE WRITING]
Improve the prose without changing its meaning, tone, register or facts.
1. Fix grammar, spelling, punctuation and awkward phrasing.
2. Tighten wordy sentences; prefer active voice and concrete nouns.
3. Vary sentence rhythm; cut filler ("very", "really", "in order to", "that of").
4. Keep names, terms, numbers and all formatting exactly as given.
5. Do not translate or localise; keep the source language.
6. Output only the improved text — no commentary, no meta text, no code fences.
`
}

// improveStyleDirs lists directories searched for improve-writing instruction
// files, in order: ./improve-writing, then <config>/dscli/improve-writing.
var improveStyleDirs = func() []string {
	dirs := []string{"improve-writing"}
	if cd, err := os.UserConfigDir(); err == nil {
		dirs = append(dirs, filepath.Join(cd, "dscli", "improve-writing"))
	}
	return dirs
}()

// ResolveImproveStyle returns the writing-improvement instructions: an explicit
// file when given, else improve-writing/default.md, else the built-in default.
func ResolveImproveStyle(explicit string) (string, error) {
	if explicit != "" {
		data, err := os.ReadFile(explicit)
		if err != nil {
			return "", fmt.Errorf("read instructions file: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	for _, dir := range improveStyleDirs {
		p := filepath.Join(dir, "default.md")
		if _, err := os.Stat(p); err == nil {
			data, err := os.ReadFile(p)
			if err != nil {
				return "", fmt.Errorf("read instructions file: %w", err)
			}
			return strings.TrimSpace(string(data)), nil
		}
	}
	return strings.TrimSpace(DefaultImproveStyle()), nil
}

// DefaultSummarizeStyle returns the built-in summarization instructions, used
// when neither --instructions nor summarize/default.md is found.
func DefaultSummarizeStyle() string {
	return `[STYLE: SUMMARIZE]
Write a compact, readable summary of the text.
1. Open with what the text is about and its main outcome or conclusion.
2. Cover key events, arguments, names and numbers; skip minor detail.
3. Keep the source language; do not translate.
4. Use flowing prose or short paragraphs, not lists, unless the source is a list.
5. Output only the summary — no commentary, no meta text, no code fences.
`
}

// summarizeStyleDirs lists directories searched for summarize instruction
// files, in order: ./summarize, then <config>/dscli/summarize.
var summarizeStyleDirs = func() []string {
	dirs := []string{"summarize"}
	if cd, err := os.UserConfigDir(); err == nil {
		dirs = append(dirs, filepath.Join(cd, "dscli", "summarize"))
	}
	return dirs
}()

// ResolveSummarizeStyle returns the summarization instructions: an explicit
// file when given, else summarize/default.md, else the built-in default.
func ResolveSummarizeStyle(explicit string) (string, error) {
	if explicit != "" {
		data, err := os.ReadFile(explicit)
		if err != nil {
			return "", fmt.Errorf("read instructions file: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	for _, dir := range summarizeStyleDirs {
		p := filepath.Join(dir, "default.md")
		if _, err := os.Stat(p); err == nil {
			data, err := os.ReadFile(p)
			if err != nil {
				return "", fmt.Errorf("read instructions file: %w", err)
			}
			return strings.TrimSpace(string(data)), nil
		}
	}
	return strings.TrimSpace(DefaultSummarizeStyle()), nil
}

// Summarize condenses content into a summary using the chunk engine with
// TaskSummarize: the adaptive sizing and per-chunk prompts of Translate apply
// unchanged, but structural verification is skipped (a summary never
// reproduces timestamps/markup). When the document needed more than one chunk,
// the per-chunk summaries are combined into one coherent summary in a final
// pass. Returns the summary (newline-terminated), the final conversation id
// and any error.
func Summarize(ctx context.Context, client *deepseek.Client, sessionID string, content []byte, format string, opts Options) (string, string, error) {
	chunks := 0
	prev := opts.OnChunk
	opts.Task = TaskSummarize
	opts.OnChunk = func(done, total int) {
		chunks = done
		if prev != nil {
			prev(done, total)
		}
	}
	text, convID, err := Translate(ctx, client, sessionID, content, format, opts)
	if err != nil {
		return "", convID, err
	}
	if chunks <= 1 {
		return text, convID, nil
	}

	model := opts.Model
	if model == "" {
		model = "default"
	}
	prompt := "The sections below are summaries of consecutive parts of one document, in order. " +
		"Combine them into a single coherent summary that keeps every key point, name and conclusion; " +
		"drop repetition; keep the ordering. Reply with ONLY the combined summary — no preamble, no commentary.\n\n" + text
	combined, convID2, _, err := translateChunk(ctx, client, convID, prompt, model, opts.Thinking)
	if err != nil {
		return "", convID2, err
	}
	return strings.TrimSpace(combined) + "\n", convID2, nil
}
