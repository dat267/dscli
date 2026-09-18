import { strict as assert } from "node:assert";
import { test } from "node:test";
import { PatchParser, type Source } from "../src/core/deepseek/sse.js";

/** feed runs one payload through the parser and returns the emitted deltas. */
function feed(p: PatchParser, payload: string): string[] {
	const out: string[] = [];
	const err = p.feed(payload, (s) => out.push(s));
	assert.equal(err, undefined, `Feed(${payload})`);
	return out;
}

test("error frame is surfaced as a stream error", () => {
	const p = new PatchParser();
	const err = p.feed(`{"type":"error","content":"Messages too frequent. Try again later.","clear_response":true,"finish_reason":"rate_limit_reached"}`, () => {});
	assert.ok(err, "error frame must be surfaced as a stream error");
	assert.ok(err!.includes("rate_limit_reached"), `error should carry the finish_reason, got ${err}`);
});

test("status detection", () => {
	// A terminal status batch (pathless array of {p,v} patches) reporting a
	// non-completion status marks the reply as truncated.
	const p = new PatchParser();
	feed(p, `{"v":{"response":{"fragments":[{"type":"response","content":"partial"}]}}}`);
	feed(p, `{"v":[{"p":"status","v":"CONTENT_FILTER"},{"p":"quasi_status","v":"CONTENT_FILTER"}]}`);
	assert.ok(p.truncated, "CONTENT_FILTER status should mark the reply truncated");
	assert.ok(p.filtered, "CONTENT_FILTER status should mark the reply filtered");
	assert.ok(!p.finished, "CONTENT_FILTER must not mark finished");

	// A BATCH quasi_status of FINISHED marks a clean end.
	const p2 = new PatchParser();
	feed(p2, `{"v":{"response":{"fragments":[{"type":"response","content":"ok"}]}}}`);
	feed(p2, `{"p":"response","o":"BATCH","v":[{"p":"quasi_status","v":"FINISHED"}]}`);
	assert.ok(p2.finished, "FINISHED status should mark the reply finished");
	assert.ok(!p2.truncated, "FINISHED must not mark truncated");
	assert.ok(!p2.filtered, "FINISHED must not mark filtered");

	// INCOMPLETE means a cut-off reply, but NOT a content-filter rejection.
	const p4 = new PatchParser();
	feed(p4, `{"v":{"response":{"fragments":[{"type":"response","content":"partial"}]}}}`);
	feed(p4, `{"p":"response/status","o":"SET","v":"INCOMPLETE"}`);
	assert.ok(p4.truncated, "INCOMPLETE should mark the reply truncated");
	assert.ok(!p4.filtered, "INCOMPLETE must not mark filtered");

	// A transient WIP followed by a clean FINISHED is NOT truncated — a
	// clean end overrides the earlier in-progress signal.
	const p3 = new PatchParser();
	feed(p3, `{"v":{"response":{"fragments":[{"type":"response","content":"ok"}]}}}`);
	feed(p3, `{"v":[{"p":"status","v":"WIP"},{"p":"quasi_status","v":"WIP"}]}`);
	feed(p3, `{"p":"response/status","o":"SET","v":"FINISHED"}`);
	assert.ok(!p3.truncated, "WIP then FINISHED must not mark truncated");
	assert.ok(p3.finished, "FINISHED should mark the reply finished");
});

test("snapshot then appends", () => {
	const p = new PatchParser();
	const all: string[] = [];

	const snapshot = `{"v":{"response":{"fragments":[{"type":"response","content":"Hello"}]},"message_id":9001}}`;
	all.push(...feed(p, snapshot));
	assert.equal(p.messageId, 9001);
	assert.equal(p.activePath, "response/fragments/-1/content");

	all.push(...feed(p, `{"p":"response/fragments/-1/content","o":"APPEND","v":" world"}`));
	all.push(...feed(p, `{"v":"!"}`));
	// A frame for a non-content path (e.g. a status field) must not leak the
	// status VALUE itself ("done" is dropped), but a subsequent pathless chunk
	// is still visible text — a status frame interleaved between answer chunks
	// must not swallow the next chunk.
	all.push(...feed(p, `{"p":"response/status","o":"SET","v":"done"}`));
	all.push(...feed(p, `{"v":"ignored"}`));

	assert.deepEqual(all, ["Hello", " world", "!", "ignored"]);
});

test("status frame between chunks (tool-call regression)", () => {
	const p = new PatchParser();
	const all: string[] = [];
	all.push(...feed(p, `{"v":{"response":{"fragments":[{"type":"response","content":""}]}}}`));
	all.push(...feed(p, `{"v":"{\\"tool"}`));
	all.push(...feed(p, `{"p":"response/status","o":"SET","v":"WIP"}`));
	all.push(...feed(p, `{"v":"\\":\\"fetch_url\\",\\"url\\":\\"https://httpbin.org/get\\"}"}`));
	assert.equal(
		all.join(""),
		`{"tool":"fetch_url","url":"https://httpbin.org/get"}`,
		`deltas=${JSON.stringify(all)}`,
	);
});

test("message id variants", () => {
	let p = new PatchParser();
	feed(p, `{"v":{"response":{"fragments":[{"type":"response","content":"x"}]},"id":7}}`);
	assert.equal(p.messageId, 7, "v.id variant");

	p = new PatchParser();
	feed(p, `{"v":{"response":{"message_id":8,"fragments":[{"type":"response","content":"x"}]}}}`);
	assert.equal(p.messageId, 8, "v.response.message_id variant");

	p = new PatchParser();
	feed(p, `{"p":"response/message_id","o":"SET","v":9}`);
	assert.equal(p.messageId, 9, "path-frame variant");
});

test("fragment type is case-insensitive", () => {
	const p = new PatchParser();
	assert.deepEqual(feed(p, `{"v":{"response":{"fragments":[{"type":"response","content":"lower"}]}}}`), ["lower"]);
});

test("non-response fragments are ignored", () => {
	const p = new PatchParser();
	const got = feed(p, `{"v":{"response":{"fragments":[
		{"type":"thinking","content":"not shown"},
		{"type":"response","content":"shown"}
	]}}}`);
	assert.deepEqual(got, ["shown"]);
});

test("single snapshot fragment emitted once", () => {
	const p = new PatchParser();
	const all = feed(p, `{"v":{"response":{"fragments":[
		{"type":"response","content":"first"},
		{"type":"response","content":"second"}
	]}}}`);
	// The second response fragment with pre-generated content is not emitted
	// (matches the site client); its content arrives as appends instead.
	assert.deepEqual(all, ["first"]);
});

test("malformed payloads are skipped", () => {
	const p = new PatchParser();
	for (const payload of ["not json", "[DONE]", `{"x":1}`, `{"v":42}`, ""]) {
		assert.deepEqual(feed(p, payload), [], `payload ${payload}`);
	}
});

test("captures search sources", () => {
	const p = new PatchParser();
	const snapshot = `{"v":{"response":{"fragments":[
		{"type":"response","content":"Gold is high [citation:1][citation:2]"},
		{"type":"tool_search","references":[
			{"url":"https://ex.com/gold","title":"Gold Prices"},
			{"url":"https://ex.com/spot"}
		]}
	],"message_id":1},"message_id":1}}`;
	const out = feed(p, snapshot);
	assert.equal(out.length, 1);
	assert.ok(out[0]!.includes("[citation:1]"));
	const want: Source[] = [
		{ url: "https://ex.com/gold", title: "Gold Prices" },
		{ url: "https://ex.com/spot", title: "" },
	];
	assert.deepEqual(p.sources, want);

	// A .../results patch appends more, deduplicated by URL.
	p.feed(`{"p":"response/fragments/0/results","o":"SET","v":[{"url":"https://ex.com/gold"},{"url":"https://new.com"}]}`, () => {});
	assert.equal(p.sources.length, 3);
	assert.equal(p.sources[2]!.url, "https://new.com");
});

test("first chunk before any path (missing-initial-characters bug)", () => {
	const p = new PatchParser();
	const all: string[] = [];
	for (const payload of [
		`{"v":"D"}`,
		`{"v":"ựa"}`,
		`{"p":"response/fragments/-1/content","o":"APPEND","v":" trên"}`,
	]) {
		all.push(...feed(p, payload));
	}
	assert.deepEqual(all, ["D", "ựa", " trên"]);
});

test("empty snapshot then pathless", () => {
	const p = new PatchParser();
	const all = feed(p, `{"v":{"response":{"fragments":[{"type":"response","content":""}],"message_id":1},"message_id":1}}`);
	all.push(...feed(p, `{"v":"Hi"}`));
	assert.deepEqual(all, ["Hi"]);
});

test("SET as initial", () => {
	const p = new PatchParser();
	const all = feed(p, `{"v":{"response":{"fragments":[{"type":"response","content":""}],"message_id":1},"message_id":1}}`);
	all.push(...feed(p, `{"p":"response/fragments/-1/content","o":"SET","v":"Hi"}`));
	all.push(...feed(p, `{"p":"response/fragments/-1/content","o":"APPEND","v":"!"}`));
	assert.deepEqual(all, ["Hi", "!"]);
});

test("SET after emit is skipped", () => {
	const p = new PatchParser();
	const all = feed(p, `{"v":{"response":{"fragments":[{"type":"response","content":"Hi"}],"message_id":1},"message_id":1}}`);
	all.push(...feed(p, `{"p":"response/fragments/-1/content","o":"SET","v":"Hi there"}`));
	all.push(...feed(p, `{"p":"response/fragments/-1/content","o":"APPEND","v":"!"}`));
	assert.deepEqual(all, ["Hi", "!"]);
});

test("object form of pathless v", () => {
	const p = new PatchParser();
	const all: string[] = [];
	all.push(...feed(p, `{"v":{"text":"A"}}`));
	all.push(...feed(p, `{"v":{"content":"B"}}`));
	assert.deepEqual(all, ["A", "B"]);
});

test("BATCH content patch", () => {
	const p = new PatchParser();
	const all = feed(p, `{"p":"response","o":"BATCH","v":[{"p":"fragments/-1/content","o":"APPEND","v":"x"}]}`);
	assert.deepEqual(all, ["x"]);
});

test("multiple frames per event (joined data lines)", () => {
	const p = new PatchParser();
	const payload =
		`{"v":{"response":{"fragments":[{"type":"response","content":""}],"message_id":1},"message_id":1}}` +
		"\n" +
		`{"p":"response/fragments/-1/content","o":"APPEND","v":"As "}` +
		"\n" +
		`{"p":"response/fragments/-1/content","o":"APPEND","v":"of Aug"}` +
		"\n" +
		`{"v":"ust"}` +
		"\n" +
		`{"p":"response/fragments/-1/content","o":"SET","v":"As of August"}`; // full-slot SET after emission: skipped
	assert.deepEqual(feed(p, payload), ["As ", "of Aug", "ust"]);
});

test("op-less content frame continues the stream", () => {
	const p = new PatchParser();
	const all: string[] = [];
	all.push(...feed(p, `{"v":{"response":{"fragments":[{"type":"THINK","content":"We"}]}}}`));
	all.push(...feed(p, `{"p":"response/fragments","o":"APPEND","v":[{"type":"RESPONSE","content":"{\\""}]}`));
	all.push(...feed(p, `{"p":"response/fragments/-1/content","v":"tool"}`)); // op-less: continuation
	all.push(...feed(p, `{"v":"\\":\\"fetch_url\\",\\"url\\":\\"https://httpbin.org/get\\"}"}`));
	assert.equal(
		all.join(""),
		`{"tool":"fetch_url","url":"https://httpbin.org/get"}`,
		`deltas=${JSON.stringify(all)}`,
	);
});

test("op-less first content frame", () => {
	const p = new PatchParser();
	const all = feed(p, `{"v":{"response":{"fragments":[{"type":"response","content":""}],"message_id":1},"message_id":1}}`);
	all.push(...feed(p, `{"p":"response/fragments/-1/content","v":"As "}`));
	all.push(...feed(p, `{"p":"response/fragments/-1/content","o":"APPEND","v":"of"}`));
	assert.deepEqual(all, ["As ", "of"]);
});

test("container-appended RESPONSE fragment", () => {
	const p = new PatchParser();
	const all: string[] = [];
	all.push(...feed(p, `{"v":{"response":{"fragments":[{"id":2,"type":"THINK","content":"thinking..."}]}}}`));
	all.push(...feed(p, `{"p":"response/fragments","o":"APPEND","v":[{"id":3,"type":"RESPONSE","content":"Based"}]}`));
	all.push(...feed(p, `{"p":"response/fragments/-1/content","o":"APPEND","v":" on the"}`));
	all.push(...feed(p, `{"v":" search"}`));
	all.push(...feed(p, `{"v":" results"}`));
	assert.deepEqual(all, ["Based", " on the", " search", " results"]);
	// A TOOL_SEARCH fragment appended the same way also yields sources.
	const p2 = new PatchParser();
	feed(p2, `{"p":"response/fragments","o":"APPEND","v":[{"id":4,"type":"tool_search","references":[{"url":"https://ex.com/g","title":"G"}]}]}`);
	assert.equal(p2.sources.length, 1);
	assert.equal(p2.sources[0]!.url, "https://ex.com/g");
});

test("thinking never leaks", () => {
	const p = new PatchParser();
	const all: string[] = [];
	all.push(...feed(p, `{"v":{"response":{"fragments":[{"type":"THINK","content":"OK"}]}}}`));
	all.push(...feed(p, `{"p":"response/fragments/-1/content","o":"APPEND","v":", the user"}`));
	all.push(...feed(p, `{"v":" asks"}`));
	all.push(...feed(p, `{"p":"response/fragments","o":"APPEND","v":[{"type":"RESPONSE","content":"**Yes"}]}`));
	all.push(...feed(p, `{"p":"response/fragments/-1/content","o":"APPEND","v":", I can"}`));
	all.push(...feed(p, `{"v":" read!"}`));
	assert.deepEqual(all, ["**Yes", ", I can", " read!"]);
});

test("interleaved thinking does not eat answer text", () => {
	const p = new PatchParser();
	const all: string[] = [];
	all.push(...feed(p, `{"v":{"response":{"fragments":[{"type":"THINK","content":"thinking"}]}}}`));
	all.push(...feed(p, `{"p":"response/fragments","o":"APPEND","v":[{"type":"RESPONSE","content":"{"}]}`));
	all.push(...feed(p, `{"p":"response/fragments","o":"APPEND","v":[{"type":"THINK","content":"(more reasoning)"}]}`));
	all.push(...feed(p, `{"p":"response/fragments/-1/content","o":"APPEND","v":"\\"tool"}`));
	all.push(...feed(p, `{"v":"\\":\\"fetch_url\\",\\"url\\":\\"https://httpbin.org/get\\"}"}`));
	assert.equal(
		all.join(""),
		`{"tool":"fetch_url","url":"https://httpbin.org/get"}`,
		`deltas=${JSON.stringify(all)}`,
	);
});

test("per-fragment SET after thinking", () => {
	const p = new PatchParser();
	const all: string[] = [];
	all.push(...feed(p, `{"v":{"response":{"fragments":[{"type":"THINK","content":"thinking..."}]}}}`));
	all.push(...feed(p, `{"p":"response/fragments/-1/content","o":"SET","v":"more thinking"}`)); // THINK: skipped
	all.push(...feed(p, `{"p":"response/fragments","o":"APPEND","v":[{"type":"RESPONSE","content":""}]}`));
	all.push(...feed(p, `{"p":"response/fragments/-1/content","o":"SET","v":"**Yes"}`)); // RESPONSE initial: kept
	all.push(...feed(p, `{"p":"response/fragments/-1/content","o":"SET","v":"**Yes, I can read!"}`)); // full replace: skipped
	all.push(...feed(p, `{"p":"response/fragments/-1/content","o":"APPEND","v":"!"}`));
	assert.deepEqual(all, ["**Yes", "!"]);
});

test("indexed content respects fragment type", () => {
	const p = new PatchParser();
	const all: string[] = [];
	all.push(
		...feed(p, `{"v":{"response":{"fragments":[
		{"type":"THINK","content":"t"},
		{"type":"RESPONSE","content":"A"}
	]}}}`),
	);
	all.push(...feed(p, `{"p":"response/fragments/0/content","o":"APPEND","v":"hink"}`)); // THINK: skipped
	all.push(...feed(p, `{"p":"response/fragments/1/content","o":"APPEND","v":"nswer"}`)); // RESPONSE: emitted
	assert.deepEqual(all, ["A", "nswer"]);
});
