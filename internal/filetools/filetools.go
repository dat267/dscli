// Package filetools implements the tools the chat model can call:
// list_directory, read_file, file_meta, grep, create_file, edit_file,
// rename_file, delete_file, translate_file and fetch_url. The contract is a
// single JSON object per turn — see Instructions — so parsing is
// deterministic and safe: extraction only accepts a well-formed object
// naming a known tool. File access is confined to a working directory,
// including through symlink chains; fetch_url is the one networked tool and
// only ever retrieves bounded http(s) responses.
package filetools

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultMaxReadBytes is the read_file size ceiling when --max-read is
// not given: 512 KiB — about a tenth of the model's ~1M-token context at
// typical code density, and the classic single-file "large file" cutoff.
// Files larger than the effective ceiling are rejected outright (never
// truncated), so only bounded text reaches the model context.
const DefaultMaxReadBytes = 512 * 1024

// MaxReadBytes is the runtime read_file ceiling. It is initialised to
// DefaultMaxReadBytes and may be tuned (the chat command's --max-read
// flag sets it before any turn runs); concurrent mutable access is not
// expected.
var MaxReadBytes = DefaultMaxReadBytes

// binaryProbeSize is how many leading bytes are inspected for NUL bytes to
// decide whether a file is text. NULs are essentially never present in text
// encodings read here (UTF-8/ASCII).
const binaryProbeSize = 8000

// MaxListEntries caps how many directory entries a single (non-recursive)
// list_directory result reports.
const MaxListEntries = 200

// MaxRecursiveEntries caps the TOTAL entries a recursive listing reports —
// the whole subtree fits in one bounded response instead of one call per
// directory.
const MaxRecursiveEntries = 500

// MaxListDepth caps how deep a recursive listing descends.
const MaxListDepth = 6

// MaxIterations caps how many tool calls the model may make for one user
// message. The loop is hard-bounded: after the cap the CLI forces a final
// prose answer, so exploration of even a very large tree cannot run
// indefinitely.
const MaxIterations = 12

// maxGrepMatches caps how many matching lines one grep call reports; when the
// cap is hit the result ends with a truncation warning so the model narrows
// the pattern instead of scanning forever.
const maxGrepMatches = 200

// fetchTimeout bounds a single fetch_url HTTP request (redirects included).
const fetchTimeout = 30 * time.Second

// CapPrompt is appended as the final turn when the model exhausts
// MaxIterations tool calls without giving a prose answer.
const CapPrompt = "You have used all your file tool calls for this request. Do not call any more tools. Give your final answer now, based only on what you have already learned."

// IsText reports whether data looks like a text file: no NUL byte in the
// leading probe window.
func IsText(data []byte) bool {
	probe := data
	if len(probe) > binaryProbeSize {
		probe = probe[:binaryProbeSize]
	}
	return !bytes.Contains(probe, []byte{0})
}

// Call is the JSON object the model emits to request a tool call.
type Call struct {
	Tool      string `json:"tool"` // "list_directory" | "read_file" | "file_meta" | "grep" | "create_file" | "edit_file" | "rename_file" | "delete_file" | "translate_file" | "fetch_url"
	Path      string `json:"path"`
	Pattern   string `json:"pattern"`   // grep: Go regular expression to search for
	URL       string `json:"url"`       // fetch_url: http(s) URL to retrieve
	Old       string `json:"old"`       // edit_file: exact existing text (first occurrence replaced)
	New       string `json:"new"`       // edit_file: replacement text; rename_file: destination name/path
	Content   string `json:"content"`   // create_file: full content of the new file
	Recursive bool   `json:"recursive"` // list_directory: list the whole subtree at once
	From      string `json:"from"`      // translate_file: source language (style lookup; optional)
	To        string `json:"to"`        // translate_file: target language
	Output    string `json:"output"`    // translate_file: output path (default <input>.translated.<ext>)
}

// Instructions returns the prompt fragment that defines the tools for the
// model. It is prepended to the user's prompt.
func Instructions(workdir string) string {
	return fmt.Sprintf(`[TOOLS]
You can inspect and change files inside %s, search them, and fetch http(s) URLs. To use a tool, reply with ONLY a JSON object (no other text, no markdown):
  {"tool":"list_directory","path":"<dir>"}
  {"tool":"read_file","path":"<file>"}
  {"tool":"file_meta","path":"<file>"}
  {"tool":"grep","pattern":"<go regex>","path":"<optional file or dir>"}
  {"tool":"fetch_url","url":"<http(s) URL>"}
  {"tool":"create_file","path":"<file>","content":"<full content>"}
  {"tool":"edit_file","path":"<file>","old":"<exact existing text>","new":"<replacement text>"}
  {"tool":"rename_file","path":"<source>","new":"<new name or path>"}
  {"tool":"delete_file","path":"<file>"}
  {"tool":"translate_file","path":"<file>","to":"<language>","output":"<optional output path>"}
Rules:
- ACT, don't announce. When the user asks you to inspect or change files, perform the tool calls yourself in the same reply series; never reply with prose about what you are about to do ("let me...", "I'll...", "first I need to...").
- Budget: at most %d tool calls per user message; spend them on what matters, then give your final answer in prose.
- Every turn is exactly ONE of two things: a single tool-call JSON object, or — only when your task is fully complete — your final answer in plain text.
- To explore: start with {"tool":"list_directory","path":"."}, which lists ONE directory, non-recursive, directories marked with a trailing /, files with sizes. Add "recursive":true to get the whole subtree in one bounded call ({\"tool\":\"list_directory\",\"path\":\".\",\"recursive\":true}) instead of listing directory by directory — prefer it for exploring a tree.
- grep searches for a Go regular expression and returns up to %d matching lines as "path:line:text"; "path" is optional (default: the whole workdir) and may name one file or directory. VCS directories (.git/.hg/.svn) are skipped; prefix the pattern with (?i) for case-insensitive search.
- read_file only reads TEXT files up to %d bytes, and grep only searches TEXT files up to %d bytes: larger or binary files are rejected — do not retry them, tell the user instead.
- fetch_url retrieves an http(s) URL and returns its text, capped at %d bytes; only http/https are allowed and binary or oversized responses are rejected. Use it when you need the exact contents of a specific page.
- translate_file translates a subtitle/lyric/document file (txt/md/lrc/srt/vtt/ass/ttml/epub) into another language in ONE call — timestamps, XML structure and markup are preserved and verified automatically; the user confirms first. Use it instead of read+create translation.
- To change: create_file makes a NEW file (it errors if the file already exists — then use edit_file or delete_file first); edit_file replaces the first exact occurrence of "old" (whitespace, quotes and indentation count — read the file first and copy from it); rename_file renames or moves a file/directory (the destination must not exist); delete_file removes a file permanently. Creating, renaming or deleting files asks the user for confirmation; if the user rejects, do not retry.
- After every tool call you receive a <tool_result> block. React to it with the next tool call, or your final answer. If an edit reports the pattern was not found, re-read the file and retry with the correct "old" text.
- Paths may be relative to %s or absolute inside it.
[END TOOLS]

User: `, workdir, MaxIterations, maxGrepMatches, MaxReadBytes, MaxReadBytes, MaxReadBytes, workdir)
}

// FormatResult wraps a tool outcome for feeding back to the model.
func FormatResult(tool, path string, body string) string {
	return fmt.Sprintf("<tool_result tool=%q path=%q>\n%s\n</tool_result>", tool, path, body)
}

// Extract finds a file-tool call in a reply. It accepts, in order of
// preference: a whole-reply JSON object, a ```json code fence, or a balanced
// {...} object beginning at the first `{"tool":`. The object must name a
// known tool with the required fields; anything else is treated as prose and
// yields ok=false.
func Extract(text string) (Call, bool) {
	candidates := []string{strings.TrimSpace(text)}
	if block, ok := fencedJSON(text); ok {
		candidates = append(candidates, block)
	}
	if obj, ok := braceObject(text); ok {
		candidates = append(candidates, obj)
	}
	for _, raw := range candidates {
		if raw == "" {
			continue
		}
		var c Call
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			continue
		}
		if valid(c) {
			return c, true
		}
	}
	return Call{}, false
}

// LikelyToolCall reports whether text embeds a tool-call-shaped JSON object
// (a ```json fence or a balanced {"tool":...} object), even when it did not
// parse as a valid Call. The chat loop uses it to keep a malformed tool call
// out of the visible chat and re-prompt the model instead of rendering the
// raw JSON.
func LikelyToolCall(text string) bool {
	if _, ok := fencedJSON(text); ok {
		return true
	}
	_, ok := braceObject(text)
	return ok
}

func valid(c Call) bool {
	switch c.Tool {
	case "read_file", "list_directory", "file_meta", "create_file", "delete_file":
		return c.Path != ""
	case "grep":
		return c.Pattern != ""
	case "fetch_url":
		return c.URL != ""
	case "edit_file":
		return c.Path != "" && c.Old != ""
	case "rename_file":
		return c.Path != "" && c.New != ""
	case "translate_file":
		return c.Path != "" && c.To != ""
	}
	return false
}

// Display renders a path for the user: relative to the canonical workdir when
// it lies inside it, otherwise as given. Used for preview headers and
// confirmations.
func Display(workdir, p string) string {
	path, err := workdirPath(workdir, p)
	if err != nil {
		return p
	}
	root, err := workdirPath(workdir, ".")
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return p
	}
	return rel
}

// fencedJSON extracts the content of a ```json ... ``` (or plain ``` ... ```)
// code fence, if the reply contains one.
func fencedJSON(text string) (string, bool) {
	start := strings.Index(text, "```")
	if start < 0 {
		return "", false
	}
	rest := text[start+len("```"):]
	rest = strings.TrimPrefix(rest, "json")
	rest = strings.TrimPrefix(rest, "\n")
	end := strings.Index(rest, "```")
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(rest[:end]), true
}

// braceObject scans for the first balanced JSON object that begins with
// `{"tool":` (allowing whitespace between "{" and the key), respecting
// string contents and escapes.
func braceObject(text string) (string, bool) {
	needle := `{"tool":`
	idx := strings.Index(text, needle)
	if idx < 0 {
		return "", false
	}
	start := idx
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[start : i+1], true
			}
		}
	}
	return "", false
}

// workdirPath resolves p inside workdir and rejects anything that escapes it.
// The check is twofold: lexically (Clean + Rel) and canonically — symlink
// chains within the workdir are resolved via EvalSymlinks and must stay
// inside the (also canonicalised) working directory. Non-existent tails (the
// create_file case) are resolved through their deepest existing ancestor.
func workdirPath(workdir, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	root, err := filepath.Abs(workdir)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	full := p
	if !filepath.IsAbs(full) {
		full = filepath.Join(root, p)
	}
	full = filepath.Clean(full)

	// Lexical containment (fast path, clear errors).
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the working directory", p)
	}

	// Canonical containment: eval the deepest existing ancestor of full, then
	// re-join the remainder, and require the result to live inside the
	// resolved root.
	rootEval, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}

	existing := full
	var tail []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		tail = append([]string{filepath.Base(existing)}, tail...)
		existing = parent
	}
	evaluated, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	final := evaluated
	for _, part := range tail {
		final = filepath.Join(final, part)
	}
	final = filepath.Clean(final)

	rel2, err := filepath.Rel(rootEval, final)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if rel2 == ".." || strings.HasPrefix(rel2, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the working directory (symlink)", p)
	}
	return filepath.Join(rootEval, rel2), nil
}

// ResolvePath resolves p inside workdir with full (symlink-aware)
// containment, for callers outside the package (e.g. the translate_file
// tool, whose execution lives in the cmd layer).
func ResolvePath(workdir, p string) (string, error) {
	return workdirPath(workdir, p)
}

// Run executes the call inside workdir and returns the <tool_result> text to
// feed back to the model (never a Go error: the outcome is for the model).
// Note: the write/delete variants apply immediately — the interactive flow
// that requires confirmation lives in the cmd layer (plan → preview →
// confirm → apply).
func (c Call) Run(workdir string) string {
	switch c.Tool {
	case "list_directory":
		return c.runList(workdir)
	case "file_meta":
		return c.runMeta(workdir)
	case "read_file":
		return c.runRead(workdir)
	case "grep":
		return c.runGrep(workdir)
	case "fetch_url":
		return c.runFetch()
	case "edit_file":
		return c.runEdit(workdir)
	case "rename_file":
		return c.runRename(workdir)
	case "create_file":
		return c.runCreate(workdir)
	case "delete_file":
		return c.runDelete(workdir)
	}
	return FormatResult(c.Tool, c.Path, "ERROR: unknown tool")
}

func (c Call) runList(workdir string) string {
	path, err := workdirPath(workdir, c.Path)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	if !c.Recursive {
		body, err := listOneDir(path)
		if err != nil {
			return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
		}
		return FormatResult(c.Tool, c.Path, body)
	}
	if f, err := os.Open(path); err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	} else {
		_ = f.Close()
	}
	body, truncated := listTree(Display(workdir, c.Path), path)
	if truncated {
		body += fmt.Sprintf("\nWARNING: tree truncated (max %d entries, depth %d)", MaxRecursiveEntries, MaxListDepth)
	}
	return FormatResult(c.Tool, c.Path, body)
}

// listOneDir renders a non-recursive listing of a directory: sorted names,
// files annotated with human-readable sizes, bounded batches so a huge
// directory costs ~MaxListEntries of work.
func listOneDir(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	var lines []string
	for {
		batch, err := f.ReadDir(256)
		for _, e := range batch {
			lines = append(lines, entryLine(e))
			if len(lines) > MaxListEntries {
				break
			}
		}
		if len(lines) > MaxListEntries || err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	sort.Strings(lines)

	truncated := len(lines) > MaxListEntries
	if truncated {
		lines = lines[:MaxListEntries]
	}
	body := strings.Join(lines, "\n")
	if truncated {
		body += fmt.Sprintf("\nWARNING: directory truncated at %d entries", MaxListEntries)
	}
	if body == "" {
		body = "(empty directory)"
	}
	return body, nil
}

// entryLine formats one directory entry: directories with a trailing '/',
// files with a human-readable size.
func entryLine(e os.DirEntry) string {
	name := e.Name()
	if e.IsDir() {
		return name + "/"
	}
	if info, err := e.Info(); err == nil {
		return name + " (" + humanSize(info.Size()) + ")"
	}
	return name
}

// listTree renders a bounded depth-first tree of path: two-space indent per
// level, directories with '/', files with sizes. rootLabel is the
// workdir-relative name of the root ("." for the workdir itself). Returns
// (body, truncated). Symlinked directories are listed but NOT descended
// into, so the walk can never leave the workdir or loop.
func listTree(rootLabel, path string) (string, bool) {
	rootIsDot := rootLabel == "."
	var b strings.Builder
	count := 0
	truncated := false
	var walk func(dir, prefix string, depth int)
	walk = func(dir, prefix string, depth int) {
		if truncated {
			return
		}
		f, err := os.Open(dir)
		if err != nil {
			fmt.Fprintf(&b, "%s(unreadable: %v)\n", prefix, err)
			return
		}
		defer func() { _ = f.Close() }()

		var entries []os.DirEntry
		for {
			batch, err := f.ReadDir(256)
			entries = append(entries, batch...)
			if len(entries) > MaxRecursiveEntries || err == io.EOF {
				break
			}
			if err != nil {
				break
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if count >= MaxRecursiveEntries {
				truncated = true
				return
			}
			indent := prefix + "  "
			fmt.Fprintf(&b, "%s%s\n", indent, entryLine(e))
			count++
			if depth >= MaxListDepth {
				continue
			}
			if e.IsDir() {
				walk(filepath.Join(dir, e.Name()), indent, depth+1)
			}
			// Symlinks are never descended into (DirEntry.IsDir does not
			// follow links), so the tree cannot escape the workdir.
		}
	}
	if rootIsDot {
		b.WriteString(".\n")
	} else {
		b.WriteString(rootLabel + "/\n")
	}
	walk(path, "", 0)
	return strings.TrimSuffix(b.String(), "\n"), truncated
}

func (c Call) runRead(workdir string) string {
	path, err := workdirPath(workdir, c.Path)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	info, err := os.Stat(path)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	if info.IsDir() {
		return FormatResult(c.Tool, c.Path, "ERROR: is a directory; read_file reads a single file")
	}
	// EPUB files are ZIP archives of XHTML; extract their text so "read my
	// book" works even though the container is binary.
	if strings.EqualFold(filepath.Ext(path), ".epub") {
		return c.readEPUB(path)
	}
	// Hard size ceiling, checked before reading so large files are never
	// pulled into memory or context.
	if info.Size() > int64(MaxReadBytes) {
		return FormatResult(c.Tool, c.Path,
			fmt.Sprintf("ERROR: file is %d bytes, exceeding the max read size of %d bytes", info.Size(), MaxReadBytes))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	// Text-only guard: binary content would pollute the model context.
	if !IsText(data) {
		return FormatResult(c.Tool, c.Path, "ERROR: not a text file (binary content); read_file only reads text")
	}
	return FormatResult(c.Tool, c.Path, string(data))
}

// readEPUB extracts the chapter text of an EPUB (ZIP of XHTML) in spine
// order, stripped of markup, capped at MaxReadBytes of extracted text, and
// wraps it as a tool result.
func (c Call) readEPUB(path string) string {
	text, err := ReadEpub(path)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	return FormatResult(c.Tool, c.Path, text)
}

// ReadEpub extracts the chapter text of an EPUB (ZIP of XHTML) in spine
// order, stripped of markup, capped at MaxReadBytes of extracted text. The
// result carries a header noting it is extracted text.
func ReadEpub(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return "", fmt.Errorf("not a readable EPUB: %w", err)
	}
	chapters, err := epubChapters(zr)
	if err != nil {
		return "", fmt.Errorf("EPUB parse failed: %w", err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "EPUB text extracted from %s (%d sections)\n\n", path, len(chapters))
	for _, ch := range chapters {
		text, err := stripHTML(ch)
		if err != nil {
			return "", fmt.Errorf("EPUB chapter parse failed: %w", err)
		}
		if text == "" {
			continue
		}
		if out.Len()+len(text) > MaxReadBytes {
			out.WriteString("\n[truncated: remaining chapters exceed the read size limit]\n")
			break
		}
		out.WriteString(text)
		out.WriteString("\n\n")
	}
	return out.String(), nil
}

// findZip locates a case-insensitively matched file inside an EPUB archive.
func findZip(zr *zip.Reader, name string) (*zip.File, bool) {
	for _, f := range zr.File {
		if strings.EqualFold(f.Name, name) {
			return f, true
		}
	}
	return nil, false
}

// epubOPFPath reads META-INF/container.xml and returns the OPF path
// (relative to the archive root) that describes the book.
func epubOPFPath(zr *zip.Reader) (string, error) {
	container, ok := findZip(zr, "META-INF/container.xml")
	if !ok {
		return "", fmt.Errorf("missing META-INF/container.xml")
	}
	rc, err := container.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	var ctr struct {
		Rootfiles struct {
			Rootfile []struct {
				FullPath string `xml:"full-path,attr"`
			} `xml:"rootfile"`
		} `xml:"rootfiles"`
	}
	if err := xml.NewDecoder(rc).Decode(&ctr); err != nil {
		return "", fmt.Errorf("container.xml: %w", err)
	}
	if len(ctr.Rootfiles.Rootfile) == 0 {
		return "", fmt.Errorf("container.xml has no rootfile")
	}
	return ctr.Rootfiles.Rootfile[0].FullPath, nil
}

// epubChapters returns the XHTML files of an EPUB in spine order, reading
// META-INF/container.xml → OPF → manifest/spine.
func epubChapters(zr *zip.Reader) ([]string, error) {
	opfPath, err := epubOPFPath(zr)
	if err != nil {
		return nil, err
	}
	opfFile, ok := findZip(zr, opfPath)
	if !ok {
		return nil, fmt.Errorf("OPF %q not found", opfPath)
	}
	rc, err := opfFile.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var opf struct {
		Manifest struct {
			Items []struct {
				ID   string `xml:"id,attr"`
				Href string `xml:"href,attr"`
			} `xml:"item"`
		} `xml:"manifest"`
		Spine struct {
			Itemrefs []struct {
				IDRef string `xml:"idref,attr"`
			} `xml:"itemref"`
		} `xml:"spine"`
	}
	if err := xml.NewDecoder(rc).Decode(&opf); err != nil {
		return nil, fmt.Errorf("OPF: %w", err)
	}
	hrefs := make(map[string]string, len(opf.Manifest.Items))
	for _, it := range opf.Manifest.Items {
		hrefs[it.ID] = it.Href
	}
	base := filepath.Dir(opfFile.Name)
	var chapters []string
	for _, ref := range opf.Spine.Itemrefs {
		href, ok := hrefs[ref.IDRef]
		if !ok {
			continue
		}
		name := filepath.ToSlash(filepath.Join(base, href))
		f, ok := findZip(zr, name)
		if !ok {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(rc, int64(MaxReadBytes+1)))
		rc.Close()
		if err != nil {
			return nil, err
		}
		chapters = append(chapters, string(data))
	}
	return chapters, nil
}

// stripHTML removes markup and decodes common entities, returning visible
// text. Deterministic and deliberately simple — formatting is irrelevant to
// the model.
func stripHTML(s string) (string, error) {
	var r strings.Builder
	r.Grow(len(s))
	inTag := false
	inScript := false
	i := 0
	for i < len(s) {
		switch {
		case inTag:
			if s[i] == '>' {
				inTag = false
			}
		case !inTag && i+7 <= len(s) && strings.EqualFold(s[i:i+6], "<script"):
			inTag = true
			inScript = true
		case inScript && strings.HasPrefix(strings.ToLower(s[i:]), "</script"):
			inScript = false
			inTag = true
		case s[i] == '<':
			inTag = true
		case s[i] == '&':
			end := strings.IndexByte(s[i:], ';')
			if end >= 0 && end <= 8 {
				switch s[i+1 : i+end] {
				case "amp":
					r.WriteByte('&')
				case "lt":
					r.WriteByte('<')
				case "gt":
					r.WriteByte('>')
				case "quot":
					r.WriteByte('"')
				case "#39", "apos":
					r.WriteByte('\'')
				case "nbsp":
					r.WriteByte(' ')
				default:
					r.WriteString(s[i : i+end+1])
				}
				i += end + 1
				continue
			}
			r.WriteByte('&')
		default:
			r.WriteByte(s[i])
		}
		i++
	}
	return r.String(), nil
}

func (c Call) runEdit(workdir string) string {
	plan := PlanEdit(workdir, c)
	if plan.Result != "" {
		return FormatResult(c.Tool, c.Path, plan.Result)
	}
	if err := ApplyEdit(workdir, c, plan); err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	return FormatResult(c.Tool, c.Path, EditSummary(plan.Count, c.Old, c.New))
}

// EditPlan is the deterministic outcome of planning an edit_file call: the
// exact content ApplyEdit will write, a line-numbered preview of the change,
// and — when the edit cannot be applied — the ERROR line to feed the model.
type EditPlan struct {
	OriginalContent string // file content at plan time; ApplyEdit verifies it is unchanged
	NewContent      string // file content after the first-occurrence replacement
	Preview         string // user-facing diff preview ("" when Result != "")
	Count           int    // total occurrences of the pattern in the file
	Mode            os.FileMode
	Result          string // non-empty means the plan failed; the other fields
	// are then empty and Result is a model-facing ERROR line
}

// PlanEdit resolves and reads the file, verifies the pattern exists, and
// computes the EXACT new content that ApplyEdit will later write — the
// preview and the write are derived from the same bytes, so what the user
// approves is precisely what lands on disk (as long as the file is not
// changed in between, which ApplyEdit detects). It performs no write.
func PlanEdit(workdir string, c Call) EditPlan {
	path, err := workdirPath(workdir, c.Path)
	if err != nil {
		return EditPlan{Result: "ERROR: " + err.Error()}
	}
	info, err := os.Stat(path)
	if err != nil {
		return EditPlan{Result: "ERROR: " + err.Error()}
	}
	if info.IsDir() {
		return EditPlan{Result: "ERROR: is a directory; edit_file edits a single file"}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return EditPlan{Result: "ERROR: " + err.Error()}
	}
	content := string(data)
	count := strings.Count(content, c.Old)
	if count == 0 {
		return EditPlan{Result: "ERROR: pattern not found in file; re-read the file and copy the exact text"}
	}
	return EditPlan{
		OriginalContent: content,
		NewContent:      strings.Replace(content, c.Old, c.New, 1),
		Preview:         buildPreview(c.Path, content, c.Old, c.New, count),
		Count:           count,
		Mode:            info.Mode().Perm(),
	}
}

// ApplyEdit writes the content produced by PlanEdit — but only after
// verifying the file still matches the plan's OriginalContent, so a file
// changed after the preview is never clobbered. It preserves the file's mode.
func ApplyEdit(workdir string, c Call, plan EditPlan) error {
	path, err := workdirPath(workdir, c.Path)
	if err != nil {
		return err
	}
	if plan.Result != "" {
		return fmt.Errorf("edit plan failed: %s", plan.Result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(data) != plan.OriginalContent {
		return fmt.Errorf("file changed since the preview; edit not applied — re-read the file")
	}
	perm := plan.Mode
	if perm == 0 {
		perm = 0644
	}
	return os.WriteFile(path, []byte(plan.NewContent), perm)
}

// humanSize renders a byte count compactly for listings.
func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// RenamePlan is the deterministic outcome of planning a rename_file call.
type RenamePlan struct {
	Preview string
	Bytes   int64
	IsDir   bool
	OldName string
	NewName string
	Result  string // non-empty means the plan failed
}

// PlanRename verifies the source exists, the destination is free, and both
// stay inside the workdir (a destination in a subdirectory moves the item).
// Nothing is renamed here.
func PlanRename(workdir string, c Call) RenamePlan {
	oldPath, err := workdirPath(workdir, c.Path)
	if err != nil {
		return RenamePlan{Result: "ERROR: " + err.Error()}
	}
	newPath, err := workdirPath(workdir, c.New)
	if err != nil {
		return RenamePlan{Result: "ERROR: " + err.Error()}
	}
	if oldPath == newPath {
		return RenamePlan{Result: "ERROR: source and destination are the same path"}
	}
	info, err := os.Stat(oldPath)
	if err != nil {
		return RenamePlan{Result: "ERROR: " + err.Error()}
	}
	if _, err := os.Lstat(newPath); err == nil {
		return RenamePlan{Result: "ERROR: destination already exists; choose a free name"}
	} else if !os.IsNotExist(err) {
		return RenamePlan{Result: "ERROR: " + err.Error()}
	}
	kind := "file"
	if info.IsDir() {
		kind = "directory"
	}
	preview := fmt.Sprintf("%s → %s\n(%s, %d bytes)",
		Display(workdir, c.Path), Display(workdir, c.New), kind, info.Size())
	return RenamePlan{
		Preview: preview,
		Bytes:   info.Size(),
		IsDir:   info.IsDir(),
		OldName: Display(workdir, c.Path),
		NewName: Display(workdir, c.New),
	}
}

// ApplyRename performs the rename the plan previewed, re-verifying the source
// still exists and the destination is still free.
func ApplyRename(workdir string, c Call) error {
	oldPath, err := workdirPath(workdir, c.Path)
	if err != nil {
		return err
	}
	newPath, err := workdirPath(workdir, c.New)
	if err != nil {
		return err
	}
	if _, err := os.Stat(oldPath); err != nil {
		return fmt.Errorf("source no longer exists: %w", err)
	}
	if _, err := os.Lstat(newPath); err == nil {
		return fmt.Errorf("destination appeared since the preview; rename not applied")
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(oldPath, newPath)
}

func (c Call) runRename(workdir string) string {
	plan := PlanRename(workdir, c)
	if plan.Result != "" {
		return FormatResult(c.Tool, c.Path, plan.Result)
	}
	if err := ApplyRename(workdir, c); err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	return FormatResult(c.Tool, c.Path, fmt.Sprintf("renamed %s → %s", plan.OldName, plan.NewName))
}

// EditSummary is the compact <tool_result> body fed back to the model after a
// successful edit.
func EditSummary(count int, old, new string) string {
	oldSnip, newSnip := old, new
	if cut := 120; len(oldSnip) > cut {
		oldSnip = oldSnip[:cut] + "..."
	}
	if cut := 120; len(newSnip) > cut {
		newSnip = newSnip[:cut] + "..."
	}
	return fmt.Sprintf("replaced first of %d occurrences\nold: %q\nnew: %q", count, oldSnip, newSnip)
}

// buildPreview renders a deterministic, line-numbered view of the first
// occurrence replacement: two context lines, the removed lines prefixed with
// '-', and the replacement lines prefixed with '+'. Draws only from the
// bytes of the actual operation.
func buildPreview(path, content, old, new string, count int) string {
	idx := strings.Index(content, old)
	if idx < 0 {
		return ""
	}
	startLine := strings.Count(content[:idx], "\n") + 1
	oldLines := strings.Count(old, "\n") + 1
	lines := strings.Split(content, "\n")
	const ctx = 2
	from := max(1, startLine-ctx)
	to := min(len(lines), startLine+oldLines-1+ctx)

	var b strings.Builder
	fmt.Fprintf(&b, "%s — replacing first of %d occurrence(s)\n", path, count)
	// Walk the visible window; at the change position, print the removed
	// lines and then the replacement lines IN PLACE (unified-diff order),
	// so the context reads top-to-bottom as the file will.
	ln := from
	for ln <= to {
		if ln == startLine {
			for i := 0; i < oldLines; i++ {
				fmt.Fprintf(&b, "-%3d │ %s\n", startLine+i, lines[startLine+i-1])
				ln++
			}
			for i, l := range strings.Split(new, "\n") {
				fmt.Fprintf(&b, "+%3d │ %s\n", startLine+i, l)
			}
			continue
		}
		fmt.Fprintf(&b, " %3d │ %s\n", ln, lines[ln-1])
		ln++
	}
	return b.String()
}

// CreatePlan is the deterministic outcome of planning a create_file call.
type CreatePlan struct {
	NewContent string
	Preview    string
	Result     string // non-empty means the plan failed
}

// PlanCreate checks the target does not exist and renders a preview of the
// new file, without writing anything.
func PlanCreate(workdir string, c Call) CreatePlan {
	path, err := workdirPath(workdir, c.Path)
	if err != nil {
		return CreatePlan{Result: "ERROR: " + err.Error()}
	}
	if _, err := os.Stat(path); err == nil {
		return CreatePlan{Result: "ERROR: file already exists; use edit_file to modify existing files"}
	} else if !os.IsNotExist(err) {
		return CreatePlan{Result: "ERROR: " + err.Error()}
	}
	var b strings.Builder
	lines := strings.Split(c.Content, "\n")
	if c.Content == "" {
		lines = nil
	}
	fmt.Fprintf(&b, "%s — creating (%d lines, %d bytes)\n", Display(workdir, c.Path), len(lines), len(c.Content))
	for i, l := range lines {
		fmt.Fprintf(&b, "+%3d │ %s\n", i+1, l)
	}
	return CreatePlan{NewContent: c.Content, Preview: b.String()}
}

// ApplyCreate writes the new file (creating parent directories within the
// workdir), with the exact content the plan previewed — provided the target
// still does not exist, so a file that appeared after the plan is never
// overwritten.
func ApplyCreate(workdir string, c Call, content string) error {
	path, err := workdirPath(workdir, c.Path)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("file already exists (created after the preview); create not applied")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0644)
}

func (c Call) runCreate(workdir string) string {
	plan := PlanCreate(workdir, c)
	if plan.Result != "" {
		return FormatResult(c.Tool, c.Path, plan.Result)
	}
	if err := ApplyCreate(workdir, c, plan.NewContent); err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	return FormatResult(c.Tool, c.Path, fmt.Sprintf("created %s (%d bytes)", c.Path, len(plan.NewContent)))
}

// DeletePlan is the deterministic outcome of planning a delete_file call.
type DeletePlan struct {
	Preview string
	Bytes   int64
	Result  string // non-empty means the plan failed
}

// PlanDelete verifies the target is a regular file and previews its head,
// without deleting anything.
func PlanDelete(workdir string, c Call) DeletePlan {
	path, err := workdirPath(workdir, c.Path)
	if err != nil {
		return DeletePlan{Result: "ERROR: " + err.Error()}
	}
	info, err := os.Stat(path)
	if err != nil {
		return DeletePlan{Result: "ERROR: " + err.Error()}
	}
	if info.IsDir() {
		return DeletePlan{Result: "ERROR: is a directory; delete_file removes a single file"}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return DeletePlan{Result: "ERROR: " + err.Error()}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — deleting (%d bytes)\n", Display(workdir, c.Path), info.Size())
	if IsText(data) {
		lines := strings.Split(string(data), "\n")
		fmt.Fprintf(&b, "%d line(s)\n", len(lines))
		show := min(len(lines), 12)
		for i := 0; i < show; i++ {
			fmt.Fprintf(&b, "-%3d │ %s\n", i+1, lines[i])
		}
		if show < len(lines) {
			fmt.Fprintf(&b, "  … %d more line(s)\n", len(lines)-show)
		}
	} else {
		fmt.Fprintln(&b, "(binary content, not shown)")
	}
	return DeletePlan{Preview: b.String(), Bytes: info.Size()}
}

// ApplyDelete removes the file the plan previewed, re-verifying it still
// exists as a regular file (a file removed or replaced in the meantime is
// never deleted blindly, and a directory is never removed).
func ApplyDelete(workdir string, c Call) error {
	path, err := workdirPath(workdir, c.Path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("is a directory; delete_file removes a single file")
	}
	return os.Remove(path)
}

func (c Call) runDelete(workdir string) string {
	plan := PlanDelete(workdir, c)
	if plan.Result != "" {
		return FormatResult(c.Tool, c.Path, plan.Result)
	}
	if err := ApplyDelete(workdir, c); err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	return FormatResult(c.Tool, c.Path, fmt.Sprintf("deleted %s (%d bytes)", c.Path, plan.Bytes))
}

// maxDirStatEntries caps how many entries a directory file_meta counts.
const maxDirStatEntries = 5000

// maxDirStatDepth caps how deep a directory file_meta walks.
const maxDirStatDepth = 8

// runMeta reports file/directory metadata without prompting.
func (c Call) runMeta(workdir string) string {
	path, err := workdirPath(workdir, c.Path)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	info, err := os.Stat(path)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "path: %s\n", c.Path)
	if info.IsDir() {
		fmt.Fprintln(&b, "kind: directory")
		count, total, truncated := dirStats(path, 0)
		fmt.Fprintf(&b, "entries: %d\n", count)
		fmt.Fprintf(&b, "total size: %s (%d bytes)\n", humanSize(total), total)
		if truncated {
			fmt.Fprintf(&b, "note: entry walk capped at %d\n", maxDirStatEntries)
		}
	} else {
		fmt.Fprintln(&b, "kind: file")
		fmt.Fprintf(&b, "size: %s (%d bytes)\n", humanSize(info.Size()), info.Size())
		fmt.Fprintf(&b, "modified: %s\n", info.ModTime().Format("2006-01-02 15:04:05"))
		fmt.Fprintf(&b, "mode: %04o\n", info.Mode().Perm())
		switch strings.ToLower(filepath.Ext(path)) {
		case ".epub":
			if meta := epubMeta(path); meta != "" {
				fmt.Fprintf(&b, "epub: %s\n", meta)
			}
		case ".lrc":
			if d, ok := lrcDuration(path); ok {
				fmt.Fprintf(&b, "duration: %s\n", d)
			}
		case ".srt":
			if d, ok := srtDuration(path); ok {
				fmt.Fprintf(&b, "duration: %s\n", d)
			}
		case ".vtt":
			if d, ok := vttDuration(path); ok {
				fmt.Fprintf(&b, "duration: %s\n", d)
			}
		case ".ass", ".ssa":
			if d, ok := assDuration(path); ok {
				fmt.Fprintf(&b, "duration: %s\n", d)
			}
		case ".ttml":
			if d, ok := ttmlDuration(path); ok {
				fmt.Fprintf(&b, "duration: %s\n", d)
			}
		}
	}
	return FormatResult(c.Tool, c.Path, strings.TrimSuffix(b.String(), "\n"))
}

// runGrep searches a file or the whole workdir for a Go regular expression
// and returns matching lines as "path:line:text", capped at maxGrepMatches.
// Binary and oversized files are skipped (mirroring read_file's ceiling), as
// are VCS metadata directories; symlinked directories are not descended into.
func (c Call) runGrep(workdir string) string {
	root := c.Path
	if root == "" {
		root = "."
	}
	path, err := workdirPath(workdir, root)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	re, err := regexp.Compile(c.Pattern)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: invalid pattern: "+err.Error())
	}
	info, err := os.Stat(path)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}

	var lines []string
	truncated := false
	collect := func(p string) { c.grepFile(re, p, workdir, &lines, &truncated) }
	if info.IsDir() {
		_ = filepath.WalkDir(path, func(p string, d os.DirEntry, werr error) error {
			if werr != nil {
				return nil // unreadable entries are skipped, like grep
			}
			if d.IsDir() {
				if p != path && skipVCSDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type().IsRegular() {
				collect(p)
			}
			return nil
		})
	} else {
		collect(path)
	}

	body := strings.Join(lines, "\n")
	if truncated {
		body += fmt.Sprintf("\nWARNING: grep truncated at %d matching lines; narrow the pattern or path", maxGrepMatches)
	}
	if body == "" {
		body = "(no matches)"
	}
	return FormatResult(c.Tool, c.Path, body)
}

// grepFile appends the matching lines of one file (displayed relative to
// workdir) as "path:line:text", stopping at maxGrepMatches (truncated is set
// when the cap stops the scan). Binary and oversized files are skipped.
func (c Call) grepFile(re *regexp.Regexp, p, workdir string, out *[]string, truncated *bool) {
	if *truncated {
		return
	}
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return
	}
	if info.Size() > int64(MaxReadBytes) {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	if !IsText(data) {
		return
	}
	display := Display(workdir, p)
	for i, line := range strings.Split(string(data), "\n") {
		if *truncated {
			return
		}
		if re.MatchString(line) {
			*out = append(*out, fmt.Sprintf("%s:%d:%s", display, i+1, line))
			if len(*out) >= maxGrepMatches {
				*truncated = true
				return
			}
		}
	}
}

// skipVCSDir reports whether a directory is a VCS metadata dir that grep must
// not descend into (their packed and binary stores produce noise and cost).
func skipVCSDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn":
		return true
	}
	return false
}

// runFetch retrieves an http(s) URL and returns its text, capped at
// MaxReadBytes. Non-http(s) schemes, binary bodies, and non-2xx responses are
// rejected. Redirects are followed and reported by their final URL; the
// filesystem is never touched.
func (c Call) runFetch() string {
	u, err := url.Parse(c.URL)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: invalid URL: "+err.Error())
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return FormatResult(c.Tool, c.Path, fmt.Sprintf("ERROR: unsupported URL scheme %q (only http/https)", u.Scheme))
	}
	if u.Host == "" {
		return FormatResult(c.Tool, c.Path, "ERROR: URL has no host")
	}
	client := &http.Client{Timeout: fetchTimeout}
	resp, err := client.Get(c.URL)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return FormatResult(c.Tool, c.Path, fmt.Sprintf("ERROR: HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(MaxReadBytes+1)))
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: read response: "+err.Error())
	}
	truncated := len(data) > MaxReadBytes
	if truncated {
		data = data[:MaxReadBytes]
	}
	if !IsText(data) {
		return FormatResult(c.Tool, c.Path, "ERROR: response is not text (binary content); fetch_url only returns text")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d %s · %s\n\n", resp.StatusCode, http.StatusText(resp.StatusCode), resp.Request.URL.String())
	b.Write(data)
	if truncated {
		fmt.Fprintf(&b, "\nWARNING: response truncated at %d bytes (the max read size)", MaxReadBytes)
	}
	return FormatResult(c.Tool, c.Path, b.String())
}

// dirStats counts entries and sums file sizes under root, with bounded batched
// reads, a depth cap, and an entry cap. Symlinked directories are not followed.
func dirStats(root string, depth int) (count int, total int64, truncated bool) {
	if depth > maxDirStatDepth || count > maxDirStatEntries {
		return count, total, count > maxDirStatEntries
	}
	f, err := os.Open(root)
	if err != nil {
		return count, total, truncated
	}
	defer func() { _ = f.Close() }()
	for {
		entries, err := f.ReadDir(256)
		for _, e := range entries {
			if count >= maxDirStatEntries {
				return count, total, true
			}
			count++
			info, err := e.Info()
			if err == nil && !info.IsDir() {
				total += info.Size()
			}
			if e.IsDir() {
				var t bool
				count2, size, trunc := dirStats(filepath.Join(root, e.Name()), depth+1)
				count, total, t = count+count2, total+size, truncated || trunc
				_ = t
			}
		}
		if err == io.EOF {
			return count, total, truncated
		}
		if err != nil {
			return count, total, truncated
		}
	}
}

// epubMeta returns "title — author(s)" from an EPUB's OPF metadata, if present.
func epubMeta(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return ""
	}
	opfPath, err := epubOPFPath(zr)
	if err != nil {
		return ""
	}
	opf, ok := findZip(zr, opfPath)
	if !ok {
		return ""
	}
	rc, err := opf.Open()
	if err != nil {
		return ""
	}
	defer rc.Close()
	var meta struct {
		Metadata struct {
			Title    string   `xml:"title"`
			Creators []string `xml:"creator"`
		} `xml:"metadata"`
	}
	if err := xml.NewDecoder(rc).Decode(&meta); err != nil || meta.Metadata.Title == "" {
		return ""
	}
	if len(meta.Metadata.Creators) > 0 && meta.Metadata.Creators[0] != "" {
		return meta.Metadata.Title + " — " + meta.Metadata.Creators[0]
	}
	return meta.Metadata.Title
}

// lrcDuration returns the last [mm:ss.xx] timestamp of an LRC as mm:ss.
func lrcDuration(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	re := regexp.MustCompile(`\[(\d+):(\d+(?:\.\d+)?)\]`)
	max := 0.0
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		mm, _ := strconv.ParseFloat(m[1], 64)
		ss, _ := strconv.ParseFloat(m[2], 64)
		if d := mm*60 + ss; d > max {
			max = d
		}
	}
	if max <= 0 {
		return "", false
	}
	return fmt.Sprintf("%02d:%05.2f", int(max)/60, max-float64(int(max)/60)*60), true
}

// vttDuration returns the latest cue end time of a WebVTT file as hh:mm:ss.
func vttDuration(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	re := regexp.MustCompile(`(\d{1,2}):(\d{2}):(\d{2})(?:\.\d+)?\s*-->`)
	max := 0.0
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		h, _ := strconv.ParseFloat(m[1], 64)
		mm, _ := strconv.ParseFloat(m[2], 64)
		ss, _ := strconv.ParseFloat(m[3], 64)
		if d := h*3600 + mm*60 + ss; d > max {
			max = d
		}
	}
	if max <= 0 {
		return "", false
	}
	return fmt.Sprintf("%02d:%02d:%02d", int(max)/3600, int(max)/60%60, int(max)%60), true
}

// srtDuration returns the latest cue end time of an SRT file as hh:mm:ss.
func srtDuration(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	re := regexp.MustCompile(`-->\s*(\d{1,2}):(\d{2}):(\d{2}),(\d{3})`)
	max := 0.0
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		h, _ := strconv.ParseFloat(m[1], 64)
		mm, _ := strconv.ParseFloat(m[2], 64)
		ss, _ := strconv.ParseFloat(m[3], 64)
		if d := h*3600 + mm*60 + ss; d > max {
			max = d
		}
	}
	if max <= 0 {
		return "", false
	}
	return formatHHMMSS(max), true
}

// assDuration returns the latest Dialogue end time of an ASS/SSA file.
func assDuration(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	re := regexp.MustCompile(`(?mi)^\s*dialogue:\s*[^,]*,[^,]*,\s*(\d+):(\d{2}):(\d{2})\.(\d{2})`)
	max := 0.0
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		h, _ := strconv.ParseFloat(m[1], 64)
		mm, _ := strconv.ParseFloat(m[2], 64)
		ss, _ := strconv.ParseFloat(m[3], 64)
		cs, _ := strconv.ParseFloat(m[4], 64)
		if d := h*3600 + mm*60 + ss + cs/100; d > max {
			max = d
		}
	}
	if max <= 0 {
		return "", false
	}
	return formatHHMMSS(max), true
}

// ttmlDuration returns the latest end="..." time of a TTML file.
func ttmlDuration(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	re := regexp.MustCompile(`\bend="(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,3}))?"`)
	max := 0.0
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		h, _ := strconv.ParseFloat(m[1], 64)
		mm, _ := strconv.ParseFloat(m[2], 64)
		ss, _ := strconv.ParseFloat(m[3], 64)
		ms := 0.0
		if len(m) > 4 && m[4] != "" {
			ms, _ = strconv.ParseFloat(m[4], 64)
			ms /= 1000
		}
		if d := h*3600 + mm*60 + ss + ms; d > max {
			max = d
		}
	}
	if max <= 0 {
		return "", false
	}
	return formatHHMMSS(max), true
}

// formatHHMMSS renders a seconds value as hh:mm:ss.
func formatHHMMSS(sec float64) string {
	return fmt.Sprintf("%02d:%02d:%02d", int(sec)/3600, int(sec)/60%60, int(sec)%60)
}

// DetectFormat classifies a transcript-style file for translation.
func DetectFormat(path string, content []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".lrc":
		return "lrc"
	case ".srt":
		return "srt"
	case ".vtt":
		return "vtt"
	case ".ass", ".ssa":
		return "ass"
	case ".ttml":
		return "ttml"
	case ".md", ".markdown":
		return "markdown"
	}
	return "text"
}

// ProtectedLines returns the structural tokens of a format that MUST survive
// a translation byte-for-byte: for LRC every [mm:ss(.xx)] timestamp in order
// (including on lyric lines); for VTT the timing lines and the WEBVTT header.
// Empty for plain text.
func ProtectedLines(format string, content []byte) []string {
	switch format {
	case "lrc":
		return lrcTimecodeRE.FindAllString(string(content), -1)
	case "srt":
		var out []string
		for _, line := range strings.Split(string(content), "\n") {
			if trim := strings.TrimSpace(line); strings.Contains(trim, "-->") {
				out = append(out, trim)
			}
		}
		return out
	case "vtt":
		var out []string
		for _, line := range strings.Split(string(content), "\n") {
			trim := strings.TrimSpace(line)
			if strings.Contains(trim, "-->") || trim == "WEBVTT" || strings.HasPrefix(trim, "NOTE") {
				out = append(out, trim)
			}
		}
		return out
	case "ass":
		var out []string
		diag := regexp.MustCompile(`(?i)^(?:dialogue|comment):`)
		for _, line := range strings.Split(string(content), "\n") {
			trim := strings.TrimSpace(line)
			if trim == "" {
				continue
			}
			if diag.MatchString(trim) {
				out = append(out, assDialoguePrefix(trim))
			} else {
				// Everything outside Dialogue/Comment lines (script info,
				// style headers, Format:, Style:) is structure — never
				// translated, must survive byte-for-byte.
				out = append(out, trim)
			}
		}
		return out
	case "ttml":
		// The ordered sequence of XML tags (with attributes) is the
		// structure; only text content between tags may change.
		return xmlTagRE.FindAllString(string(content), -1)
	}
	return nil
}

// assDialoguePrefix returns the Dialogue/Comment line prefix through the 9th
// comma — Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect —
// which must stay untouched; only the trailing text field is translated.
func assDialoguePrefix(line string) string {
	n := 0
	for i := 0; i < len(line); i++ {
		if line[i] == ',' {
			n++
			if n == 9 {
				return line[:i+1]
			}
		}
	}
	return line
}

// xmlTagRE matches an XML tag including attributes (used for TTML structure).
var xmlTagRE = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)

// lrcTimecodeRE matches an LRC timestamp tag like [01:23.45] or [1:02].
var lrcTimecodeRE = regexp.MustCompile(`\[\d{1,2}:\d{2}(?:\.\d+)?\]`)

// VerifyProtected checks that the protected lines of a translation are
// exactly the protected lines of the original.
func VerifyProtected(format string, original, translated string) error {
	orig := ProtectedLines(format, []byte(original))
	trans := ProtectedLines(format, []byte(translated))
	if len(orig) != len(trans) {
		return fmt.Errorf("protected line count changed (%d → %d); timestamps/headers must stay identical", len(orig), len(trans))
	}
	for i := range orig {
		if orig[i] != trans[i] {
			return fmt.Errorf("protected line %d changed:\n  original: %q\n  translated: %q", i+1, orig[i], trans[i])
		}
	}
	return nil
}
