package cmd

import (
	"context"

	"github.com/dat267/dscli/internal/deepseek"
)

// runFiles runs run for each file, either sequentially over one shared
// client (stopping at the first error) or concurrently with a fresh client
// per file (each in its own session; summaries/replies may interleave).
// Returns the first error encountered.
func runFiles(ctx context.Context, files []string, parallel bool, newClient func() *deepseek.Client, run func(ctx context.Context, client *deepseek.Client, file string) error) error {
	if !parallel {
		client := newClient()
		for _, file := range files {
			if err := run(ctx, client, file); err != nil {
				return err
			}
		}
		return nil
	}
	type result struct{ err error }
	results := make(chan result, len(files))
	for _, file := range files {
		go func(file string) {
			results <- result{run(ctx, newClient(), file)}
		}(file)
	}
	var firstErr error
	for range files {
		if err := (<-results).err; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
