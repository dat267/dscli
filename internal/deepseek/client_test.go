package deepseek

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestSessionCookieHeader(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"abc123", "ds_session_id=abc123"},
		{"ds_session_id=zzz", "ds_session_id=zzz"},
		{"ds_session_id=zzz; hl=en", "ds_session_id=zzz; hl=en"},
		{"hl=en; ds_session_id=zzz", "hl=en; ds_session_id=zzz"}, // full cookie string passes through
	}
	for _, tc := range tests {
		if got := sessionCookie(tc.in); got != tc.want {
			t.Errorf("sessionCookie(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCompletionRequestBody(t *testing.T) {
	first := CompletionRequest{ChatSessionID: "s1", Prompt: "hi", ModelType: "default"}
	body := first.body()
	if body["chat_session_id"] != "s1" || body["prompt"] != "hi" {
		t.Errorf("first-turn body wrong: %v", body)
	}
	if body["model_type"] != "default" {
		t.Errorf("first-turn body missing model_type: %v", body)
	}
	if body["action"] != nil || body["preempt"] != false {
		t.Errorf("action/preempt wrong: %v", body)
	}
	if refs, ok := body["ref_file_ids"].([]any); !ok || len(refs) != 0 {
		t.Errorf("ref_file_ids wrong: %v", body["ref_file_ids"])
	}
	// parent_message_id is a typed nil pointer: it must marshal to JSON null.
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"parent_message_id":null`)) {
		t.Errorf("first-turn body must send parent_message_id null, got %s", raw)
	}

	// Resume: parent id set, model_type omitted.
	pid := int64(7)
	resume := CompletionRequest{ChatSessionID: "s1", ParentMessageID: &pid, Prompt: "more", ModelType: ""}
	body = resume.body()
	if got := body["parent_message_id"].(*int64); *got != 7 {
		t.Errorf("resume parent_message_id wrong: %v", body["parent_message_id"])
	}
	if _, ok := body["model_type"]; ok {
		t.Errorf("resume body must not carry model_type: %v", body)
	}
}