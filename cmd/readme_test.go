package cmd

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestREADMEHelpMatchesCLI(t *testing.T) {
	output, err := exec.Command("go", "run", "..", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("run CLI help: %v\n%s", err, output)
	}
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(readme)
	start := strings.Index(text, "```\n")
	if start < 0 {
		t.Fatal("README has no help block")
	}
	start += len("```\n")
	end := strings.Index(text[start:], "\n```")
	if end < 0 {
		t.Fatal("README help block is not closed")
	}
	want := strings.TrimSpace(text[start : start+end])
	got := strings.TrimSpace(string(output))
	if got != want {
		t.Fatalf("README help block differs from CLI output:\n--- README ---\n%s\n--- CLI ---\n%s", want, got)
	}
}
