package cmd

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/dat267/dscli/internal/deepseek"
)

func TestRunFilesSequentialRunsInOrderAndStopsAtFirstError(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	clients := 0
	errBoom := errors.New("boom")
	err := runFiles(context.Background(), []string{"a", "b", "c"}, false,
		func() *deepseek.Client { clients++; return nil },
		func(_ context.Context, _ *deepseek.Client, file string) error {
			mu.Lock()
			ran = append(ran, file)
			mu.Unlock()
			if file == "b" {
				return errBoom
			}
			return nil
		})
	if !errors.Is(err, errBoom) {
		t.Errorf("err = %v, want the b failure", err)
	}
	if len(ran) != 2 || ran[0] != "a" || ran[1] != "b" {
		t.Errorf("ran = %v, want [a b] (stop at the first error)", ran)
	}
	if clients != 1 {
		t.Errorf("clients = %d, want 1 (sequential reuses one client)", clients)
	}
}

func TestRunFilesParallelRunsEveryFileWithOwnClient(t *testing.T) {
	var mu sync.Mutex
	ran := map[string]bool{}
	clients := 0
	errBoom := errors.New("boom")
	err := runFiles(context.Background(), []string{"a", "b", "c"}, true,
		func() *deepseek.Client { mu.Lock(); clients++; mu.Unlock(); return nil },
		func(_ context.Context, _ *deepseek.Client, file string) error {
			mu.Lock()
			ran[file] = true
			mu.Unlock()
			if file == "b" {
				return errBoom
			}
			return nil
		})
	if !errors.Is(err, errBoom) {
		t.Errorf("err = %v, want an error", err)
	}
	if clients != 3 {
		t.Errorf("clients = %d, want 3 (one per file)", clients)
	}
	if !ran["a"] || !ran["b"] || !ran["c"] {
		t.Errorf("not every file ran: %v", ran)
	}
}

func TestRunFilesParallelNoError(t *testing.T) {
	err := runFiles(context.Background(), []string{"a", "b"}, true,
		func() *deepseek.Client { return nil },
		func(_ context.Context, _ *deepseek.Client, _ string) error { return nil })
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}
