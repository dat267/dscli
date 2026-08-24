package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/dat267/dscli/internal/deepseek"
)

// SessionCmdGroup implements `dscli session`: show the persisted default
// conversation, forget it, or delete it server-side and forget it.
// A bare `dscli session` prints the persisted value.
type SessionCmdGroup struct {
	Delete SessionDeleteCmd `cmd:"" help:"Delete the persisted default session server-side and forget it"`
	Forget SessionForgetCmd `cmd:"" help:"Forget the persisted default session (the thread is kept server-side)"`
}

func (c *SessionCmdGroup) Run(app *App) error {
	if saved := loadSavedSession(app.CfgPath()); saved != "" {
		fmt.Println(saved)
	} else {
		fmt.Println("no persisted session")
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
