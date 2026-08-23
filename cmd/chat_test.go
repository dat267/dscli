package cmd

import "testing"

func TestSplitConversation(t *testing.T) {
	tests := []struct {
		in         string
		wantSess   string
		wantParent *int64
	}{
		{"", "", nil},
		{"sess123", "sess123", nil},
		{"sess123:42", "sess123", int64ptr(42)},
		{"sess123:notanumber", "sess123", nil},
		{"sess123:", "sess123", nil},
		{":42", "", nil},
	}
	for _, tc := range tests {
		sess, parent := splitConversation(tc.in)
		if sess != tc.wantSess {
			t.Errorf("splitConversation(%q) session = %q, want %q", tc.in, sess, tc.wantSess)
		}
		if (parent == nil) != (tc.wantParent == nil) {
			t.Errorf("splitConversation(%q) parent-nil = %v, want %v", tc.in, parent == nil, tc.wantParent == nil)
			continue
		}
		if parent != nil && *parent != *tc.wantParent {
			t.Errorf("splitConversation(%q) parent = %d, want %d", tc.in, *parent, *tc.wantParent)
		}
	}
}

func TestConversationID(t *testing.T) {
	tests := []struct {
		session  string
		parent   *int64
		msgID    int64
		want     string
	}{
		{"s", nil, 0, "s"},
		{"s", nil, 7, "s:7"},
		{"s", int64ptr(3), 7, "s:3"}, // resuming keeps the id used to ask
	}
	for _, tc := range tests {
		if got := conversationID(tc.session, tc.parent, tc.msgID); got != tc.want {
			t.Errorf("conversationID(%q,%v,%d) = %q, want %q", tc.session, tc.parent, tc.msgID, got, tc.want)
		}
	}
}

func TestEffectiveModel(t *testing.T) {
	if got := effectiveModel(""); got != "default" {
		t.Errorf("effectiveModel(\"\") = %q", got)
	}
	if got := effectiveModel("expert"); got != "expert" {
		t.Errorf("effectiveModel(expert) = %q", got)
	}
}

func TestParseToggle(t *testing.T) {
	if !parseToggle("/thinking on", "/thinking ", false) {
		t.Error("on should enable")
	}
	if parseToggle("/thinking off", "/thinking ", true) {
		t.Error("off should disable")
	}
	if !parseToggle("/thinking 1", "/thinking ", false) {
		t.Error("1 should enable")
	}
	if parseToggle("/thinking wat", "/thinking ", true) != true {
		t.Error("unknown value should keep the current setting")
	}
	if parseToggle("/thinking wat", "/thinking ", false) != false {
		t.Error("unknown value should keep the current setting")
	}
}

func int64ptr(i int64) *int64 { return &i }