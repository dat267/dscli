package filetools

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	t.Run("create_file", func(t *testing.T) {
		c, ok := Extract(`{"tool":"create_file","path":"new.txt","content":"hi\nthere"}`)
		if !ok || c.Tool != "create_file" || c.Content != "hi\nthere" {
			t.Errorf("got %+v ok=%v", c, ok)
		}
	})
	t.Run("delete_file", func(t *testing.T) {
		c, ok := Extract(`{"tool":"delete_file","path":"old.txt"}`)
		if !ok || c.Tool != "delete_file" || c.Path != "old.txt" {
			t.Errorf("got %+v ok=%v", c, ok)
		}
	})
	t.Run("list_directory recursive", func(t *testing.T) {
		c, ok := Extract(`{"tool":"list_directory","path":".","recursive":true}`)
		if !ok || !c.Recursive {
			t.Errorf("got %+v ok=%v (Recursive=%v)", c, ok, c.Recursive)
		}
		c2, _ := Extract(`{"tool":"list_directory","path":"."}`)
		if c2.Recursive {
			t.Errorf("recursive must default to false: %+v", c2)
		}
	})
	t.Run("rename_file", func(t *testing.T) {
		c, ok := Extract(`{"tool":"rename_file","path":"a.txt","new":"b.txt"}`)
		if !ok || c.Tool != "rename_file" || c.New != "b.txt" {
			t.Errorf("got %+v ok=%v", c, ok)
		}
		if _, ok := Extract(`{"tool":"rename_file","path":"a.txt"}`); ok {
			t.Error("rename without a destination must be rejected")
		}
	})
	t.Run("grep", func(t *testing.T) {
		c, ok := Extract(`{"tool":"grep","pattern":"func \\w+","path":"cmd"}`)
		if !ok || c.Tool != "grep" || c.Pattern != `func \w+` || c.Path != "cmd" {
			t.Errorf("got %+v ok=%v", c, ok)
		}
		if _, ok := Extract(`{"tool":"grep"}`); ok {
			t.Error("grep without a pattern must be rejected")
		}
	})
	t.Run("fetch_url", func(t *testing.T) {
		c, ok := Extract(`{"tool":"fetch_url","url":"https://example.com/a"}`)
		if !ok || c.Tool != "fetch_url" || c.URL != "https://example.com/a" {
			t.Errorf("got %+v ok=%v", c, ok)
		}
		if _, ok := Extract(`{"tool":"fetch_url"}`); ok {
			t.Error("fetch_url without a URL must be rejected")
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

func TestRunReadTooLarge(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(f, []byte(strings.Repeat("x", MaxReadBytes+100)), 0644); err != nil {
		t.Fatal(err)
	}
	res := (Call{Tool: "read_file", Path: "big.txt"}).Run(dir)
	if !strings.Contains(res, "exceeding the max read size") {
		t.Errorf("expected size rejection, got %q", res)
	}
}

func TestRunReadRejectsBinary(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		data []byte
	}{
		{"nul prefix", []byte("\x7fELF\x00\x01")},
		{"nul inside", append([]byte("head\n"), append([]byte{0}, []byte("tail")...)...)},
	}
	for _, tc := range cases {
		f := filepath.Join(dir, tc.name)
		if err := os.WriteFile(f, tc.data, 0644); err != nil {
			t.Fatal(err)
		}
		res := (Call{Tool: "read_file", Path: tc.name}).Run(dir)
		if !strings.Contains(res, "not a text file") {
			t.Errorf("%s: expected binary rejection, got %q", tc.name, res)
		}
	}
	// A NUL past the probe window still counts as text (documented heuristic).
	late := bytes.Repeat([]byte{'a'}, binaryProbeSize+10)
	late[binaryProbeSize+9] = 0
	f := filepath.Join(dir, "late.txt")
	if err := os.WriteFile(f, late, 0644); err != nil {
		t.Fatal(err)
	}
	if res := (Call{Tool: "read_file", Path: "late.txt"}).Run(dir); strings.Contains(res, "not a text file") {
		t.Errorf("NUL beyond probe should not trigger binary rejection: %q", res)
	}
}

func TestRunReadSizeCeiling(t *testing.T) {
	dir := t.TempDir()
	orig := MaxReadBytes
	t.Cleanup(func() { MaxReadBytes = orig })
	MaxReadBytes = 16

	f := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(f, []byte("0123456789abcdef"), 0644); err != nil { // 16 bytes, allowed
		t.Fatal(err)
	}
	if res := (Call{Tool: "read_file", Path: "small.txt"}).Run(dir); strings.Contains(res, "ERROR") {
		t.Errorf("16-byte file must read under a 16-byte ceiling: %q", res)
	}
	if err := os.WriteFile(f, []byte("0123456789abcdefX"), 0644); err != nil { // 17 bytes
		t.Fatal(err)
	}
	if res := (Call{Tool: "read_file", Path: "small.txt"}).Run(dir); !strings.Contains(res, "exceeding the max read size") {
		t.Errorf("17-byte file must be rejected under a 16-byte ceiling: %q", res)
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
	for _, want := range []string{"list_directory", "read_file", "edit_file", "create_file", "delete_file", "grep", "fetch_url", "/tmp/proj", "TOOLS"} {
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
	if err := ApplyEdit(dir, c, p1); err != nil {
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
	if err := ApplyEdit(dir, c, p); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "one\n2\n3\n4\nfour\n" {
		t.Errorf("applied content = %q", got)
	}
}

func TestPlanCreate(t *testing.T) {
	dir := t.TempDir()
	c := Call{Tool: "create_file", Path: "sub/new.txt", Content: "hello\nworld"}

	p1 := PlanCreate(dir, c)
	if p1.Result != "" {
		t.Fatalf("plan failed: %q", p1.Result)
	}
	if !strings.Contains(p1.Preview, "sub/new.txt — creating (2 lines, 11 bytes)") {
		t.Errorf("preview header wrong:\n%s", p1.Preview)
	}
	for _, want := range []string{"+  1 │ hello", "+  2 │ world"} {
		if !strings.Contains(p1.Preview, want) {
			t.Errorf("preview missing %q:\n%s", want, p1.Preview)
		}
	}
	// Deterministic, and applying lands exactly the previewed content
	// (including auto-created parent directories).
	p2 := PlanCreate(dir, c)
	if p1.NewContent != p2.NewContent || p1.Preview != p2.Preview {
		t.Error("PlanCreate is not deterministic")
	}
	if err := ApplyCreate(dir, c, p1.NewContent); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "sub", "new.txt"))
	if err != nil || string(got) != "hello\nworld" {
		t.Errorf("created file = %q err=%v", got, err)
	}

	// Existing file: plan errors and nothing changes.
	before, _ := os.ReadFile(filepath.Join(dir, "sub", "new.txt"))
	if p := PlanCreate(dir, c); !strings.Contains(p.Result, "already exists") {
		t.Errorf("expected already-exists error, got %q", p.Result)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "sub", "new.txt"))
	if string(before) != string(after) {
		t.Errorf("existing file must not be touched by a failed create")
	}
}

func TestPlanDelete(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "gone.txt")
	if err := os.WriteFile(f, []byte("alpha\nbeta\ngamma\n"), 0644); err != nil {
		t.Fatal(err)
	}
	c := Call{Tool: "delete_file", Path: "gone.txt"}

	p1 := PlanDelete(dir, c)
	if p1.Result != "" {
		t.Fatalf("plan failed: %q", p1.Result)
	}
	if !strings.Contains(p1.Preview, "gone.txt — deleting (17 bytes)\n4 line(s)") {
		t.Errorf("preview header wrong:\n%s", p1.Preview)
	}
	if !strings.Contains(p1.Preview, "-  1 │ alpha") {
		t.Errorf("preview missing head:\n%s", p1.Preview)
	}
	if err := ApplyDelete(dir, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Errorf("file still exists after delete: %v", err)
	}

	// Missing file and directory targets error without deleting.
	if p := PlanDelete(dir, c); !strings.Contains(p.Result, "no such file") {
		t.Errorf("missing file should error, got %q", p.Result)
	}
	sub := filepath.Join(dir, "adirt")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if p := PlanDelete(dir, Call{Tool: "delete_file", Path: "adirt"}); !strings.Contains(p.Result, "directory") {
		t.Errorf("directory delete should error, got %q", p.Result)
	}
	if _, err := os.Stat(sub); err != nil {
		t.Errorf("directory must not be deleted")
	}
}

func TestCreateDeletePathEscapes(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"../x.txt", "/etc/passwd", ""} {
		if r := PlanCreate(dir, Call{Tool: "create_file", Path: p, Content: "x"}); !strings.Contains(r.Result, "ERROR") {
			t.Errorf("create path %q must be rejected, got %q", p, r.Result)
		}
		if r := PlanDelete(dir, Call{Tool: "delete_file", Path: p}); !strings.Contains(r.Result, "ERROR") {
			t.Errorf("delete path %q must be rejected, got %q", p, r.Result)
		}
	}
}

// TestSymlinkEscape: symlinks inside the workdir pointing outside must not
// let any tool escape the working directory.
func TestSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "sensitive.md"), []byte("top secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	// All five tools must refuse paths that resolve through the symlink.
	readRes := (Call{Tool: "read_file", Path: "escape/secret.txt"}).Run(dir)
	if !strings.Contains(readRes, "escapes") {
		t.Errorf("read through symlink must be rejected, got: %q", readRes)
	}
	listRes := (Call{Tool: "list_directory", Path: "escape"}).Run(dir)
	if !strings.Contains(listRes, "escapes") {
		t.Errorf("list through symlink must be rejected, got: %q", listRes)
	}
	if p := PlanEdit(dir, Call{Tool: "edit_file", Path: "escape/sensitive.md", Old: "x", New: "y"}); !strings.Contains(p.Result, "escapes") {
		t.Errorf("edit through symlink must be rejected, got: %q", p.Result)
	}
	if p := PlanCreate(dir, Call{Tool: "create_file", Path: "escape/new.txt", Content: "x"}); !strings.Contains(p.Result, "escapes") {
		t.Errorf("create through symlink must be rejected, got: %q", p.Result)
	}
	if p := PlanDelete(dir, Call{Tool: "delete_file", Path: "escape/secret.txt"}); !strings.Contains(p.Result, "escapes") {
		t.Errorf("delete through symlink must be rejected, got: %q", p.Result)
	}
	// Prove the symlink really reaches outside: the outside file must not be
	// readable or written.
	if data, err := os.ReadFile(filepath.Join(outside, "secret.txt")); err != nil || string(data) != "secret" {
		t.Errorf("outside state changed: %q %v", data, err)
	}

	// A symlink that stays INSIDE the workdir is fine.
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "inner.txt"), []byte("inside"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sub, filepath.Join(dir, "linkin")); err != nil {
		t.Fatal(err)
	}
	if res := (Call{Tool: "read_file", Path: "linkin/inner.txt"}).Run(dir); !strings.Contains(res, "inside") {
		t.Errorf("in-workdir symlink read failed: %q", res)
	}
}

// TestEditPreservesMode: applying an edit keeps the file's original mode
// instead of resetting it to 0644.
func TestEditPreservesMode(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "private.txt")
	if err := os.WriteFile(f, []byte("old content"), 0600); err != nil {
		t.Fatal(err)
	}
	c := Call{Tool: "edit_file", Path: "private.txt", Old: "old", New: "new"}
	plan := PlanEdit(dir, c)
	if plan.Result != "" {
		t.Fatal(plan.Result)
	}
	if err := ApplyEdit(dir, c, plan); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("mode after edit = %o, want 600", info.Mode().Perm())
	}
	got, _ := os.ReadFile(f)
	if string(got) != "new content" {
		t.Errorf("content after edit = %q", got)
	}
}

// TestApplyEditDetectsChange: a file modified between plan and apply must not
// be clobbered by the stale plan.
func TestApplyEditDetectsChange(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("AAA\nBBB\n"), 0644); err != nil {
		t.Fatal(err)
	}
	c := Call{Tool: "edit_file", Path: "a.txt", Old: "AAA", New: "AXA"}
	plan := PlanEdit(dir, c)
	if plan.Result != "" {
		t.Fatal(plan.Result)
	}
	// Someone edits the file after the preview.
	if err := os.WriteFile(f, []byte("AAA\nCHANGED\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ApplyEdit(dir, c, plan); err == nil {
		t.Fatal("expected stale-plan edit to be refused")
	}
	got, _ := os.ReadFile(f)
	if string(got) != "AAA\nCHANGED\n" {
		t.Errorf("file was clobbered: %q", got)
	}
}

// TestApplyCreateNoClobber: a file appearing after the plan must never be
// overwritten by ApplyCreate.
func TestApplyCreateNoClobber(t *testing.T) {
	dir := t.TempDir()
	c := Call{Tool: "create_file", Path: "new.txt", Content: "planned"}
	if p := PlanCreate(dir, c); p.Result != "" {
		t.Fatal(p.Result)
	}
	// A file appears after the preview.
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("someone else"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCreate(dir, c, "planned"); err == nil {
		t.Fatal("expected create to refuse overwriting a file that appeared")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "new.txt"))
	if string(got) != "someone else" {
		t.Errorf("existing file was overwritten: %q", got)
	}
}

// TestApplyDeleteVerifies: deleting never removes something that changed
// (or disappeared) since the plan.
func TestApplyDeleteVerifies(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "gone.txt")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	c := Call{Tool: "delete_file", Path: "gone.txt"}
	if p := PlanDelete(dir, c); p.Result != "" {
		t.Fatal(p.Result)
	}
	// Removed by someone else after the plan.
	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDelete(dir, c); err == nil {
		t.Fatal("expected delete to fail for a file that no longer exists")
	}
	// Replaced by a directory after the plan: must not delete the directory.
	if err := os.Mkdir(f, 0755); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDelete(dir, c); err == nil {
		t.Fatal("expected delete to refuse removing a directory")
	}
	if _, err := os.Stat(f); err != nil {
		t.Errorf("directory must not be removed: %v", err)
	}
}

func TestRunRename(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(f, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}

	c := Call{Tool: "rename_file", Path: "old.txt", New: "new.txt"}
	p := PlanRename(dir, c)
	if p.Result != "" {
		t.Fatalf("plan failed: %q", p.Result)
	}
	if !strings.Contains(p.Preview, "old.txt → new.txt") || !strings.Contains(p.Preview, "file") {
		t.Errorf("preview wrong:\n%s", p.Preview)
	}
	if err := ApplyRename(dir, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.txt")); !os.IsNotExist(err) {
		t.Error("old name must be gone after rename")
	}
	got, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil || string(got) != "content" {
		t.Errorf("renamed file = %q err=%v", got, err)
	}

	// Destination already taken → error, nothing changes.
	if err := os.WriteFile(filepath.Join(dir, "taken.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if p := PlanRename(dir, Call{Tool: "rename_file", Path: "new.txt", New: "taken.txt"}); !strings.Contains(p.Result, "already exists") {
		t.Errorf("expected destination-exists error, got %q", p.Result)
	}

	// Same path → error.
	if p := PlanRename(dir, Call{Tool: "rename_file", Path: "new.txt", New: "new.txt"}); !strings.Contains(p.Result, "same") {
		t.Errorf("expected same-path error, got %q", p.Result)
	}

	// Moving into a subdirectory renames across directories (still inside
	// the workdir).
	if p := PlanRename(dir, Call{Tool: "rename_file", Path: "new.txt", New: "sub/moved.txt"}); p.Result != "" {
		t.Fatalf("move plan failed: %q", p.Result)
	}
	if err := ApplyRename(dir, Call{Tool: "rename_file", Path: "new.txt", New: "sub/moved.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sub, "moved.txt")); err != nil {
		t.Errorf("move failed: %v", err)
	}

	// Renaming a directory works.
	if err := ApplyRename(dir, Call{Tool: "rename_file", Path: "sub", New: "folder"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "folder", "moved.txt")); err != nil {
		t.Errorf("dir rename failed: %v", err)
	}
}

func TestApplyRenameVerifies(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	c := Call{Tool: "rename_file", Path: "src.txt", New: "dst.txt"}
	if p := PlanRename(dir, c); p.Result != "" {
		t.Fatal(p.Result)
	}
	// Source vanishes after the plan → cannot rename.
	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	if err := ApplyRename(dir, c); err == nil {
		t.Fatal("expected rename to fail when the source disappeared")
	}
	// Destination appears after the plan → cannot clobber it.
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dst.txt"), []byte("other"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ApplyRename(dir, c); err == nil {
		t.Fatal("expected rename to refuse clobbering an appeared destination")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "dst.txt"))
	if string(got) != "other" {
		t.Errorf("destination was clobbered: %q", got)
	}
}

func TestRunReadEPUB(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "book.epub")
	if err := writeTestEPUB(f); err != nil {
		t.Fatal(err)
	}
	res := (Call{Tool: "read_file", Path: "book.epub"}).Run(dir)
	if !strings.Contains(res, "EPUB text extracted") {
		t.Errorf("expected EPUB extraction marker, got %q", res)
	}
	for _, want := range []string{"Chapter one content", "Chapter two content"} {
		if !strings.Contains(res, want) {
			t.Errorf("EPUB text missing %q:\n%s", want, res)
		}
	}
	if strings.Contains(res, "<p>") || strings.Contains(res, "<html") {
		t.Errorf("markup not stripped:\n%s", res)
	}
}

// writeTestEPUB builds a minimal EPUB: container.xml → OPF → two XHTML
// chapters.
func writeTestEPUB(path string) error {
	ch1 := "<html><body><h1>One</h1><p>Chapter one content &amp; stuff</p></body></html>"
	ch2 := "<html><body><p>Chapter two content</p></body></html>"
	files := map[string]string{
		"META-INF/container.xml": `<?xml version="1.0"?><container><rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`,
		"OEBPS/content.opf": `<?xml version="1.0"?><package><manifest>
			<item id="c1" href="ch1.xhtml" media-type="application/xhtml+xml"/>
			<item id="c2" href="ch2.xhtml" media-type="application/xhtml+xml"/>
			</manifest><spine>
			<itemref idref="c1"/><itemref idref="c2"/>
			</spine></package>`,
		"OEBPS/ch1.xhtml": ch1,
		"OEBPS/ch2.xhtml": ch2,
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(content)); err != nil {
			return err
		}
	}
	return zw.Close()
}

func TestRunListShowsSizes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, 1500), 0644); err != nil {
		t.Fatal(err)
	}
	res := (Call{Tool: "list_directory", Path: "."}).Run(dir)
	if !strings.Contains(res, "big.bin (1.5 KB)") {
		t.Errorf("listing missing humanised size: %q", res)
	}
	// Boundary sanity for humanSize itself.
	for n, want := range map[int64]string{
		500:        "500 B",
		1500:       "1.5 KB",
		3 << 20:    "3.0 MB",
		1536 << 20: "1.5 GB",
	} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestRunListRecursive(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "top.txt"), []byte("t"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"cmd", "internal/deepseek"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "cmd", "chat.go"), []byte("c"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "deepseek", "client.go"), []byte("d"), 0644); err != nil {
		t.Fatal(err)
	}

	res := (Call{Tool: "list_directory", Path: ".", Recursive: true}).Run(dir)
	for _, want := range []string{
		".\n",               // root marker
		"  cmd/\n",          // depth-1 dir
		"    chat.go (1 B)", // depth-2 file with size
		"  internal/",
		"    deepseek/",
		"      client.go (1 B)",
		"  top.txt (1 B)",
	} {
		if !strings.Contains(res, want) {
			t.Errorf("recursive listing missing %q:\n%s", want, res)
		}
	}
	if strings.Contains(res, "ERROR") {
		t.Errorf("unexpected error: %s", res)
	}

	// A nested root renders relative to the requested path.
	nested := (Call{Tool: "list_directory", Path: "internal", Recursive: true}).Run(dir)
	if !strings.Contains(nested, "internal/\n") || !strings.Contains(nested, "  deepseek/") {
		t.Errorf("nested root wrong:\n%s", nested)
	}
}

func TestRunListRecursiveBounded(t *testing.T) {
	dir := t.TempDir()
	// A deep chain far beyond the depth cap; the walk must stop at
	// MaxListDepth and never blow past the entry budget.
	cur := dir
	for i := 0; i < MaxListDepth+4; i++ {
		cur = filepath.Join(cur, fmt.Sprintf("d%d", i))
	}
	if err := os.MkdirAll(cur, 0755); err != nil {
		t.Fatal(err)
	}
	res := (Call{Tool: "list_directory", Path: ".", Recursive: true}).Run(dir)
	if strings.Contains(res, fmt.Sprintf("d%d", MaxListDepth+1)) {
		t.Errorf("descended past the depth cap:\n%s", res)
	}
	// Wide tree past the entry budget → truncation warning.
	wide := t.TempDir()
	for i := 0; i < MaxRecursiveEntries+50; i++ {
		if err := os.WriteFile(filepath.Join(wide, fmt.Sprintf("f%04d.txt", i)), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	res = (Call{Tool: "list_directory", Path: ".", Recursive: true}).Run(wide)
	if !strings.Contains(res, "tree truncated") {
		t.Errorf("expected truncation warning, got:\n%s", res[:min(400, len(res))])
	}
	if n := strings.Count(res, ".txt"); n > MaxRecursiveEntries+1 {
		t.Errorf("recursive listing too large: %d entries", n)
	}
}

func TestRunListRecursiveSymlinkNotDescended(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	res := (Call{Tool: "list_directory", Path: ".", Recursive: true}).Run(dir)
	if !strings.Contains(res, "escape") {
		t.Errorf("symlink entry not listed: %s", res)
	}
	if strings.Contains(res, "secret.txt") {
		t.Errorf("recursive listing escaped through a symlink:\n%s", res)
	}
}

func TestRunMetaFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "song.lrc"), []byte("[00:01.00]hello\n[00:05.50]world\n"), 0600); err != nil {
		t.Fatal(err)
	}
	res := (Call{Tool: "file_meta", Path: "song.lrc"}).Run(dir)
	for _, want := range []string{"kind: file", "size: 32 B (32 bytes)", "modified: 20", "mode: 0600", "duration: 00:05.50"} {
		if !strings.Contains(res, want) {
			t.Errorf("file meta missing %q:\n%s", want, res)
		}
	}
}

func TestRunMetaDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("12345"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.bin"), []byte("123"), 0644); err != nil {
		t.Fatal(err)
	}
	res := (Call{Tool: "file_meta", Path: "."}).Run(dir)
	if !strings.Contains(res, "kind: directory") || !strings.Contains(res, "entries: 3") {
		t.Errorf("dir meta wrong:\n%s", res)
	}
	if !strings.Contains(res, "total size: 8 B") {
		t.Errorf("total size wrong:\n%s", res)
	}
	// Path escape still applies.
	if r := (Call{Tool: "file_meta", Path: "../x"}).Run(dir); !strings.Contains(r, "ERROR") {
		t.Errorf("escape must be rejected: %q", r)
	}
}

func TestRunMetaEPUB(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "book.epub")
	if err := writeTestEPUB(f); err != nil {
		t.Fatal(err)
	}
	// The test EPUB has no dc:title; opacity check is just "no error".
	if res := (Call{Tool: "file_meta", Path: "book.epub"}).Run(dir); strings.Contains(res, "ERROR") {
		t.Errorf("epub meta errored: %s", res)
	}
}

func TestProtectedAndVerify(t *testing.T) {
	lrc := "[ti:Hello]\n[00:01.00]hello world\n[00:05.50]bye now\n"
	got := ProtectedLines("lrc", []byte(lrc))
	want := []string{"[00:01.00]", "[00:05.50]"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lrc protected = %v, want %v", got, want)
	}
	// Identical translation passes.
	if err := VerifyProtected("lrc", lrc, "[ti:Hola]\n[00:01.00]hola mundo\n[00:05.50]adiós\n"); err != nil {
		t.Errorf("valid lrc translation rejected: %v", err)
	}
	// A changed timestamp fails, even when the lyric text is fine.
	err := VerifyProtected("lrc", lrc, "[ti:Hello]\n[00:01.00]hola mundo\n[99:99.99]adiós\n")
	if err == nil || !strings.Contains(err.Error(), "protected line") {
		t.Errorf("changed timestamp must fail: %v", err)
	}

	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:04.000\nHi there\n"
	vp := ProtectedLines("vtt", []byte(vtt))
	if len(vp) != 2 || !strings.Contains(vp[0], "WEBVTT") || !strings.Contains(vp[1], "-->") {
		t.Errorf("vtt protected = %v", vp)
	}
	if err := VerifyProtected("vtt", vtt, "WEBVTT\n\n00:00:01.000 --> 00:00:04.000\nHola\n"); err != nil {
		t.Errorf("valid vtt translation rejected: %v", err)
	}
	if err := VerifyProtected("vtt", vtt, "WEBVTT\n\n00:00:09.000 --> 00:00:12.000\nHola\n"); err == nil {
		t.Error("changed vtt timing must fail")
	}
}

func TestDetectFormat(t *testing.T) {
	for path, want := range map[string]string{
		"a.txt": "text", "README.md": "markdown", "notes.markdown": "markdown",
		"song.lrc": "lrc", "movie.vtt": "vtt", "movie.srt": "srt",
		"sub.ass": "ass", "sub.ssa": "ass", "sub.ttml": "ttml",
	} {
		if got := DetectFormat(path, nil); got != want {
			t.Errorf("DetectFormat(%s) = %s, want %s", path, got, want)
		}
	}
}

func TestProtectedLinesAdditionalFormats(t *testing.T) {
	srt := "1\n00:00:01,000 --> 00:00:04,000\nHello\n\n2\n00:00:05,000 --> 00:00:08,000\nBye\n"
	p := ProtectedLines("srt", []byte(srt))
	if len(p) != 2 || p[0] != "00:00:01,000 --> 00:00:04,000" {
		t.Errorf("srt protected = %v", p)
	}
	if err := VerifyProtected("srt", srt, "1\n00:00:01,000 --> 00:00:04,000\nHola\n\n2\n00:00:05,000 --> 00:00:08,000\nAdios\n"); err != nil {
		t.Errorf("valid srt rejected: %v", err)
	}
	if err := VerifyProtected("srt", srt, "1\n00:00:01,500 --> 00:00:04,000\nHola\n"); err == nil {
		t.Error("changed srt timing must fail")
	}

	ass := "[Script Info]\nPlayResX: 1920\n\n[V4+ Styles]\nFormat: Name, Fontname\nStyle: Default,Arial\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Hello, there\n"
	p = ProtectedLines("ass", []byte(ass))
	if len(p) != 8 { // 7 header/style lines + 1 dialogue prefix
		t.Fatalf("ass protected = %d entries: %v", len(p), p)
	}
	pref := p[len(p)-1]
	if !strings.HasPrefix(pref, "Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,") {
		t.Errorf("dialogue prefix wrong: %q", pref)
	}
	// Valid ass translation: text field translated (commas inside text ok).
	good := "[Script Info]\nPlayResX: 1920\n\n[V4+ Styles]\nFormat: Name, Fontname\nStyle: Default,Arial\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Hola, amigo\n"
	if err := VerifyProtected("ass", ass, good); err != nil {
		t.Errorf("valid ass rejected: %v", err)
	}
	// A changed start time in a Dialogue prefix fails.
	bad := "[Script Info]\nPlayResX: 1920\n\n[V4+ Styles]\nFormat: Name, Fontname\nStyle: Default,Arial\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,9:99:01.00,0:00:04.00,Default,,0,0,0,,Hola\n"
	if err := VerifyProtected("ass", ass, bad); err == nil {
		t.Error("changed ass timing must fail")
	}
	// Dropping the header fails (count mismatch).
	if err := VerifyProtected("ass", ass, "[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Hola\n"); err == nil {
		t.Error("dropped ass header must fail")
	}

	ttml := `<tt xmlns="http://www.w3.org/ns/ttml"><body><div><p begin="00:00:01.000" end="00:00:04.000">Hello</p></div></body></tt>`
	p = ProtectedLines("ttml", []byte(ttml))
	if len(p) != 6 { // tt, body, div, p, /p, /div, /body, /tt = 8 actually
		t.Logf("ttml protected = %v", p)
	}
	if err := VerifyProtected("ttml", ttml, `<tt xmlns="http://www.w3.org/ns/ttml"><body><div><p begin="00:00:01.000" end="00:00:04.000">Hola</p></div></body></tt>`); err != nil {
		t.Errorf("valid ttml rejected: %v", err)
	}
	if err := VerifyProtected("ttml", ttml, `<tt xmlns="http://www.w3.org/ns/ttml"><body><div><p begin="00:00:01.000" end="00:00:05.000">Hola</p></div></body></tt>`); err == nil {
		t.Error("changed ttml attribute must fail")
	}
}

func TestRunMetaAdditionalDurations(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "m.srt"), []byte("1\n00:00:01,000 --> 00:00:03,500\nHi\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if res := (Call{Tool: "file_meta", Path: "m.srt"}).Run(dir); !strings.Contains(res, "duration: 00:00:03") {
		t.Errorf("srt duration missing: %s", res)
	}
	if err := os.WriteFile(filepath.Join(dir, "m.ass"), []byte("[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:02:30.00,Default,,0,0,0,,Hi\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if res := (Call{Tool: "file_meta", Path: "m.ass"}).Run(dir); !strings.Contains(res, "duration: 00:02:30") {
		t.Errorf("ass duration missing: %s", res)
	}
	if err := os.WriteFile(filepath.Join(dir, "m.ttml"), []byte(`<tt><p begin="00:00:01.000" end="00:01:00.500">Hi</p></tt>`), 0644); err != nil {
		t.Fatal(err)
	}
	if res := (Call{Tool: "file_meta", Path: "m.ttml"}).Run(dir); !strings.Contains(res, "duration: 00:01:00") {
		t.Errorf("ttml duration missing: %s", res)
	}
}

func TestRunGrep(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"a.go":        "package main\n\nfunc main() {}\n",
		"b.txt":       "hello world\nhello again\n",
		"nested/c.go": "func helper() {}\n",
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// A bare pattern searches the whole workdir (path defaults to ".").
	res := (Call{Tool: "grep", Pattern: "func"}).Run(dir)
	for _, want := range []string{"a.go:3:func main() {}", "nested/c.go:1:func helper() {}"} {
		if !strings.Contains(res, want) {
			t.Errorf("grep missing %q:\n%s", want, res)
		}
	}
	if strings.Contains(res, "hello") {
		t.Errorf("grep matched prose unexpectedly:\n%s", res)
	}
	if strings.Contains(res, "ERROR") {
		t.Errorf("unexpected error: %s", res)
	}

	// A single-file path narrows the search; grep is case-sensitive unless
	// the pattern opts into (?i).
	res = (Call{Tool: "grep", Pattern: "HELLO", Path: "b.txt"}).Run(dir)
	if !strings.Contains(res, "(no matches)") {
		t.Errorf("case-sensitive grep matched: %s", res)
	}
	res = (Call{Tool: "grep", Pattern: "(?i)HELLO", Path: "b.txt"}).Run(dir)
	if !strings.Contains(res, "b.txt:1:hello world") {
		t.Errorf("case-insensitive grep failed: %s", res)
	}

	// A missing path, a broken pattern and a path escape all error.
	if r := (Call{Tool: "grep", Pattern: "x", Path: "nope"}).Run(dir); !strings.Contains(r, "ERROR") {
		t.Errorf("missing path must error: %q", r)
	}
	if r := (Call{Tool: "grep", Pattern: "("}).Run(dir); !strings.Contains(r, "ERROR") {
		t.Errorf("bad regex must error: %q", r)
	}
	if r := (Call{Tool: "grep", Pattern: "x", Path: "../escape"}).Run(dir); !strings.Contains(r, "ERROR") {
		t.Errorf("escape must be rejected: %q", r)
	}
}

func TestRunGrepSkipsNoise(t *testing.T) {
	dir := t.TempDir()
	// VCS metadata directories are never searched.
	if err := os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("secret = x"), 0644); err != nil {
		t.Fatal(err)
	}
	// Binary and oversized files are skipped, like read_file.
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0x00, 0x01, 0x02, 'x'}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Repeat("needle\n", MaxReadBytes/5)), 0644); err != nil {
		t.Fatal(err)
	}
	res := (Call{Tool: "grep", Pattern: "secret|needle"}).Run(dir)
	if strings.Contains(res, "secret") {
		t.Errorf(".git was searched:\n%s", res)
	}
	if strings.Contains(res, "needle") {
		t.Errorf("oversized file was searched:\n%s", res)
	}
	if !strings.Contains(res, "(no matches)") {
		t.Errorf("expected no matches, got:\n%s", res)
	}
}

func TestRunGrepTruncates(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < maxGrepMatches+50; i++ {
		fmt.Fprintf(&b, "match line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "many.txt"), []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
	res := (Call{Tool: "grep", Pattern: "match"}).Run(dir)
	if !strings.Contains(res, "truncated at") {
		t.Errorf("expected truncation warning:\n%s", res)
	}
	if n := strings.Count(res, "many.txt:"); n > maxGrepMatches+1 {
		t.Errorf("grep returned %d lines, want ≤ %d", n, maxGrepMatches)
	}
}

func TestRunGrepSymlinkNotFollowed(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	res := (Call{Tool: "grep", Pattern: "secret"}).Run(dir)
	if strings.Contains(res, "secret") {
		t.Errorf("grep escaped through a symlink:\n%s", res)
	}
}

func TestRunFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			_, _ = io.WriteString(w, "Hello from the web page")
		case "/redirect":
			http.Redirect(w, r, "/page", http.StatusFound)
		case "/binary":
			_, _ = w.Write([]byte{0x00, 0x01, 0x02, 'x'})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	res := (Call{Tool: "fetch_url", URL: srv.URL + "/page"}).Run("")
	if !strings.Contains(res, "Hello from the web page") {
		t.Errorf("fetch missing content: %q", res)
	}
	if !strings.Contains(res, "HTTP 200 OK") {
		t.Errorf("fetch missing status header: %q", res)
	}
	if strings.Contains(res, "ERROR") {
		t.Errorf("unexpected error: %q", res)
	}

	// Redirects are followed and the body lands.
	res = (Call{Tool: "fetch_url", URL: srv.URL + "/redirect"}).Run("")
	if !strings.Contains(res, "Hello from the web page") {
		t.Errorf("redirect not followed: %q", res)
	}

	// Non-2xx responses and binary bodies are rejected.
	res = (Call{Tool: "fetch_url", URL: srv.URL + "/missing"}).Run("")
	if !strings.Contains(res, "ERROR") || !strings.Contains(res, "404") {
		t.Errorf("404 must error: %q", res)
	}
	res = (Call{Tool: "fetch_url", URL: srv.URL + "/binary"}).Run("")
	if !strings.Contains(res, "not text") {
		t.Errorf("binary body must be rejected: %q", res)
	}

	// Non-http(s) schemes and empty hosts are rejected without any request.
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/x", "not-a-url"} {
		res = (Call{Tool: "fetch_url", URL: u}).Run("")
		if !strings.Contains(res, "ERROR") {
			t.Errorf("URL %q must be rejected: %q", u, res)
		}
	}
}

func TestRunFetchSizeCap(t *testing.T) {
	orig := MaxReadBytes
	t.Cleanup(func() { MaxReadBytes = orig })
	MaxReadBytes = 64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("q", 128)))
	}))
	defer srv.Close()
	res := (Call{Tool: "fetch_url", URL: srv.URL}).Run("")
	if !strings.Contains(res, "truncated at 64 bytes") {
		t.Errorf("expected truncation note: %q", res)
	}
	if n := strings.Count(res, "q"); n > 64 {
		t.Errorf("body not capped at MaxReadBytes: %d q's", n)
	}
}
