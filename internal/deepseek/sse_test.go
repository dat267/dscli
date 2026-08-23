package deepseek

import (
	"reflect"
	"strings"
	"testing"
)

// feed runs one payload through the parser and returns the emitted deltas.
func feed(t *testing.T, p *patchParser, payload string) []string {
	t.Helper()
	var out []string
	if err := p.Feed([]byte(payload), func(s string) error {
		out = append(out, s)
		return nil
	}); err != nil {
		t.Fatalf("Feed(%q): %v", payload, err)
	}
	return out
}

func TestSSESnapshotThenAppends(t *testing.T) {
	p := &patchParser{}
	var all []string

	// Snapshot frame: whole response object with the assistant message and
	// its initial content plus a message_id under v.response.
	snapshot := `{"v":{"response":{"fragments":[{"type":"response","content":"Hello"}]},"message_id":9001}}`
	all = append(all, feed(t, p, snapshot)...)
	if got, want := p.messageID, int64(9001); got == nil || *got != want {
		t.Fatalf("messageID = %v, want %d", got, want)
	}
	if p.activePath != "response/fragments/-1/content" {
		t.Errorf("activePath = %q", p.activePath)
	}

	// Path-setting append frame.
	all = append(all, feed(t, p, `{"p":"response/fragments/-1/content","o":"APPEND","v":" world"}`)...)
	// Bare appends continue the active path.
	all = append(all, feed(t, p, `{"v":"!"}`)...)
	// A frame for a non-content path (e.g. a status field) must not leak text.
	all = append(all, feed(t, p, `{"p":"response/status","o":"SET","v":"done"}`)...)
	// Bare append after path switched away must be ignored.
	all = append(all, feed(t, p, `{"v":"ignored"}`)...)

	want := []string{"Hello", " world", "!"}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("deltas = %q, want %q", all, want)
	}
}

func TestSSEMessageIDVariants(t *testing.T) {
	// message id at top level under v.id
	p := &patchParser{}
	feed(t, p, `{"v":{"response":{"fragments":[{"type":"response","content":"x"}]},"id":7}}`)
	if got := p.messageID; got == nil || *got != 7 {
		t.Fatalf("v.id variant: messageID = %v", got)
	}

	// message id inside v.response.message_id
	p = &patchParser{}
	feed(t, p, `{"v":{"response":{"message_id":8,"fragments":[{"type":"response","content":"x"}]}}}`)
	if got := p.messageID; got == nil || *got != 8 {
		t.Fatalf("v.response.message_id variant: messageID = %v", got)
	}

	// message id delivered as a path frame
	p = &patchParser{}
	feed(t, p, `{"p":"response/message_id","o":"SET","v":9}`)
	if got := p.messageID; got == nil || *got != 9 {
		t.Fatalf("path-frame variant: messageID = %v", got)
	}
}

func TestSSEFragmentTypeCaseInsensitive(t *testing.T) {
	p := &patchParser{}
	got := feed(t, p, `{"v":{"response":{"fragments":[{"type":"response","content":"lower"}]}}}`)
	if len(got) != 1 || got[0] != "lower" {
		t.Errorf("lowercase fragment type: got %q", got)
	}
}

func TestSSENonResponseFragmentsIgnored(t *testing.T) {
	p := &patchParser{}
	// Fragments of other types (thinking, search citations, ...) are skipped.
	got := feed(t, p, `{"v":{"response":{"fragments":[
		{"type":"thinking","content":"not shown"},
		{"type":"response","content":"shown"}
	]}}}`)
	want := []string{"shown"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deltas = %q, want %q", got, want)
	}
}

func TestSSESingleSnapshotFragmentEmittedOnce(t *testing.T) {
	p := &patchParser{}
	var all []string
	all = append(all, feed(t, p, `{"v":{"response":{"fragments":[
		{"type":"response","content":"first"},
		{"type":"response","content":"second"}
	]}}}`)...)
	// The second response fragment with pre-generated content is not emitted
	// (matches the site client); its content arrives as appends instead.
	want := []string{"first"}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("deltas = %q, want %q", all, want)
	}
}

func TestSSEMalformedPayloadsSkipped(t *testing.T) {
	p := &patchParser{}
	// Non-JSON, non-object frames and [DONE] must not error or emit text.
	for _, payload := range []string{"not json", "[DONE]", `{"x":1}`, `{"v":42}`, ""} {
		if got := feed(t, p, payload); len(got) != 0 {
			t.Errorf("payload %q emitted %q", payload, got)
		}
	}
}

func TestReadSSEJoinsDataLines(t *testing.T) {
	stream := "data: {\"p\":\"response/fragments/-1/content\",\"o\":\"APPEND\",\"v\":\"Hel\"}\n\n" +
		"event: patch\n" +
		"data: {\"v\":\"lo\"}\n\n" +
		"data: [DONE]\n\n" +
		": comment line\n" +
		"data: {\"v\":\"!\"}\n\n"
	var payloads []string
	if err := readSSE(strings.NewReader(stream), func(p []byte) error {
		payloads = append(payloads, string(p))
		return nil
	}); err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	want := []string{`{"p":"response/fragments/-1/content","o":"APPEND","v":"Hel"}`, `{"v":"lo"}`, `[DONE]`, `{"v":"!"}`}
	if !reflect.DeepEqual(payloads, want) {
		t.Errorf("payloads = %q, want %q", payloads, want)
	}
}

func TestSSECapturesSearchSources(t *testing.T) {
	p := &patchParser{}
	snapshot := `{"v":{"response":{"fragments":[
		{"type":"response","content":"Gold is high [citation:1][citation:2]"},
		{"type":"tool_search","references":[
			{"url":"https://ex.com/gold","title":"Gold Prices"},
			{"url":"https://ex.com/spot"}
		]}
	],"message_id":1},"message_id":1}}`
	var out []string
	if err := p.Feed([]byte(snapshot), func(s string) error { out = append(out, s); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !strings.Contains(out[0], "[citation:1]") {
		t.Errorf("reply text = %v", out)
	}
	want := []Source{{URL: "https://ex.com/gold", Title: "Gold Prices"}, {URL: "https://ex.com/spot"}}
	if !reflect.DeepEqual(p.sources, want) {
		t.Errorf("sources = %v, want %v", p.sources, want)
	}

	// A .../results patch appends more, deduplicated by URL.
	if err := p.Feed([]byte(`{"p":"response/fragments/0/results","o":"SET","v":[{"url":"https://ex.com/gold"},{"url":"https://new.com"}]}`), func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(p.sources) != 3 || p.sources[2].URL != "https://new.com" {
		t.Errorf("sources after patch = %v", p.sources)
	}
}
