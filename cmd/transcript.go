package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// transcriptDirName is the folder, next to the config file, holding one
// JSONL transcript per chat session.
const transcriptDirName = "transcripts"

// transcriptEntry is one saved message in a session transcript: a timestamp,
// the role ("user" or "assistant"), and the text as exchanged.
type transcriptEntry struct {
	Time string `json:"time"`
	Role string `json:"role"`
	Text string `json:"text"`
}

// transcriptsEnabled reports whether session texts should be saved: the run is
// not ephemeral (--no-persist) and the config (data) directory is known, so
// there is a stable place to write them.
func transcriptsEnabled(cfgPath string, noPersist, noTranscript bool) bool {
	return cfgPath != "" && !noPersist && !noTranscript
}

// transcriptPath returns the JSONL transcript path for a session: the
// transcripts folder in the app's persistent data directory (next to the
// config file), named after the bare session id. Returns "" when there is no
// config file or no session id.
func transcriptPath(cfgPath, sessionID string) string {
	if cfgPath == "" {
		return ""
	}
	sess, _ := splitConversation(sessionID)
	if sess == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfgPath), transcriptDirName, sess+".jsonl")
}

// loadTranscript returns the saved messages of a session's transcript file,
// skipping malformed lines. A nil slice is returned when there is no file.
func loadTranscript(cfgPath, sessionID string) ([]transcriptEntry, error) {
	p := transcriptPath(cfgPath, sessionID)
	if p == "" {
		return nil, nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []transcriptEntry
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e transcriptEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // keep the file readable even if a line was corrupted
		}
		out = append(out, e)
	}
	return out, nil
}

// appendTranscript appends one message to a session's transcript file,
// creating the transcripts folder and the file on first use. Failures are
// reported with the same soft warning used for session persistence — a
// transcript problem never breaks the chat itself.
func appendTranscript(cfgPath, sessionID, role, text string) {
	if text == "" {
		return
	}
	p := transcriptPath(cfgPath, sessionID)
	if p == "" {
		return
	}
	data, err := json.Marshal(transcriptEntry{
		Time: time.Now().Format(time.RFC3339),
		Role: role,
		Text: text,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save transcript: %v\n", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save transcript: %v\n", err)
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save transcript: %v\n", err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(data, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save transcript: %v\n", err)
	}
}
