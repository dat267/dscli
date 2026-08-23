package filetools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtract(t *testing.T) {
	t.Run("exact json", func(t *testing.T) {
		c, ok := Extract(`{"tool":"read_file","path":"src/main.go"}`)
		if !ok || c.Tool != "read_file" || c.Path != "src/main.go" {
			t.Errorf("got %+v ok=%v", c, ok)
		}
	})
	t.Run("fenced json", func(t *testing.T) {
		reply := "Here is the call:\n```json\n{\"tool\":\"edit_file\",\"path\":\"a.go\",\"old\":\"x\",\"new\":\"y\"}\n```"
		c, ok := Extract(reply)
		if !ok || c.Tool != "edit_file" || c.Old != "x" || c.New != "y" {
			t.Errorf("got %+v ok=%v", c, ok)
		}
	})
	t.Run("prose then json", func(t *testing.T) {
		reply := "I need to look at this:\n{\"tool\":\"read_file\",\"path\":\"internal/x\"}\nsome trailing prose"
		c, ok := Extract(reply)
		if !ok || c.Tool != "read_file" || c.Path != "internal/x" {
			t.Errorf("brace-scan got %+v ok=%v", c, ok)
		}
	})
	t.Run("escaped quotes inside json", func(t *testing.T) {
		c, ok := Extract(`{"tool":"edit_file","path":"a.go","old":"say \"hi\"","new":"say \"bye\""}`)
		if !ok || c.Old != `say "hi"` {
			t.Errorf("got %+v old=%q", c, c.Old)
		}
	})
	t.Run("list_directory", func(t *testing.T) {
		c, ok := Extract(`{"tool":"list_directory","path":"."}`)
		if !ok || c.Tool != "list_directory" || c.Path != "." {
			t.Errorf("got %+v ok=%v", c, ok)
		}
	})
	t.Run("prose is not a call", func(t *testing.T) {
		for _, reply := range []string{
			"Hello! How can I help?",
			"the tool is {\"tool\":\"nope\",\"path\":\"x\"}",
			`{"tool":"read_file"}`,
			`{"tool":"edit_file","path":"a"}`,
			"not json at all { broken",
		} {
			if _, ok := Extract(reply); ok {
				t.Errorf("expected prose for %q", reply)
			}
		}
	})
}

func TestRunRead(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("hello file"), 0644); err != nil {
		t.Fatal(err)
	}
	c := Call{Tool: "read_file", Path: "a.txt"}
	res := c.Run(dir)
	if !strings.Contains(res, "hello file") {
		t.Errorf("read result missing content: %q", res)
	}
}

func TestRunReadTruncates(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(f, []byte(strings.Repeat("x", MaxReadBytes+100)), 0644); err != nil {
		t.Fatal(err)
	}
	res := (Call{Tool: "read_file", Path: "big.txt"}).Run(dir)
	if !strings.Contains(res, "truncated") {
		t.Errorf("expected truncation warning: %q", res)
	}
	if n := strings.Count(res, "x"); n >= MaxReadBytes+100 {
		t.Errorf("result too large: %d bytes", n)
	}
}

func TestRunEdit(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("aaa\nbbb\naaa\n"), 0644); err != nil {
		t.Fatal(err)
	}
	c := Call{Tool: "edit_file", Path: "a.txt", Old: "aaa", New: "AXA"}
	res := c.Run(dir)
	if !strings.Contains(res, "replaced first of 2 occurrences") {
		t.Errorf("summary wrong: %q", res)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "AXA\nbbb\naaa\n" {
		t.Errorf("file after edit = %q", got)
	}
}

func TestRunEditPatternNotFound(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("abc"), 0644); err != nil {
		t.Fatal(err)
	}
	res := (Call{Tool: "edit_file", Path: "a.txt", Old: "zzz", New: "q"}).Run(dir)
	if !strings.Contains(res, "pattern not found") {
		t.Errorf("expected pattern-not-found result: %q", res)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "abc" {
		t.Errorf("file must not change on failure, got %q", got)
	}
}

func TestRunEditMissingFile(t *testing.T) {
	res := (Call{Tool: "edit_file", Path: "nope.txt", Old: "x", New: "y"}).Run(t.TempDir())
	if !strings.HasPrefix(res, "<tool_result") || !strings.Contains(res, "ERROR") {
		t.Errorf("expected error result, got %q", res)
	}
}

func TestRunList(t *testing.T) {
	dir := t.TempDir()
	for name := range map[string]struct {
		isDir bool
	}{
		"a.txt":     {},
		"cmd":       {isDir: true},
		"README.md": {},
	} {
		if name == "cmd" {
			if err := os.Mkdir(filepath.Join(dir, name), 0755); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	res := (Call{Tool: "list_directory", Path: "."}).Run(dir)
	for _, want := range []string{"a.txt", "cmd/", "README.md"} {
		if !strings.Contains(res, want) {
			t.Errorf("listing missing %q:\n%s", want, res)
		}
	}
	if strings.Contains(res, "ERROR") {
		t.Errorf("unexpected error: %s", res)
	}

	// A nested listing works and a missing directory errors.
	res = (Call{Tool: "list_directory", Path: "cmd"}).Run(dir)
	if strings.Contains(res, "ERROR") {
		t.Errorf("nested listing failed: %s", res)
	}
	res = (Call{Tool: "list_directory", Path: "nope"}).Run(dir)
	if !strings.Contains(res, "ERROR") {
		t.Errorf("missing directory should error: %s", res)
	}
}

func TestRunListTruncates(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < MaxListEntries+50; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	res := (Call{Tool: "list_directory", Path: "."}).Run(dir)
	if !strings.Contains(res, "truncated") {
		t.Errorf("expected truncation warning: %s", res)
	}
	if n := strings.Count(res, ".txt"); n >= MaxListEntries+50 {
		t.Errorf("listing too large: %d entries", n)
	}
}

func TestPathEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{
		"../escape.txt",
		"../",
		"a/../../escape.txt",
		"/etc/passwd", // absolute outside the workdir
		"/etc",
		"",
	} {
		for _, tool := range []string{"read_file", "list_directory", "edit_file"} {
			res := (Call{Tool: tool, Path: p, Old: "x", New: "y"}).Run(dir)
			if !strings.Contains(res, "ERROR") {
				t.Errorf("path %q with tool %s must be rejected, got %q", p, tool, res)
			}
		}
	}
	// Absolute path inside the workdir is fine.
	inside := filepath.Join(dir, "ok.txt")
	if err := os.WriteFile(inside, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	res := (Call{Tool: "read_file", Path: inside}).Run(dir)
	if !strings.Contains(res, "ok") {
		t.Errorf("absolute in-workdir path rejected: %q", res)
	}
}

func TestInstructionsMentionsTools(t *testing.T) {
	ins := Instructions("/tmp/proj")
	for _, want := range []string{"list_directory", "read_file", "edit_file", "/tmp/proj", "FILE TOOLS"} {
		if !strings.Contains(ins, want) {
			t.Errorf("instructions missing %q", want)
		}
	}
}

func TestPlanEditPreview(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("module x\n\nrequire y\nmodule x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	c := Call{Tool: "edit_file", Path: "a.txt", Old: "require y", New: "require z"}

	p1 := PlanEdit(dir, c)
	if p1.Result != "" || p1.Count != 1 {
		t.Fatalf("plan: result=%q count=%d", p1.Result, p1.Count)
	}
	// Deterministic: planning twice yields the identical content and preview.
	p2 := PlanEdit(dir, c)
	if p1.NewContent != p2.NewContent || p1.Preview != p2.Preview {
		t.Error("PlanEdit is not deterministic across calls")
	}
	// The preview mirrors the actual first-occurrence replacement.
	for _, want := range []string{"replacing first of 1 occurrence(s)", "-  3 │ require y", "+  3 │ require z"} {
		if !strings.Contains(p1.Preview, want) {
			t.Errorf("preview missing %q:\n%s", want, p1.Preview)
		}
	}
	// Unified-diff order: removal, then replacement IN PLACE, then context —
	// never the replacement after the trailing context.
	if i, j, k := strings.Index(p1.Preview, "-  3 │ require y"),
		strings.Index(p1.Preview, "+  3 │ require z"),
		strings.Index(p1.Preview, "   4 │ module x"); i < 0 || j < 0 || k < 0 || !(i < j && j < k) {
		t.Errorf("preview lines out of order (got %d, %d, %d):\n%s", i, j, k, p1.Preview)
	}

	// Applying the planned content lands exactly the previewed change.
	if err := ApplyEdit(dir, c, p1.NewContent); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "module x\n\nrequire z\nmodule x\n" {
		t.Errorf("applied content = %q", got)
	}
}

func TestPlanEditMultiLinePreview(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("one\ntwo\nthree\nfour\n"), 0644); err != nil {
		t.Fatal(err)
	}
	c := Call{Tool: "edit_file", Path: "a.txt", Old: "two\nthree", New: "2\n3\n4"}
	p := PlanEdit(dir, c)
	if p.Result != "" {
		t.Fatalf("plan failed: %q", p.Result)
	}
	if !strings.Contains(p.Preview, "-  2 │ two") || !strings.Contains(p.Preview, "-  3 │ three") {
		t.Errorf("multi-line old not marked in preview:\n%s", p.Preview)
	}
	if !strings.Contains(p.Preview, "+  2 │ 2") || !strings.Contains(p.Preview, "+  3 │ 3") || !strings.Contains(p.Preview, "+  4 │ 4") {
		t.Errorf("multi-line new not shown in preview:\n%s", p.Preview)
	}
	if err := ApplyEdit(dir, c, p.NewContent); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "one\n2\n3\n4\nfour\n" {
		t.Errorf("applied content = %q", got)
	}
}
