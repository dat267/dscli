package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	first := CompletionRequest{ChatSessionID: "s1", Prompt: "hi", ModelType: "default", ThinkingEnabled: true}
	body := first.body()
	if body["chat_session_id"] != "s1" || body["prompt"] != "hi" {
		t.Errorf("first-turn body wrong: %v", body)
	}
	if body["model_type"] != "default" {
		t.Errorf("first-turn body missing model_type: %v", body)
	}
	if body["thinking_enabled"] != true {
		t.Errorf("first-turn body must carry thinking_enabled true: %v", body)
	}
	if body["action"] != nil || body["preempt"] != false {
		t.Errorf("action/preempt wrong: %v", body)
	}
	if refs, ok := body["ref_file_ids"].([]string); !ok || len(refs) != 0 {
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

	// Attachments are carried through ref_file_ids.
	withFiles := CompletionRequest{ChatSessionID: "s1", Prompt: "summarise", ModelType: "default", RefFileIDs: []string{"file-a", "file-b"}}
	refs, _ := withFiles.body()["ref_file_ids"].([]string)
	if len(refs) != 2 || refs[0] != "file-a" || refs[1] != "file-b" {
		t.Errorf("ref_file_ids = %v, want [file-a file-b]", refs)
	}

	// Resume: parent id set, model_type sent as JSON null (the web client
	// always includes the field; a thread's model is fixed at creation).
	pid := int64(7)
	resume := CompletionRequest{ChatSessionID: "s1", ParentMessageID: &pid, Prompt: "more", ModelType: ""}
	body = resume.body()
	if got := body["parent_message_id"].(*int64); *got != 7 {
		t.Errorf("resume parent_message_id wrong: %v", body["parent_message_id"])
	}
	if v, ok := body["model_type"]; !ok || v != nil {
		t.Errorf("resume body model_type = %v (present=%v), want explicit null", v, ok)
	}
	raw, err = json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"model_type":null`)) {
		t.Errorf("resume body must send model_type null, got %s", raw)
	}
}

// TestClientHeadersMatchWebClient pins the request headers to what the
// current chat.deepseek.com web client sends (captured in a HAR).
func TestClientHeadersMatchWebClient(t *testing.T) {
	tz := 25200 // UTC+7
	c := NewClient(Session{Token: "tok", DeviceID: "c54e9f4a-c397-44a0-9335-3755e5f2e856", TimezoneOffset: &tz}, 0)
	h := c.headers()
	for _, tc := range []struct{ name, want string }{
		{"x-client-version", "2.5.0"},
		{"x-client-platform", "web"},
		{"x-client-bundle-id", "com.deepseek.chat"},
		{"x-client-locale", "en_US"},
		{"x-client-timezone-offset", "25200"},
		{"x-device-id", "c54e9f4a-c397-44a0-9335-3755e5f2e856"},
	} {
		if got := h.Get(tc.name); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, got, tc.want)
		}
	}
	if _, ok := h["X-App-Version"]; ok {
		t.Error("x-app-version is not sent by the current web client")
	}
	// x-device-model is present but empty in the captured requests.
	if vals, ok := h["X-Device-Model"]; !ok || len(vals) != 1 || vals[0] != "" {
		t.Errorf("x-device-model = %v (present=%v), want one empty value", vals, ok)
	}

	// A client without an explicit device id still sends a well-formed one.
	c2 := NewClient(Session{Token: "tok"}, 0)
	id := c2.headers().Get("x-device-id")
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Errorf("generated x-device-id = %q, want a UUID", id)
	}
	if id == "00000000-0000-0000-0000-000000000000" {
		t.Error("generated x-device-id must be random")
	}
}

// TestChatHistory: GET /api/v0/chat/history_messages parses the triple
// envelope and returns messages in order, with the visible text from either
// the content field or RESPONSE fragments.
func TestChatHistory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != historyPath {
			t.Errorf("path = %s, want %s", r.URL.Path, historyPath)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if got := r.URL.Query().Get("chat_session_id"); got != "sess-1" {
			t.Errorf("chat_session_id = %q, want sess-1", got)
		}
		_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"biz_data":{
			"chat_session":{"id":"sess-1","title":"x"},
			"chat_messages":[
				{"message_id":1,"parent_id":null,"role":"USER","content":"hello"},
				{"message_id":2,"parent_id":1,"role":"ASSISTANT","content":"hi back",
				 "fragments":[{"type":"THINK","content":"hidden"},{"type":"RESPONSE","content":"hi back"}]},
				{"message_id":3,"parent_id":2,"role":"USER","content":"",
				 "fragments":[{"type":"REQUEST","content":"shown"}]}
			]
		}}}`)
	}))
	defer srv.Close()
	c := NewClient(Session{Token: "tok"}, 0, srv.URL)
	hist, err := c.ChatHistory(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ChatHistory: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("messages = %d, want 3", len(hist))
	}
	if hist[0].Role != "USER" || hist[0].Text() != "hello" {
		t.Errorf("msg0 = %+v", hist[0])
	}
	if hist[1].Role != "ASSISTANT" || hist[1].Text() != "hi back" {
		t.Errorf("msg1 = %+v", hist[1])
	}
	// Empty content falls back to the fragments' visible text.
	if hist[2].Text() != "shown" {
		t.Errorf("msg2 text = %q, want %q (fragments fallback)", hist[2].Text(), "shown")
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

func TestValidateAttachments(t *testing.T) {
	// At the limit is fine.
	ok := make([]int64, MaxAttachments)
	for i := range ok {
		ok[i] = MaxAttachmentBytes
	}
	if err := ValidateAttachments(ok); err != nil {
		t.Errorf("50 files at the size limit should pass: %v", err)
	}
	// One too many.
	if err := ValidateAttachments(make([]int64, MaxAttachments+1)); err == nil {
		t.Error("51 files should be rejected")
	}
	// One byte too large.
	if err := ValidateAttachments([]int64{MaxAttachmentBytes + 1}); err == nil {
		t.Error("a file over the per-file limit should be rejected")
	}
	if err := ValidateAttachments(nil); err != nil {
		t.Errorf("no attachments: %v", err)
	}
}

func TestUploadFileShape(t *testing.T) {
	var gotPath, gotMethod, gotCT, gotPow, gotThinking, gotModel, gotSize string
	var gotName, gotContent, gotFileCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case powChallengePath:
			_, _ = io.WriteString(w, `{"code":0,"data":{"biz_code":0,"biz_data":{"challenge":{"algorithm":"DeepSeekHashV1","challenge":"9099d8ee62c210152bb06cf47a6071e24785ab2e6c413d4cab9bbb9f849f5a58","salt":"450f343f44a1e9e6","signature":"sig","difficulty":2000,"expire_at":1752033600,"expire_after":300000,"target_path":"/api/v0/file/upload_file"}}}}`)
			var req map[string]string
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &req)
			if req["target_path"] != UploadPath {
				t.Errorf("challenge target_path = %q, want %q", req["target_path"], UploadPath)
			}
		case UploadPath:
			gotPath, gotMethod = r.URL.Path, r.Method
			gotCT = r.Header.Get("content-type")
			gotPow = r.Header.Get("x-ds-pow-response")
			gotThinking = r.Header.Get("x-thinking-enabled")
			gotModel = r.Header.Get("x-model-type")
			gotSize = r.Header.Get("x-file-size")
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("ParseMultipartForm: %v", err)
			}
			f, hdr, err := r.FormFile("file")
			if err != nil {
				t.Fatalf("FormFile: %v", err)
			}
			defer func() { _ = f.Close() }()
			data, _ := io.ReadAll(f)
			gotName, gotContent, gotFileCT = hdr.Filename, string(data), hdr.Header.Get("Content-Type")
			_, _ = io.WriteString(w, `{"code":0,"data":{"biz_code":0,"biz_data":{"id":"file-1","status":"PENDING","file_name":"notes.md","file_size":5,"model_kind":"VISION","is_image":false}}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Session{Token: "tok"}, 0, srv.URL)
	ref, err := c.UploadFile(context.Background(), "notes.md", []byte("#hi\n\n"), "expert", true)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if ref.ID != "file-1" || ref.Status != "PENDING" || ref.FileName != "notes.md" {
		t.Errorf("FileRef = %+v", ref)
	}
	if gotPath != UploadPath || gotMethod != http.MethodPost {
		t.Errorf("request = %s %s, want POST %s", gotMethod, gotPath, UploadPath)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data; boundary=") {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotName != "notes.md" || gotContent != "#hi\n\n" {
		t.Errorf("multipart file = %q %q", gotName, gotContent)
	}
	if gotFileCT != "text/markdown; charset=utf-8" && gotFileCT != "text/markdown" {
		t.Errorf("part content-type = %q, want a markdown type", gotFileCT)
	}
	if gotPow == "" {
		t.Error("x-ds-pow-response missing")
	}
	if gotThinking != "1" {
		t.Errorf("x-thinking-enabled = %q, want 1", gotThinking)
	}
	if gotModel != "expert" {
		t.Errorf("x-model-type = %q, want expert", gotModel)
	}
	if gotSize != "5" {
		t.Errorf("x-file-size = %q, want 5", gotSize)
	}
}

func TestUploadFileRejectsOversize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("nothing should be uploaded, got %s", r.URL.Path)
	}))
	defer srv.Close()
	c := NewClient(Session{Token: "tok"}, 0, srv.URL)
	if _, err := c.UploadFile(context.Background(), "big.bin", make([]byte, MaxAttachmentBytes+1), "default", false); err == nil {
		t.Error("an oversized file should be rejected before uploading")
	}
}

func TestFetchFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != FetchFilesPath || r.Method != http.MethodGet {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query()["file_ids"]; len(got) != 2 || got[0] != "file-a" || got[1] != "file-b" {
			t.Errorf("file_ids = %v", got)
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"biz_code":0,"biz_data":{"files":[{"id":"file-a","status":"SUCCESS","file_name":"a.md","file_size":10},{"id":"file-b","status":"PENDING","file_name":"b.md","file_size":20}]}}}`)
	}))
	defer srv.Close()
	c := NewClient(Session{Token: "tok"}, 0, srv.URL)
	files, err := c.FetchFiles(context.Background(), []string{"file-a", "file-b"})
	if err != nil {
		t.Fatalf("FetchFiles: %v", err)
	}
	if len(files) != 2 || files[0].ID != "file-a" || files[1].Status != "PENDING" {
		t.Errorf("files = %+v", files)
	}
}
