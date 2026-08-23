// Package filetools implements the file tools the chat model can call:
// list_directory, read_file and edit_file. The contract is a single JSON
// object per turn — see Instructions — so parsing is deterministic and safe:
// extraction only accepts a well-formed object naming a known tool, and every
// file access is confined to a working directory.
package filetools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// MaxListEntries caps how many directory entries a list_directory result
// reports in one call.
const MaxListEntries = 200

// MaxIterations caps a model↔tool resolution loop.
const MaxIterations = 12

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
	Tool    string `json:"tool"` // "list_directory" | "read_file" | "edit_file" | "create_file" | "delete_file"
	Path    string `json:"path"`
	Old     string `json:"old"`     // edit_file: exact existing text (first occurrence replaced)
	New     string `json:"new"`     // edit_file: replacement text
	Content string `json:"content"` // create_file: full content of the new file
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
  {"tool":"delete_file","path":"<file>"}
Rules:
- ACT, don't announce. When the user asks you to inspect or change files, perform the tool calls yourself in the same reply series; never reply with prose about what you are about to do ("let me...", "I'll...", "first I need to...").
- Every turn is exactly ONE of two things: a single tool-call JSON object, or — only when your task is fully complete — your final answer in plain text.
- To explore: start with {"tool":"list_directory","path":"."}, which lists ONE directory, non-recursive, directories marked with a trailing /; drill into subdirectories, then read what you need.
- read_file only reads TEXT files up to %d bytes: larger or binary files are rejected — do not retry them, tell the user instead.
- To change: create_file makes a NEW file (it errors if the file already exists — then use edit_file or delete_file first); edit_file replaces the first exact occurrence of "old" (whitespace, quotes and indentation count — read the file first and copy from it); delete_file removes a file permanently. Creating or deleting files asks the user for confirmation; if the user rejects, do not retry.
- After every tool call you receive a <tool_result> block. React to it with the next tool call, or your final answer. If an edit reports the pattern was not found, re-read the file and retry with the correct "old" text.
- Paths may be relative to %s or absolute inside it.
[END FILE TOOLS]

User: `, workdir, MaxReadBytes, workdir)
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
	}
	return false
}

// Display renders a path for the user: relative to the workdir when it lies
// inside it, otherwise as given. Used for preview headers and confirmations.
func Display(workdir, p string) string {
	path, err := workdirPath(workdir, p)
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(workdir, path)
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
// The check is textual (Clean + Rel): it does not follow symlinks.
func workdirPath(workdir, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	full := p
	if !filepath.IsAbs(full) {
		full = filepath.Join(workdir, p)
	}
	full = filepath.Clean(full)
	rel, err := filepath.Rel(workdir, full)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the working directory", p)
	}
	return full, nil
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
	entries, err := os.ReadDir(path)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	var lines []string
	truncated := false
	for i, e := range entries {
		if i >= MaxListEntries {
			truncated = true
			break
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		lines = append(lines, name)
	}
	body := strings.Join(lines, "\n")
	if truncated {
		body += fmt.Sprintf("\nWARNING: directory truncated at %d entries", MaxListEntries)
	}
	if body == "" {
		body = "(empty directory)"
	}
	return FormatResult(c.Tool, c.Path, body)
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

func (c Call) runEdit(workdir string) string {
	plan := PlanEdit(workdir, c)
	if plan.Result != "" {
		return FormatResult(c.Tool, c.Path, plan.Result)
	}
	if err := ApplyEdit(workdir, c, plan.NewContent); err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	return FormatResult(c.Tool, c.Path, EditSummary(plan.Count, c.Old, c.New))
}

// EditPlan is the deterministic outcome of planning an edit_file call: the
// exact content ApplyEdit will write, a line-numbered preview of the change,
// and — when the edit cannot be applied — the ERROR line to feed the model.
type EditPlan struct {
	NewContent string // file content after the first-occurrence replacement
	Preview    string // user-facing diff preview ("" when Result != "")
	Count      int    // total occurrences of the pattern in the file
	Result     string // non-empty means the plan failed; NewContent/Preview
	// are then empty and Result is a model-facing ERROR line
}

// PlanEdit resolves and reads the file, verifies the pattern exists, and
// computes the EXACT new content that ApplyEdit will later write — the
// preview and the write are derived from the same bytes, so what the user
// approves is precisely what lands on disk. It performs no write.
func PlanEdit(workdir string, c Call) EditPlan {
	path, err := workdirPath(workdir, c.Path)
	if err != nil {
		return EditPlan{Result: "ERROR: " + err.Error()}
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
		NewContent: strings.Replace(content, c.Old, c.New, 1),
		Preview:    buildPreview(c.Path, content, c.Old, c.New, count),
		Count:      count,
	}
}

// ApplyEdit writes the new content produced by PlanEdit. It performs exactly
// the first-occurrence replacement the preview described.
func ApplyEdit(workdir string, c Call, newContent string) error {
	path, err := workdirPath(workdir, c.Path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(newContent), 0644)
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
// workdir), with the exact content the plan previewed.
func ApplyCreate(workdir string, c Call, content string) error {
	path, err := workdirPath(workdir, c.Path)
	if err != nil {
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

// ApplyDelete removes the file the plan previewed.
func ApplyDelete(workdir string, c Call) error {
	path, err := workdirPath(workdir, c.Path)
	if err != nil {
		return err
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
