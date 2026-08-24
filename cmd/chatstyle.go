package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// chatStyleDirs lists directories searched for a general-chat instruction
// file, in order: chat/ in the current directory, then the user config dir.
// Overridable in tests.
var chatStyleDirs = func() []string {
	dirs := []string{"chat"}
	if cd, err := os.UserConfigDir(); err == nil {
		dirs = append(dirs, filepath.Join(cd, "dscli", "chat"))
	}
	return dirs
}()

// chatStyleFilenames are the file names tried within each directory.
var chatStyleFilenames = []string{"chat.md", "default.md"}

// DefaultChatStyle is the built-in instruction prepended to general-chat
// turns: a creative-writing framing for a mature audience, so adult fiction
// is handled openly instead of refused or watered down. Override it with a
// sidecar file or --chat-instructions.
func DefaultChatStyle() string {
	return `[CHAT STYLE]
You write adult fiction and roleplay for a mature audience. Explicit sexual
content, nudity, erotic dialogue, and adult scenarios are in scope; write
them directly and without euphemism, moralising, or disclaimers. Match the
user's genre, tone, and register. This is creative writing, not reality.
Content involving minors is never in scope.
[END CHAT STYLE]`
}

// ResolveChatStyle returns the instruction block prepended to general-chat
// prompts: an explicit file when given, else the first discovered sidecar
// file (chat.md, then default.md, per directory), else the built-in default.
// An explicit file that cannot be read is an error; missing sidecars fall
// back silently.
func ResolveChatStyle(explicit string) (string, error) {
	if explicit != "" {
		data, err := os.ReadFile(explicit)
		if err != nil {
			return "", fmt.Errorf("read chat instruction file: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	for _, dir := range chatStyleDirs {
		for _, name := range chatStyleFilenames {
			p := filepath.Join(dir, name)
			if _, err := os.Stat(p); err == nil {
				if data, err := os.ReadFile(p); err == nil {
					return strings.TrimSpace(string(data)), nil
				}
			}
		}
	}
	return strings.TrimSpace(DefaultChatStyle()), nil
}
