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

func TestSSEErrorFrameSurfaced(t *testing.T) {
	p := &patchParser{}
	err := p.Feed([]byte(`{"type":"error","content":"Messages too frequent. Try again later.","clear_response":true,"finish_reason":"rate_limit_reached"}`), func(string) error { return nil })
	if err == nil {
		t.Fatal("error frame must be surfaced as a stream error")
	}
	if !strings.Contains(err.Error(), "rate_limit_reached") {
		t.Errorf("error should carry the finish_reason, got %q", err)
	}
}
func TestSSEStatusDetection(t *testing.T) {
	// A terminal status batch (pathless array of {p,v} patches) reporting a
	// non-completion status marks the reply as truncated.
	p := &patchParser{}
	feed(t, p, `{"v":{"response":{"fragments":[{"type":"response","content":"partial"}]}}}`)
	feed(t, p, `{"v":[{"p":"status","v":"CONTENT_FILTER"},{"p":"quasi_status","v":"CONTENT_FILTER"}]}`)
	if !p.truncated {
		t.Error("CONTENT_FILTER status should mark the reply truncated")
	}
	if p.finished {
		t.Error("CONTENT_FILTER must not mark finished")
	}

	// A BATCH quasi_status of FINISHED marks a clean end.
	p2 := &patchParser{}
	feed(t, p2, `{"v":{"response":{"fragments":[{"type":"response","content":"ok"}]}}}`)
	feed(t, p2, `{"p":"response","o":"BATCH","v":[{"p":"quasi_status","v":"FINISHED"}]}`)
	if !p2.finished {
		t.Error("FINISHED status should mark the reply finished")
	}
	if p2.truncated {
		t.Error("FINISHED must not mark truncated")
	}

	// A transient WIP followed by a clean FINISHED is NOT truncated — a
	// clean end overrides the earlier in-progress signal.
	p3 := &patchParser{}
	feed(t, p3, `{"v":{"response":{"fragments":[{"type":"response","content":"ok"}]}}}`)
	feed(t, p3, `{"v":[{"p":"status","v":"WIP"},{"p":"quasi_status","v":"WIP"}]}`)
	feed(t, p3, `{"p":"response/status","o":"SET","v":"FINISHED"}`)
	if p3.truncated {
		t.Error("WIP then FINISHED must not mark truncated")
	}
	if !p3.finished {
		t.Error("FINISHED should mark the reply finished")
	}
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
	// A frame for a non-content path (e.g. a status field) must not leak the
	// status VALUE itself ("done" is dropped), but a subsequent pathless chunk
	// is still visible text — a status frame interleaved between answer chunks
	// must not swallow the next chunk.
	all = append(all, feed(t, p, `{"p":"response/status","o":"SET","v":"done"}`)...)
	all = append(all, feed(t, p, `{"v":"ignored"}`)...)

	want := []string{"Hello", " world", "!", "ignored"}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("deltas = %q, want %q", all, want)
	}
}

// TestSSEStatusFrameBetweenChunks: the exact tool-call regression — the
// reply's `{"tool":...}` was reconstructed as `{"":...}` because a status
// frame landed between the `{"` and `tool` pathless chunks, and the old gate
// dropped the chunk after a non-content path. It must now be kept.
func TestSSEStatusFrameBetweenChunks(t *testing.T) {
	p := &patchParser{}
	var all []string
	all = append(all, feed(t, p, `{"v":{"response":{"fragments":[{"type":"response","content":""}]}}}`)...)
	all = append(all, feed(t, p, `{"v":"{\"tool"}`)...)
	all = append(all, feed(t, p, `{"p":"response/status","o":"SET","v":"WIP"}`)...)
	all = append(all, feed(t, p, `{"v":"\":\"fetch_url\",\"url\":\"https://httpbin.org/get\"}"}`)...)
	got := strings.Join(all, "")
	if want := `{"tool":"fetch_url","url":"https://httpbin.org/get"}`; got != want {
		t.Errorf("reconstructed reply = %q, want %q (deltas=%q)", got, want, all)
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

// TestSSEFirstChunkBeforeAnyPath: the reply's first characters arrive as
// pathless {"v":...} chunks before any "p" frame or snapshot text; they must
// not be dropped (this was the missing-initial-characters bug).
func TestSSEFirstChunkBeforeAnyPath(t *testing.T) {
	p := &patchParser{}
	var all []string
	for _, payload := range []string{
		`{"v":"D"}`,
		`{"v":"ựa"}`,
		`{"p":"response/fragments/-1/content","o":"APPEND","v":" trên"}`,
	} {
		all = append(all, feed(t, p, payload)...)
	}
	want := []string{"D", "ựa", " trên"}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("deltas = %q, want %q", all, want)
	}
}

// TestSSEEmptySnapshotThenPathless: the snapshot's response fragment has no
// pre-generated text; the first real chunk still arrives pathless and must
// be kept.
func TestSSEEmptySnapshotThenPathless(t *testing.T) {
	p := &patchParser{}
	all := feed(t, p, `{"v":{"response":{"fragments":[{"type":"response","content":""}],"message_id":1},"message_id":1}}`)
	all = append(all, feed(t, p, `{"v":"Hi"}`)...)
	if !reflect.DeepEqual(all, []string{"Hi"}) {
		t.Errorf("deltas = %q, want [Hi]", all)
	}
}

// TestSSESetAsInitial: a SET (not APPEND) can carry the initial text when
// nothing was emitted yet.
func TestSSESetAsInitial(t *testing.T) {
	p := &patchParser{}
	// A content SET always targets an existing (registered) fragment.
	all := feed(t, p, `{"v":{"response":{"fragments":[{"type":"response","content":""}],"message_id":1},"message_id":1}}`)
	all = append(all, feed(t, p, `{"p":"response/fragments/-1/content","o":"SET","v":"Hi"}`)...)
	all = append(all, feed(t, p, `{"p":"response/fragments/-1/content","o":"APPEND","v":"!"}`)...)
	want := []string{"Hi", "!"}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("deltas = %q, want %q", all, want)
	}
}

// TestSSESetAfterEmitSkipped: a later SET replaces the whole content slot
// and must not duplicate text already streamed.
func TestSSESetAfterEmitSkipped(t *testing.T) {
	p := &patchParser{}
	all := feed(t, p, `{"v":{"response":{"fragments":[{"type":"response","content":"Hi"}],"message_id":1},"message_id":1}}`)
	all = append(all, feed(t, p, `{"p":"response/fragments/-1/content","o":"SET","v":"Hi there"}`)...)
	all = append(all, feed(t, p, `{"p":"response/fragments/-1/content","o":"APPEND","v":"!"}`)...)
	want := []string{"Hi", "!"}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("deltas = %q, want %q", all, want)
	}
}

// TestSSEObjectFormV: pathless chunks may carry {"text":...}/{"content":...}.
func TestSSEObjectFormV(t *testing.T) {
	p := &patchParser{}
	var all []string
	all = append(all, feed(t, p, `{"v":{"text":"A"}}`)...)
	all = append(all, feed(t, p, `{"v":{"content":"B"}}`)...)
	if !reflect.DeepEqual(all, []string{"A", "B"}) {
		t.Errorf("deltas = %q", all)
	}
}

// TestSSEBatchContentPatch: BATCH frames nest {p, o, v} patches that can
// carry content appends.
func TestSSEBatchContentPatch(t *testing.T) {
	p := &patchParser{}
	all := feed(t, p, `{"p":"response","o":"BATCH","v":[{"p":"fragments/-1/content","o":"APPEND","v":"x"}]}`)
	if !reflect.DeepEqual(all, []string{"x"}) {
		t.Errorf("deltas = %q", all)
	}
}

// TestSSEMultipleFramesPerEvent: upstream batches several patch frames into
// one SSE event (joined data: lines). Every frame must be applied — the
// second frame riding with the snapshot was previously dropped, eating the
// reply's opening token ("As of..." -> " of...").
func TestSSEMultipleFramesPerEvent(t *testing.T) {
	p := &patchParser{}
	payload := `{"v":{"response":{"fragments":[{"type":"response","content":""}],"message_id":1},"message_id":1}}` + "\n" +
		`{"p":"response/fragments/-1/content","o":"APPEND","v":"As "}` + "\n" +
		`{"p":"response/fragments/-1/content","o":"APPEND","v":"of Aug"}` + "\n" +
		`{"v":"ust"}` + "\n" +
		`{"p":"response/fragments/-1/content","o":"SET","v":"As of August"}` // full-slot SET after emission: skipped
	all := feed(t, p, payload)
	want := []string{"As ", "of Aug", "ust"}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("deltas = %q, want %q", all, want)
	}
}

// TestSSEOpLessContentFrameContinues reproduces the live tool-call stream:
// the RESPONSE fragment is container-appended with its first token, and the
// next chunk arrives as an OP-LESS {"p":"response/fragments/-1/content","v":...}
// frame (no "o" field). That frame continues the content — it must not be
// treated as a full-slot SET, which is skipped after emission and corrupts
// {"tool":...} into {"":...}.
func TestSSEOpLessContentFrameContinues(t *testing.T) {
	p := &patchParser{}
	var all []string
	all = append(all, feed(t, p, `{"v":{"response":{"fragments":[{"type":"THINK","content":"We"}]}}}`)...)
	all = append(all, feed(t, p, `{"p":"response/fragments","o":"APPEND","v":[{"type":"RESPONSE","content":"{\""}]}`)...)
	all = append(all, feed(t, p, `{"p":"response/fragments/-1/content","v":"tool"}`)...) // op-less: continuation
	all = append(all, feed(t, p, `{"v":"\":\"fetch_url\",\"url\":\"https://httpbin.org/get\"}"}`)...)
	got := strings.Join(all, "")
	if want := `{"tool":"fetch_url","url":"https://httpbin.org/get"}`; got != want {
		t.Errorf("reconstructed reply = %q, want %q (deltas=%q)", got, want, all)
	}
}

// TestSSEOpLessFirstContentFrame: the first content update may carry no "o"
// field at all; it must still be treated as a continuation.
func TestSSEOpLessFirstContentFrame(t *testing.T) {
	p := &patchParser{}
	all := feed(t, p, `{"v":{"response":{"fragments":[{"type":"response","content":""}],"message_id":1},"message_id":1}}`)
	all = append(all, feed(t, p, `{"p":"response/fragments/-1/content","v":"As "}`)...)
	all = append(all, feed(t, p, `{"p":"response/fragments/-1/content","o":"APPEND","v":"of"}`)...)
	if !reflect.DeepEqual(all, []string{"As ", "of"}) {
		t.Errorf("deltas = %q", all)
	}
}

// TestSSEContainerAppendResponseFragment reproduces the real stream shape:
// the snapshot holds only THINK/TOOL_SEARCH fragments, and the answer's FIRST
// token arrives as a RESPONSE fragment appended to the response/fragments
// container — it must be emitted, then -1/content appends continue it.
func TestSSEContainerAppendResponseFragment(t *testing.T) {
	p := &patchParser{}
	var all []string
	all = append(all, feed(t, p, `{"v":{"response":{"fragments":[{"id":2,"type":"THINK","content":"thinking..."}]}}}`)...)
	all = append(all, feed(t, p, `{"p":"response/fragments","o":"APPEND","v":[{"id":3,"type":"RESPONSE","content":"Based"}]}`)...)
	all = append(all, feed(t, p, `{"p":"response/fragments/-1/content","o":"APPEND","v":" on the"}`)...)
	all = append(all, feed(t, p, `{"v":" search"}`)...)
	all = append(all, feed(t, p, `{"v":" results"}`)...)
	want := []string{"Based", " on the", " search", " results"}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("deltas = %q, want %q", all, want)
	}
	// A TOOL_SEARCH fragment appended the same way also yields sources.
	p2 := &patchParser{}
	feed(t, p2, `{"p":"response/fragments","o":"APPEND","v":[{"id":4,"type":"tool_search","references":[{"url":"https://ex.com/g","title":"G"}]}]}`)
	if len(p2.sources) != 1 || p2.sources[0].URL != "https://ex.com/g" {
		t.Errorf("container-appended tool_search sources = %v", p2.sources)
	}
}

// TestSSEThinkingNeverLeaks: with thinking enabled, content belonging to
// THINK fragments (snapshot content, -1/content appends, pathless chunks)
// must never render as answer text; only the RESPONSE fragment content does.
// This mirrors the --think mode: thinking text was previously shown as the
// reply ("..., the user has asked me...").
func TestSSEThinkingNeverLeaks(t *testing.T) {
	p := &patchParser{}
	var all []string
	// Snapshot: THINK fragment with pre-generated thinking.
	all = append(all, feed(t, p, `{"v":{"response":{"fragments":[{"type":"THINK","content":"OK"}]}}}`)...)
	// Thinking continues via appends and pathless chunks (target -1 = THINK).
	all = append(all, feed(t, p, `{"p":"response/fragments/-1/content","o":"APPEND","v":", the user"}`)...)
	all = append(all, feed(t, p, `{"v":" asks"}`)...)
	// The answer: RESPONSE fragment appended with its first token.
	all = append(all, feed(t, p, `{"p":"response/fragments","o":"APPEND","v":[{"type":"RESPONSE","content":"**Yes"}]}`)...)
	all = append(all, feed(t, p, `{"p":"response/fragments/-1/content","o":"APPEND","v":", I can"}`)...)
	all = append(all, feed(t, p, `{"v":" read!"}`)...)
	want := []string{"**Yes", ", I can", " read!"}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("deltas = %q, want %q", all, want)
	}
}

// TestSSEInterleavedThinkingDoesNotEatAnswerText reproduces the tool-call
// regression: the answer's chunks arrived as `{"` (container-appended
// RESPONSE), then a THINK fragment landed in the middle, and the rest of the
// JSON came through -1/content and pathless chunks. The THINK fragment must
// not hijack the routing — otherwise {"tool":"fetch_url",...} renders as
// {"":"fetch_url",...} and the tool call is never executed.
func TestSSEInterleavedThinkingDoesNotEatAnswerText(t *testing.T) {
	p := &patchParser{}
	var all []string
	all = append(all, feed(t, p, `{"v":{"response":{"fragments":[{"type":"THINK","content":"thinking"}]}}}`)...)
	all = append(all, feed(t, p, `{"p":"response/fragments","o":"APPEND","v":[{"type":"RESPONSE","content":"{"}]}`)...)
	all = append(all, feed(t, p, `{"p":"response/fragments","o":"APPEND","v":[{"type":"THINK","content":"(more reasoning)"}]}`)...)
	all = append(all, feed(t, p, `{"p":"response/fragments/-1/content","o":"APPEND","v":"\"tool"}`)...)
	all = append(all, feed(t, p, `{"v":"\":\"fetch_url\",\"url\":\"https://httpbin.org/get\"}"}`)...)
	got := strings.Join(all, "")
	if want := `{"tool":"fetch_url","url":"https://httpbin.org/get"}`; got != want {
		t.Errorf("reconstructed reply = %q, want %q (deltas=%q)", got, want, all)
	}
}

// TestSSEPerFragmentSetAfterThinking: the answer's first token can arrive as
// a SET on the RESPONSE fragment's content path AFTER thinking already
// emitted — the per-fragment guard must still accept it (the old global
// emittedAny flag dropped "**Yes" here).
func TestSSEPerFragmentSetAfterThinking(t *testing.T) {
	p := &patchParser{}
	var all []string
	all = append(all, feed(t, p, `{"v":{"response":{"fragments":[{"type":"THINK","content":"thinking..."}]}}}`)...)
	all = append(all, feed(t, p, `{"p":"response/fragments/-1/content","o":"SET","v":"more thinking"}`)...) // THINK: skipped
	all = append(all, feed(t, p, `{"p":"response/fragments","o":"APPEND","v":[{"type":"RESPONSE","content":""}]}`)...)
	all = append(all, feed(t, p, `{"p":"response/fragments/-1/content","o":"SET","v":"**Yes"}`)...)              // RESPONSE initial: kept
	all = append(all, feed(t, p, `{"p":"response/fragments/-1/content","o":"SET","v":"**Yes, I can read!"}`)...) // full replace: skipped
	all = append(all, feed(t, p, `{"p":"response/fragments/-1/content","o":"APPEND","v":"!"}`)...)
	want := []string{"**Yes", "!"}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("deltas = %q, want %q", all, want)
	}
}

// TestSSEIndexedContentRespectsFragmentType: content appends targeting an
// explicit index follow that fragment's type (index 0 = THINK is skipped,
// index 1 = RESPONSE is emitted).
func TestSSEIndexedContentRespectsFragmentType(t *testing.T) {
	p := &patchParser{}
	var all []string
	all = append(all, feed(t, p, `{"v":{"response":{"fragments":[
		{"type":"THINK","content":"t"},
		{"type":"RESPONSE","content":"A"}
	]}}}`)...)
	all = append(all, feed(t, p, `{"p":"response/fragments/0/content","o":"APPEND","v":"hink"}`)...)  // THINK: skipped
	all = append(all, feed(t, p, `{"p":"response/fragments/1/content","o":"APPEND","v":"nswer"}`)...) // RESPONSE: emitted
	want := []string{"A", "nswer"}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("deltas = %q, want %q", all, want)
	}
}
