package filetools

import (
	"bytes"
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
	for _, want := range []string{"list_directory", "read_file", "edit_file", "create_file", "delete_file", "/tmp/proj", "FILE TOOLS"} {
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
