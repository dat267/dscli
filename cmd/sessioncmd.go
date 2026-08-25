package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dat267/dscli/internal/deepseek"
)

// SessionCmdGroup implements `dscli session`: show the persisted default
// conversation, list and select sessions with saved texts, forget/delete the
// default, or inspect the saved texts. A bare `dscli session` prints the
// persisted value.
type SessionCmdGroup struct {
	List       SessionListCmd       `cmd:"" help:"List sessions with saved texts"`
	Select     SessionSelectCmd     `cmd:"" help:"Select a session to resume as the default"`
	Transcript SessionTranscriptCmd `cmd:"" help:"Print or delete the saved session texts (transcript) for a session"`
	Delete     SessionDeleteCmd     `cmd:"" help:"Delete the persisted default session server-side and forget it"`
	Forget     SessionForgetCmd     `cmd:"" help:"Forget the persisted default session (the thread is kept server-side)"`
}

func (c *SessionCmdGroup) Run(app *App) error {
	if saved := loadSavedSession(app.CfgPath()); saved != "" {
		fmt.Println(saved)
	} else {
		fmt.Println("no persisted session")
	}
	return nil
}

// SessionListCmd implements `dscli session list`: list the sessions the CLI
// knows about locally — those with a saved transcript — most recently used
// first, marking the persisted default.
type SessionListCmd struct{}

func (c *SessionListCmd) Run(app *App) error {
	cfgPath := app.CfgPath()
	dir := filepath.Join(filepath.Dir(cfgPath), transcriptDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("no local sessions (nothing saved yet)")
			return nil
		}
		return fmt.Errorf("read transcripts: %w", err)
	}
	type row struct {
		id    string
		msgs  int
		last  string
		mtime int64
	}
	var rows []row
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(dir, e.Name())
		msgs, last := transcriptSummary(path, info.ModTime().Unix())
		rows = append(rows, row{
			id:    strings.TrimSuffix(e.Name(), ".jsonl"),
			msgs:  msgs,
			last:  last,
			mtime: info.ModTime().Unix(),
		})
	}
	if len(rows) == 0 {
		fmt.Println("no local sessions (nothing saved yet)")
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].mtime > rows[j].mtime })
	saved, _ := splitConversation(loadSavedSession(cfgPath))
	for _, r := range rows {
		marker := ""
		if r.id == saved {
			marker = "  (default)"
		}
		fmt.Printf("%-24s %d msgs  last %s%s\n", r.id, r.msgs, r.last, marker)
	}
	return nil
}

// transcriptSummary counts a transcript file's messages and returns the
// timestamp of the last one, falling back to the file's mtime when the file
// is empty or unreadable.
func transcriptSummary(path string, fallbackMtime int64) (msgs int, last string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, timeString(fallbackMtime)
	}
	last = timeString(fallbackMtime)
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		msgs++
		var e transcriptEntry
		if json.Unmarshal([]byte(line), &e) == nil && e.Time != "" {
			last = e.Time
		}
	}
	return msgs, last
}

// timeString renders a unix timestamp in a compact local form.
func timeString(ts int64) string {
	return time.Unix(ts, 0).Format("2006-01-02 15:04")
}

// transcriptCount reports how many messages a session's transcript holds, and
// whether the transcript exists at all.
func transcriptCount(cfgPath, bare string) (msgs int, ok bool) {
	entries, err := loadTranscript(cfgPath, bare)
	if err != nil || len(entries) == 0 {
		return 0, false
	}
	return len(entries), true
}

// SessionSelectCmd implements `dscli session select <session>`: make the
// given session the persisted default to resume next time. The thread resumes
// from its root (the bare session id; a "session:message" tail is stripped).
type SessionSelectCmd struct {
	Session string `arg:"" help:"Session id to resume as the default"`
}

func (c *SessionSelectCmd) Run(app *App) error {
	path := app.CfgPath()
	bare, _ := splitConversation(strings.TrimSpace(c.Session))
	if bare == "" {
		return errors.New("give a session id (run 'dscli session list' to see the saved ones)")
	}
	if msgs, ok := transcriptCount(path, bare); ok {
		fmt.Printf("selected session %s (%d saved messages)\n", bare, msgs)
	} else {
		fmt.Printf("selected session %s (no local transcript yet — first chat will create it)\n", bare)
	}
	if err := saveSession(path, bare); err != nil {
		return err
	}
	return nil
}

// resolveTranscriptSession returns the config path and bare session id for an
// explicit session, or the persisted default when none is given. ok is false
// when there is nothing to address.
func resolveTranscriptSession(app *App, given string) (cfgPath, bare string, ok bool) {
	cfgPath = app.CfgPath()
	sess := given
	if sess == "" {
		sess = loadSavedSession(cfgPath)
	}
	bare, _ = splitConversation(sess)
	if bare == "" {
		return "", "", false
	}
	return cfgPath, bare, true
}

// SessionTranscriptCmd implements `dscli session transcript [session]`: print
// the session texts saved by the chat (the JSONL transcript next to the
// config file) for the persisted default session, or for an explicit session
// id. With --delete the transcript file is removed instead (plus the
// transcripts folder when it becomes empty); the server-side thread, if any,
// is untouched.
type SessionTranscriptCmd struct {
	Session string `arg:"" optional:"" help:"Session id (defaults to the persisted default session)"`
	Delete  bool   `help:"Delete the transcript instead of printing it"`
}

func (c *SessionTranscriptCmd) Run(app *App) error {
	cfgPath, bare, ok := resolveTranscriptSession(app, c.Session)
	if !ok {
		if c.Delete {
			fmt.Println("no session to delete")
		} else {
			fmt.Println("no session to show")
		}
		return nil
	}
	p := transcriptPath(cfgPath, bare)
	if c.Delete {
		if err := os.Remove(p); err != nil {
			if os.IsNotExist(err) {
				fmt.Printf("no saved texts for session %s (nothing to delete)\n", bare)
				return nil
			}
			return fmt.Errorf("delete transcript: %w", err)
		}
		// Drop the transcripts folder too when it is now empty.
		_ = os.Remove(filepath.Dir(p))
		fmt.Printf("deleted transcript for session %s\n", bare)
		return nil
	}
	entries, err := loadTranscript(cfgPath, bare)
	if err != nil {
		return fmt.Errorf("read transcript: %w", err)
	}
	if len(entries) == 0 {
		fmt.Printf("no saved texts for session %s (%s does not exist)\n", bare, p)
		return nil
	}
	fmt.Printf("session %s · %s\n", bare, p)
	for i, e := range entries {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("%s  %s\n%s\n", e.Time, e.Role, e.Text)
	}
	return nil
}

// SessionForgetCmd implements `dscli session forget`: remove the persisted
// default conversation from the config. The thread stays on the server.
type SessionForgetCmd struct{}

func (c *SessionForgetCmd) Run(app *App) error {
	path := app.CfgPath()
	saved := loadSavedSession(path)
	if saved == "" {
		fmt.Println("no persisted session to forget")
		return nil
	}
	if err := clearSession(path); err != nil {
		return err
	}
	fmt.Printf("forgot session %s (thread kept server-side)\n", saved)
	return nil
}

// SessionDeleteCmd implements `dscli session delete`: delete the persisted
// default conversation server-side, then forget it. Deleting needs
// credentials; when none are available the thread is only forgotten.
type SessionDeleteCmd struct {
	Token     string `env:"DS_TOKEN" help:"DeepSeek user token (localStorage.userToken). Alternatively: config set token"`
	Cookie    string `env:"DS_COOKIE" help:"DeepSeek ds_session_id cookie value. Alternatively: config set cookie"`
	UserAgent string `env:"DS_USER_AGENT" help:"Browser user-agent; some deployments reject non-browser UAs"`

	// clientBase overrides the API base URL for tests.
	clientBase string
}

func (c *SessionDeleteCmd) Run(app *App) error {
	path := app.CfgPath()
	saved := loadSavedSession(path)
	if saved == "" {
		fmt.Println("no persisted session to delete")
		return nil
	}
	sess, _ := splitConversation(saved)
	if c.Token == "" {
		fmt.Fprintln(os.Stderr, "warning: no credentials configured; thread not deleted server-side")
	} else {
		client := deepseek.NewClient(deepseek.Session{
			Token:     c.Token,
			Cookie:    c.Cookie,
			UserAgent: c.UserAgent,
		}, 0, c.clientBase)
		if err := client.DeleteSessions(context.Background(), []string{sess}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: server-side delete failed: %v\n", err)
		} else {
			fmt.Printf("deleted session %s server-side\n", sess)
		}
	}
	if err := clearSession(path); err != nil {
		return err
	}
	fmt.Printf("forgot session %s\n", saved)
	return nil
}
