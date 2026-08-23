package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
func TestDeleteSessions(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"success", 200, `{"code":0,"data":{}}`, false},
		{"api error code", 200, `{"code":40002,"msg":"Missing Token"}`, true},
		{"http error", 403, `<html>forbidden</html>`, true},
		{"missing envelope", 200, `{"weird":true}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotMethod string
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotMethod = r.URL.Path, r.Method
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &gotBody)
				if r.Header.Get("authorization") != "Bearer tok" {
					t.Errorf("authorization header = %q", r.Header.Get("authorization"))
				}
				if r.Header.Get("cookie") != "ds_session_id=ck" {
					t.Errorf("cookie header = %q", r.Header.Get("cookie"))
				}
				if r.Header.Get("x-client-platform") != "web" {
					t.Errorf("x-client-platform = %q", r.Header.Get("x-client-platform"))
				}
				if r.Header.Get("referer") != BaseURL+"/" {
					t.Errorf("referer = %q", r.Header.Get("referer"))
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			client := NewClient(Session{Token: "tok", Cookie: "ck"}, 0)
			client.base = srv.URL // use the fake server
			err := client.DeleteSessions(context.Background(), []string{"s1", "s2"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("DeleteSessions err = %v, wantErr %v", err, tc.wantErr)
			}
			if gotPath != sessionDeletePath || gotMethod != http.MethodPost {
				t.Errorf("request = %s %s, want POST %s", gotMethod, gotPath, sessionDeletePath)
			}
			ids, _ := gotBody["chat_session_ids"].([]any)
			if len(ids) != 2 || ids[0] != "s1" || ids[1] != "s2" {
				t.Errorf("body chat_session_ids = %v", gotBody)
			}
		})
	}
}

func TestDeleteSessionsNoopForEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be sent for an empty id list")
	}))
	defer srv.Close()
	client := NewClient(Session{Token: "tok"}, 0)
	client.base = srv.URL
	if err := client.DeleteSessions(context.Background(), nil); err != nil {
		t.Fatalf("DeleteSessions(nil) = %v", err)
	}
}

func TestHTTPStatusErrorPrefersEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":40001,"msg":"Unauthorized"}`)
	}))
	defer srv.Close()
	client := NewClient(Session{Token: "tok"}, 0)
	client.base = srv.URL
	_, err := client.CreateChatSession(context.Background())
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("Unauthorized")) {
		t.Errorf("CreateChatSession err = %v, want envelope msg", err)
	}
}
