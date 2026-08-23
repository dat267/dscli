// Package filetools implements the file tools the chat model can call:
// list_directory, read_file, create_file, edit_file and delete_file. The
// contract is a single JSON object per turn — see Instructions — so parsing
// is deterministic and safe: extraction only accepts a well-formed object
// naming a known tool, and every file access is confined to a working
// directory, including through symlink chains.
package filetools

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultMaxReadBytes is the read_file size ceiling when --file-max-read is
// not given: 512 KiB — about a tenth of the model's ~1M-token context at
// typical code density, and the classic single-file "large file" cutoff.
// Files larger than the effective ceiling are rejected outright (never
// truncated), so only bounded text reaches the model context.
const DefaultMaxReadBytes = 512 * 1024

// MaxReadBytes is the runtime read_file ceiling. It is initialised to
// DefaultMaxReadBytes and may be tuned (the chat command's --file-max-read
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

// CapPrompt is appended as the final turn when the model exhausts
// MaxIterations tool calls without giving a prose answer.
const CapPrompt = "You have used all your file tool calls for this request. Do not call any more tools. Give your final answer now, based only on what you have already learned."

// isText reports whether data looks like a text file: no NUL byte in the
// leading probe window.
func isText(data []byte) bool {
	probe := data
	if len(probe) > binaryProbeSize {
		probe = probe[:binaryProbeSize]
	}
	return !bytes.Contains(probe, []byte{0})
}

// Call is the JSON object the model emits to request a file operation.
type Call struct {
	Tool      string `json:"tool"` // "list_directory" | "read_file" | "create_file" | "edit_file" | "rename_file" | "delete_file"
	Path      string `json:"path"`
	Old       string `json:"old"`       // edit_file: exact existing text (first occurrence replaced)
	New       string `json:"new"`       // edit_file: replacement text; rename_file: destination name/path
	Content   string `json:"content"`   // create_file: full content of the new file
	Recursive bool   `json:"recursive"` // list_directory: list the whole subtree at once
}

// Instructions returns the prompt fragment that defines the tools for the
// model. It is prepended to the user's prompt.
func Instructions(workdir string) string {
	return fmt.Sprintf(`[FILE TOOLS]
You can inspect and change files inside %s. To use a tool, reply with ONLY a JSON object (no other text, no markdown):
  {"tool":"list_directory","path":"<dir>"}
  {"tool":"read_file","path":"<file>"}
  {"tool":"create_file","path":"<file>","content":"<full content>"}
  {"tool":"edit_file","path":"<file>","old":"<exact existing text>","new":"<replacement text>"}
  {"tool":"rename_file","path":"<source>","new":"<new name or path>"}
  {"tool":"delete_file","path":"<file>"}
Rules:
- ACT, don't announce. When the user asks you to inspect or change files, perform the tool calls yourself in the same reply series; never reply with prose about what you are about to do ("let me...", "I'll...", "first I need to...").
- Budget: at most %d tool calls per user message; spend them on what matters, then give your final answer in prose.
- Every turn is exactly ONE of two things: a single tool-call JSON object, or — only when your task is fully complete — your final answer in plain text.
- To explore: start with {"tool":"list_directory","path":"."}, which lists ONE directory, non-recursive, directories marked with a trailing /, files with sizes. Add "recursive":true to get the whole subtree in one bounded call ({\"tool\":\"list_directory\",\"path\":\".\",\"recursive\":true}) instead of listing directory by directory — prefer it for exploring a tree.
- read_file only reads TEXT files up to %d bytes: larger or binary files are rejected — do not retry them, tell the user instead.
- To change: create_file makes a NEW file (it errors if the file already exists — then use edit_file or delete_file first); edit_file replaces the first exact occurrence of "old" (whitespace, quotes and indentation count — read the file first and copy from it); rename_file renames or moves a file/directory (the destination must not exist); delete_file removes a file permanently. Creating, renaming or deleting files asks the user for confirmation; if the user rejects, do not retry.
- After every tool call you receive a <tool_result> block. React to it with the next tool call, or your final answer. If an edit reports the pattern was not found, re-read the file and retry with the correct "old" text.
- Paths may be relative to %s or absolute inside it.
[END FILE TOOLS]

User: `, workdir, MaxIterations, MaxReadBytes, workdir)
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

func valid(c Call) bool {
	switch c.Tool {
	case "read_file", "list_directory", "create_file", "delete_file":
		return c.Path != ""
	case "edit_file":
		return c.Path != "" && c.Old != ""
	case "rename_file":
		return c.Path != "" && c.New != ""
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

// Run executes the call inside workdir and returns the <tool_result> text to
// feed back to the model (never a Go error: the outcome is for the model).
// Note: the write/delete variants apply immediately — the interactive flow
// that requires confirmation lives in the cmd layer (plan → preview →
// confirm → apply).
func (c Call) Run(workdir string) string {
	switch c.Tool {
	case "list_directory":
		return c.runList(workdir)
	case "read_file":
		return c.runRead(workdir)
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
	if !isText(data) {
		return FormatResult(c.Tool, c.Path, "ERROR: not a text file (binary content); read_file only reads text")
	}
	return FormatResult(c.Tool, c.Path, string(data))
}

// readEPUB extracts the chapter text of an EPUB (ZIP of XHTML) in spine
// order, stripped of markup, capped at MaxReadBytes of extracted text.
func (c Call) readEPUB(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: not a readable EPUB: "+err.Error())
	}
	chapters, err := epubChapters(zr)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: EPUB parse failed: "+err.Error())
	}
	var out strings.Builder
	fmt.Fprintf(&out, "EPUB text extracted from %s (%d sections)\n\n", c.Path, len(chapters))
	for _, ch := range chapters {
		text, err := stripHTML(ch)
		if err != nil {
			return FormatResult(c.Tool, c.Path, "ERROR: EPUB chapter parse failed: "+err.Error())
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
	return FormatResult(c.Tool, c.Path, out.String())
}

// epubChapters returns the XHTML files of an EPUB in spine order, reading
// META-INF/container.xml → OPF → manifest/spine.
func epubChapters(zr *zip.Reader) ([]string, error) {
	find := func(name string) (*zip.File, bool) {
		for _, f := range zr.File {
			if strings.EqualFold(f.Name, name) {
				return f, true
			}
		}
		return nil, false
	}
	container, ok := find("META-INF/container.xml")
	if !ok {
		return nil, fmt.Errorf("missing META-INF/container.xml")
	}
	rc, err := container.Open()
	if err != nil {
		return nil, err
	}
	var ctr struct {
		Rootfiles struct {
			Rootfile []struct {
				FullPath string `xml:"full-path,attr"`
			} `xml:"rootfile"`
		} `xml:"rootfiles"`
	}
	if err := xml.NewDecoder(rc).Decode(&ctr); err != nil {
		rc.Close()
		return nil, fmt.Errorf("container.xml: %w", err)
	}
	rc.Close()
	if len(ctr.Rootfiles.Rootfile) == 0 {
		return nil, fmt.Errorf("container.xml has no rootfile")
	}
	opfFile, ok := find(ctr.Rootfiles.Rootfile[0].FullPath)
	if !ok {
		return nil, fmt.Errorf("OPF %q not found", ctr.Rootfiles.Rootfile[0].FullPath)
	}
	rc, err = opfFile.Open()
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
		f, ok := find(name)
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
	if isText(data) {
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
