package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dat267/dscli/internal/deepseek"
)

// uploadAttachments reads and uploads the given files to DeepSeek, returning
// their server ids in order. File sizes are validated against the site's
// attachment limits (at most 50 files, 100 MB each) before anything is sent.
// model and thinking are carried in the x-model-type/x-thinking-enabled
// headers the web client sends alongside the upload's PoW header.
func uploadAttachments(ctx context.Context, client *deepseek.Client, paths []string, model string, thinking bool) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	sizes := make([]int64, len(paths))
	for i, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("attachment %s: %w", p, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("attachment %s: not a regular file", p)
		}
		sizes[i] = info.Size()
	}
	if err := deepseek.ValidateAttachments(sizes); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("attachment %s: %w", p, err)
		}
		ref, err := client.UploadFile(ctx, filepath.Base(p), data, model, thinking)
		if err != nil {
			return nil, fmt.Errorf("upload %s: %w", p, err)
		}
		ids = append(ids, ref.ID)
	}
	return ids, nil
}
