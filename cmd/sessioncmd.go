package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dat267/dscli/internal/deepseek"
)

// SessionCmdGroup implements `dscli session`: show the persisted default
// conversation, forget it, delete it server-side and forget it, or inspect the
// saved session texts. A bare `dscli session` prints the persisted value.
type SessionCmdGroup struct {
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
