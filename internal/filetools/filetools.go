// Package filetools implements the file tools the chat model can call:
// list_directory, read_file and edit_file. The contract is a single JSON
// object per turn — see Instructions — so parsing is deterministic and safe:
// extraction only accepts a well-formed object naming a known tool, and every
// file access is confined to a working directory.
package filetools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MaxReadBytes caps how much of a file is fed back to the model per read.
const MaxReadBytes = 48 * 1024

// MaxListEntries caps how many directory entries a list_directory result
// reports in one call.
const MaxListEntries = 200

// MaxIterations caps a model↔tool resolution loop.
const MaxIterations = 12

// Call is the JSON object the model emits to request a file operation.
type Call struct {
	Tool string `json:"tool"` // "list_directory" | "read_file" | "edit_file"
	Path string `json:"path"`
	Old  string `json:"old"` // edit_file: exact existing text (first occurrence replaced)
	New  string `json:"new"` // edit_file: replacement text
}

// Instructions returns the prompt fragment that defines the tools for the
// model. It is prepended to the user's prompt.
func Instructions(workdir string) string {
	return fmt.Sprintf(`[FILE TOOLS]
You can work with files inside %s by replying with ONLY a JSON object (no other text, no markdown):
  {"tool":"list_directory","path":"<dir>"}
  {"tool":"read_file","path":"<file>"}
  {"tool":"edit_file","path":"<file>","old":"<exact existing text>","new":"<replacement text>"}
Rules:
- Start with {"tool":"list_directory","path":"."} to see what is in %s. list_directory lists ONE directory non-recursively (directories are marked with a trailing /); drill into subdirectories by listing them, then read the files you need.
- Paths may be relative to %s or absolute inside it.
- Before editing, always read the file so "old" matches the current content EXACTLY (whitespace, quotes and indentation count); the first occurrence is replaced.
- After each tool call you receive a <tool_result> block. Reply with the next tool call, or — when done — with your final answer in plain text.
[END FILE TOOLS]

User: `, workdir, workdir, workdir)
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
	case "read_file", "list_directory":
		return c.Path != ""
	case "edit_file":
		return c.Path != "" && c.Old != ""
	}
	return false
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
func (c Call) Run(workdir string) string {
	switch c.Tool {
	case "list_directory":
		return c.runList(workdir)
	case "read_file":
		return c.runRead(workdir)
	case "edit_file":
		return c.runEdit(workdir)
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
	data, err := os.ReadFile(path)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	if int64(len(data)) > MaxReadBytes {
		data = data[:MaxReadBytes]
		return FormatResult(c.Tool, c.Path, fmt.Sprintf("WARNING: file truncated at %d bytes\n%s", MaxReadBytes, data))
	}
	return FormatResult(c.Tool, c.Path, string(data))
}

func (c Call) runEdit(workdir string) string {
	path, err := workdirPath(workdir, c.Path)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	content := string(data)
	count := strings.Count(content, c.Old)
	if count == 0 {
		return FormatResult(c.Tool, c.Path, "ERROR: pattern not found in file; re-read the file and copy the exact text")
	}
	replaced := strings.Replace(content, c.Old, c.New, 1)
	if err := os.WriteFile(path, []byte(replaced), 0644); err != nil {
		return FormatResult(c.Tool, c.Path, "ERROR: "+err.Error())
	}
	oldSnip, newSnip := c.Old, c.New
	if cut := 120; len(oldSnip) > cut {
		oldSnip = oldSnip[:cut] + "..."
	}
	if cut := 120; len(newSnip) > cut {
		newSnip = newSnip[:cut] + "..."
	}
	summary := fmt.Sprintf("replaced first of %d occurrences\nold: %q\nnew: %q", count, oldSnip, newSnip)
	return FormatResult(c.Tool, c.Path, summary)
}
