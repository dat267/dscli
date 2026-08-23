package cmd

import (
	"strings"
	"testing"
)

func TestLoginSnippetCoversBothValues(t *testing.T) {
	snippet := loginSnippet()
	for _, want := range []string{"userToken", "ds_session_id", "visible_cookies", "cookie"} {
		if !strings.Contains(snippet, want) {
			t.Errorf("login snippet missing %q:\n%s", want, snippet)
		}
	}
}

func TestLoginCmdExplainsHttpOnlyCookie(t *testing.T) {
	app := setupTestApp(t)
	out := captureStdout(t, func() {
		if err := (&LoginCmd{}).Run(app); err != nil {
			t.Fatalf("login Run: %v", err)
		}
	})
	for _, want := range []string{"config set token", "config set cookie", "HttpOnly", "Network"} {
		if !strings.Contains(out, want) {
			t.Errorf("login output missing %q:\n%s", want, out)
		}
	}
}
