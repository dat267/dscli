package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/dat267/dscli/internal/deepseek"
)

// sessionKey is the config key holding the persisted default chat session id,
// so consecutive invocations continue the same thread unless --no-persist is
// given.
const sessionKey = "session"

// loadSavedSession returns the persisted default session id, or "" when none
// is saved (or the config cannot be read).
func loadSavedSession(cfgPath string) string {
	if cfgPath == "" {
		return ""
	}
	m, err := loadConfigMap(cfgPath)
	if err != nil {
		return ""
	}
	if s, ok := m[sessionKey].(string); ok {
		return s
	}
	return ""
}

// saveSession persists the default session id into the config file, preserving
// every other key. A no-op when cfgPath is empty (tests).
func saveSession(cfgPath, sessionID string) error {
	if cfgPath == "" {
		return nil
	}
	m, err := loadConfigMap(cfgPath)
	if err != nil {
		return err
	}
	m[sessionKey] = sessionID
	return saveConfigMap(cfgPath, m)
}

// clearSession removes the persisted default session key from the config file.
func clearSession(cfgPath string) error {
	if cfgPath == "" {
		return nil
	}
	m, err := loadConfigMap(cfgPath)
	if err != nil {
		return err
	}
	if _, ok := m[sessionKey]; !ok {
		return nil
	}
	delete(m, sessionKey)
	return saveConfigMap(cfgPath, m)
}

// resolveDefaultSession returns the session id to use for one run.
//
// With noPersist a fresh session is created and the returned cleanup deletes
// it when the run ends (the pre-persistence behaviour). Otherwise the session
// saved in the config is reused; when none is saved, a fresh one is created
// and saved as the new default. trusted reports whether the id came from the
// config, which makes it eligible for stale-session recovery.
func resolveDefaultSession(ctx context.Context, client *deepseek.Client, cfgPath string, noPersist bool) (sessionID string, trusted bool, cleanup func(), err error) {
	if noPersist {
		sid, err := client.CreateChatSession(ctx)
		if err != nil {
			return "", false, nil, err
		}
		return sid, false, func() {
			if err := client.DeleteSessions(context.Background(), []string{sid}); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to delete session: %v\n", err)
			}
		}, nil
	}
	if saved := loadSavedSession(cfgPath); saved != "" {
		return saved, true, nil, nil
	}
	sid, err := client.CreateChatSession(ctx)
	if err != nil {
		return "", false, nil, err
	}
	if err := saveSession(cfgPath, sid); err != nil {
		return "", false, nil, err
	}
	return sid, false, nil, nil
}

// persistConversation saves the advanced conversation position (session:message)
// after a successful turn, unless the run is ephemeral (--no-persist). Without
// it the saved value stays the bare session id and every later run resumes the
// thread from its root instead of the last message. A no-op when cfgPath is
// empty (tests) or convID is empty.
func persistConversation(cfgPath string, noPersist bool, convID string) {
	if noPersist || cfgPath == "" || convID == "" {
		return
	}
	if err := saveSession(cfgPath, convID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save conversation position: %v\n", err)
	}
}

// advanceConversation returns the conversation id to persist after a turn in
// session that produced msgID. session may already be a "session:message" id;
// only the bare session part is kept and the message tail is replaced.
func advanceConversation(session string, msgID int64) string {
	sess, _ := splitConversation(session)
	return conversationID(sess, nil, msgID)
}

// recoverStaleSession runs run once. If it fails and the session was a trusted
// persisted one (loaded from the config), a fresh session is created, saved as
// the new default, and run is re-executed once with it — covering the case
// where the saved session no longer exists server-side. The returned id is the
// one the successful run used (the original on error).
func recoverStaleSession(ctx context.Context, client *deepseek.Client, cfgPath string, sessionID string, trusted bool, run func(string) error) (string, error) {
	firstErr := run(sessionID)
	if firstErr == nil || !trusted {
		return sessionID, firstErr
	}
	sid, cerr := client.CreateChatSession(ctx)
	if cerr != nil {
		return sessionID, firstErr // report the original failure
	}
	if serr := saveSession(cfgPath, sid); serr == nil {
		if rerr := run(sid); rerr == nil {
			return sid, nil
		}
	}
	return sessionID, firstErr
}
