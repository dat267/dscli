package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dat267/dscli/internal/deepseek"
)

// TestAskAttachUploadsFiles: --attach uploads each file (with the size header
// the web client sends) and the completion carries the returned ids in
// ref_file_ids.
func TestAskAttachUploadsFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, rec := fakeDeepSeekServer(t)
	cmd := &AskCmd{
		Prompt:     []string{"summarise"},
		Attach:     []string{p},
		Token:      "tok",
		clientBase: srv.URL,
		cfgPath:    filepath.Join(dir, "cfg.json"),
	}
	if err := cmd.Run(nil, context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rec.mu.Lock()
	uploads := append([]string(nil), rec.uploads...)
	rec.mu.Unlock()
	if len(uploads) != 1 || uploads[0] != "5" {
		t.Errorf("uploads = %v, want one file of 5 bytes", uploads)
	}
	if got := completionRefIDs(t, rec, 0); len(got) != 1 || got[0] != "file-1" {
		t.Errorf("ref_file_ids = %v, want [file-1]", got)
	}
}

// TestReplAttachCommand: /attach uploads a file and attaches it to the next
// message only.
func TestReplAttachCommand(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, rec := fakeDeepSeekServer(t)
	c := deepseek.NewClient(deepseek.Session{Token: "tok"}, 0, srv.URL)
	cmd := &ChatCmd{Workdir: dir}

	withStdin(t, "/attach a.md\nfirst\nsecond\n/quit\n", func() {
		captureStdout(t, func() {
			captureStderr(t, func() {
				_ = cmd.replLoop(context.Background(), c, "sess-1", nil, false)
			})
		})
	})

	if got := completionRefIDs(t, rec, 0); len(got) != 1 || got[0] != "file-1" {
		t.Errorf("first message ref_file_ids = %v, want [file-1]", got)
	}
	if got := completionRefIDs(t, rec, 1); len(got) != 0 {
		t.Errorf("second message ref_file_ids = %v, want none", got)
	}
}

// TestAttachLimitsRejectBeforeUpload: the site's limits are enforced before
// anything is sent.
func TestAttachLimitsRejectBeforeUpload(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 0, deepseek.MaxAttachments+1)
	for i := 0; i <= deepseek.MaxAttachments; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%d", i))
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	// A nil client is safe: validation must fail before any request is made.
	if _, err := uploadAttachments(context.Background(), nil, paths, "default", false); err == nil {
		t.Fatal("51 attachments should be rejected before uploading")
	}
	// A missing file is a clear error, not a panic.
	if _, err := uploadAttachments(context.Background(), nil, []string{filepath.Join(dir, "nope")}, "default", false); err == nil {
		t.Fatal("a missing attachment should be rejected")
	}
}
