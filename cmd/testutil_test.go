package cmd

import (
	"bytes"
	"os"
	"testing"
)

// captureStdout runs fn and returns everything written to os.Stdout.
// It is only safe for tests that do not run in parallel, since it
// redirects the process-global stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	return capture(t, os.Stdout, func() *os.File { return os.Stdout }, func(f *os.File) { os.Stdout = f }, fn)
}

// captureStderr runs fn and returns everything written to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	return capture(t, os.Stderr, func() *os.File { return os.Stderr }, func(f *os.File) { os.Stderr = f }, fn)
}

func capture(t *testing.T, _ *os.File, get func() *os.File, set func(*os.File), fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	old := get()
	set(w)
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("failed to close pipe writer: %v", err)
	}
	set(old)
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("failed to read captured output: %v", err)
	}
	return buf.String()
}
