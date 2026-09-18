import { strict as assert } from "node:assert";
import { test } from "node:test";
import * as http from "node:http";
import { DeepSeekClient } from "../src/core/deepseek/client.js";
import {
	chunkText,
	defaultOutput,
	firstChunk,
	langCode,
	prompt,
	summarize,
	translate,
} from "../src/core/translate/engine.js";
import { AdaptiveSizer, idealChunk, shrinkChunk } from "../src/core/translate/sizer.js";

/** sseReply builds one completion's SSE frames emitting content with message_id n+1. */
function sseReply(id: number, content: string): string[] {
	return [
		`{"v":{"response":{"fragments":[{"type":"response","content":""}],"message_id":${id}},"message_id":${id}}}`,
		`{"p":"response/fragments/-1/content","o":"APPEND","v":"${content}"}`,
		`{"v":[{"p":"status","v":"FINISHED"},{"p":"quasi_status","v":"FINISHED"}]}`,
	];
}

interface Fake {
	url: string;
	close(): Promise<void>;
	replies: string[]; // raw completion bodies (one per request)
	bodies: string[];
	created: number;
}

/** fakeServer: sessions + challenge + scripted completion replies. */
async function fakeServer(replies: string[]): Promise<Fake> {
	const bodies: string[] = [];
	let created = 0;
	let n = 0;
	const srv = http.createServer((req, res) => {
		const chunks: Buffer[] = [];
		req.on("data", (c) => chunks.push(c as Buffer));
		req.on("end", () => {
			const body = Buffer.concat(chunks).toString("utf8");
			if (req.url === "/api/v0/chat_session/create") {
				created++;
				// Each create gets a unique session id.
				res.writeHead(200, { "content-type": "application/json" });
				res.end(`{"code":0,"data":{"biz_data":{"chat_session":{"id":"sess-${created}"}}}}`);
				return;
			}
			if (req.url === "/api/v0/chat/create_pow_challenge") {
				res.writeHead(200, { "content-type": "application/json" });
				res.end(
					JSON.stringify({
						code: 0,
						data: {
							biz_data: {
								challenge: {
									algorithm: "DeepSeekHashV1",
									challenge: "9099d8ee62c210152bb06cf47a6071e24785ab2e6c413d4cab9bbb9f849f5a58",
									salt: "450f343f44a1e9e6",
									signature: "signed-request",
									target_path: "/api/v0/chat/completion",
									difficulty: 2000,
									expire_at: 1752033600,
								},
							},
						},
					}),
				);
				return;
			}
			if (req.url === "/api/v0/chat/completion") {
				bodies.push(body);
				const reply = replies[Math.min(n, replies.length - 1)]!;
				n++;
				res.writeHead(200, { "content-type": "text/event-stream" });
				for (const line of reply.split("\n")) res.write(`data: ${line}\n`);
				res.write("\n");
				res.end();
				return;
			}
			if (req.url === "/api/v0/chat_session/delete") {
				res.writeHead(200, { "content-type": "application/json" });
				res.end(`{"code":0,"data":{}}`);
				return;
			}
			res.writeHead(404);
			res.end();
		});
	});
	await new Promise<void>((resolve) => srv.listen(0, "127.0.0.1", resolve));
	return {
		url: `http://127.0.0.1:${(srv.address() as import("node:net").AddressInfo).port}`,
		close: () => new Promise<void>((resolve) => srv.close(() => resolve())),
		get replies() {
			return replies;
		},
		get bodies() {
			return bodies;
		},
		get created() {
			return created;
		},
	};
}

const client = (url: string) => new DeepSeekClient({ token: "tok" }, { base: url });

test("firstChunk keeps lines intact and hard-splits over-long lines", () => {
	assert.equal(firstChunk("", 100), "");
	assert.equal(firstChunk("hello", 100), "hello");
	// Byte budget: "a\n" (2) + "bb\n" (3) exceeds 4, so only the first line fits.
	assert.equal(firstChunk("a\nbb\nccc\n", 4), "a\n");
	assert.equal(firstChunk("a\nbb\nccc\n", 2), "a\n");
	// A line longer than the budget is hard-split at the boundary.
	assert.equal(firstChunk("abcdef", 3), "abc");
	// The final newline is reproduced — including the phantom split element
	// (Go's FirstChunk("x\n", 10) is "x\n\n"; the model reply trims it).
	assert.equal(firstChunk("x\n", 10), "x\n\n");
	// Multi-byte characters are never split.
	const ja = "あああああ";
	assert.equal(firstChunk(ja, 7), "ああ"); // 3 bytes each: 6 fits, 9 does not
});

test("chunkText splits fully; empty text yields one empty chunk", () => {
	assert.deepEqual(chunkText("", 10), [""]);
	assert.deepEqual(chunkText("a\nb\nc\n", 2), ["a\n", "b\n", "c\n"]);
	const big = "abcdefghij".repeat(3);
	const parts = chunkText(big, 10);
	assert.equal(parts.join(""), big);
	assert.ok(parts.every((p) => Buffer.byteLength(p) <= 10));
});

test("defaultOutput and langCode", () => {
	assert.equal(defaultOutput("chapter.md", "Japanese"), "chapter.translated.ja.md");
	assert.equal(defaultOutput("a.translated.en.md", "Chinese"), "a.translated.zh.md");
	assert.equal(defaultOutput("book.epub", "English"), "book.translated.en.txt");
	assert.equal(defaultOutput("x.txt", "日本語"), "x.translated.txt");
	assert.equal(langCode("English"), "en");
	assert.equal(langCode(" japanese "), "ja");
	assert.equal(langCode("日本語"), "");
});

test("ideal and shrink chunk math", () => {
	assert.equal(idealChunk(36_000, 0, 1 << 20), 1 << 20);
	// ratio 2: 36000*0.85/2 = 15300
	assert.equal(idealChunk(36_000, 2, 1 << 20), 15300);
	assert.equal(shrinkChunk(8000, 36_000, 0), 4000); // half wins (est 7650 is larger)
	assert.equal(shrinkChunk(800, 36_000, 0), 1024); // floored
});

test("adaptive sizer learns and grows to the cap", () => {
	const s = new AdaptiveSizer(1 << 20);
	assert.equal(s.size(), 8 * 1024);
	// A terse reply learns a tiny ratio -> grow to the cap.
	s.success(8 * 1024, 4);
	assert.equal(s.size(), 1 << 20);
	// Truncation at the cap shrinks; the cap is re-learned from the partial.
	assert.equal(s.truncated(40_000), true);
	assert.ok(s.size() < 1 << 20);
});

test("translate: adaptive growth chunks a big file into probe + capped chunks", async () => {
	const MAX = 64 * 1024;
	const srv = await fakeServer([sseReply(2, "ok\\n").join("\n")]);
	try {
		const content = "a".repeat(3 * MAX);
		const { text, convId } = await translate(client(srv.url), "sess-1", content, "text", { to: "English", chunkBytes: MAX });
		// Probe (8 KiB) + three capped chunks (64 KiB, 64 KiB, 57344).
		assert.equal(srv.bodies.length, 4, `bodies=${srv.bodies.length}`);
		assert.equal(convId, "sess-1:2");
		assert.equal(text, "ok\nok\nok\nok\n");
	} finally {
		await srv.close();
	}
});

test("translate: verification retry rescues a corrupted timestamp", async () => {
	const srv = await fakeServer([sseReply(2, "changed --> timing\\n").join("\n")]);
	try {
		const content = "1\n00:00:01,000 --> 00:00:02,000\nHello\n";
		const { text } = await translate(client(srv.url), "sess-1", content, "srt", { to: "English" });
		// The retry's reply is accepted (the fake always returns the same
		// frames, so the second attempt also "fails" verification — the run
		// must fail loudly rather than write a corrupted chunk).
	} catch (err) {
		assert.match(err instanceof Error ? err.message : String(err), /protected line 1 changed/);
	} finally {
		await srv.close();
	}
});

test("translate: strict retry accepts a corrected second attempt", async () => {
	// First attempt breaks the timing line; the retry keeps it byte-for-byte.
	const bad = [
		`{"v":{"response":{"fragments":[{"type":"response","content":""}],"message_id":2},"message_id":2}}`,
		`{"p":"response/fragments/-1/content","o":"APPEND","v":"1\\n00:00:01,500 --> 00:00:02,000\\nHola\\n"}`,
		`{"v":[{"p":"status","v":"FINISHED"},{"p":"quasi_status","v":"FINISHED"}]}`,
	];
	const good = [
		`{"v":{"response":{"fragments":[{"type":"response","content":""}],"message_id":3},"message_id":3}}`,
		`{"p":"response/fragments/-1/content","o":"APPEND","v":"1\\n00:00:01,000 --> 00:00:02,000\\nHola\\n"}`,
		`{"v":[{"p":"status","v":"FINISHED"},{"p":"quasi_status","v":"FINISHED"}]}`,
	];
	const srv = await fakeServer([bad.join("\n"), good.join("\n")]);
	try {
		const content = "1\n00:00:01,000 --> 00:00:02,000\nHello\n";
		const { text } = await translate(client(srv.url), "sess-1", content, "srt", { to: "English" });
		assert.equal(text, "1\n00:00:01,000 --> 00:00:02,000\nHola\n");
		assert.equal(srv.bodies.length, 2);
		assert.match(srv.bodies[1]!, /TRANSLATION VERIFICATION FAILED LAST TIME/);
	} finally {
		await srv.close();
	}
});

test("translate: truncation shrinks and re-splits without losing content", async () => {
	// The first reply is "truncated" (WIP status); the retry succeeds.
	const truncated = [
		`{"v":{"response":{"fragments":[{"type":"response","content":""}],"message_id":2},"message_id":2}}`,
		`{"p":"response/fragments/-1/content","o":"APPEND","v":"partial"}`,
		`{"v":[{"p":"status","v":"INCOMPLETE"}]}`,
	].join("\n");
	const ok = sseReply(3, "full").join("\n");
	const srv = await fakeServer([truncated, ok]);
	try {
		const content = "x".repeat(20 * 1024); // one 8 KiB probe + rest
		const { text } = await translate(client(srv.url), "sess-1", content, "text", { to: "English" });
		assert.ok(text.endsWith("full\n"), text.slice(-20));
		assert.ok(srv.bodies.length >= 2);
	} finally {
		await srv.close();
	}
});

test("prompt includes format rules, style and the reminder", () => {
	const p = prompt("srt", "Japanese", "English", false, "[STYLE]\nkeep names\n");
	assert.match(p, /Translate the following SRT subtitles from Japanese to English\./);
	assert.match(p, /Preserve the cue index numbers/);
	assert.match(p, /\[STYLE\]\nkeep names\n/);
	assert.match(p, /Reply with ONLY the translated content/);
	const r = prompt("srt", "", "", true, "");
	assert.match(r, /from auto to English\./);
	assert.match(r, /TRANSLATION VERIFICATION FAILED LAST TIME/);
});

test("summarize: whole file in one reply, no combine pass", async () => {
	const srv = await fakeServer([sseReply(2, "A terse summary.\\n").join("\n")]);
	try {
		const content = "word ".repeat(6 * 1024); // 24 KiB, over the 8 KiB probe
		const { text } = await summarize(client(srv.url), "sess-1", content, "text", {});
		assert.equal(text, "A terse summary.\n");
		assert.equal(srv.bodies.length, 1);
		assert.match(srv.bodies[0]!, /Summarize the following/);
	} finally {
		await srv.close();
	}
});

test("summarize: multi-chunk runs a combine pass", async () => {
	const s1 = sseReply(2, "Section one summary.\\n").join("\n");
	const s2 = sseReply(3, "Section two summary.\\n").join("\n");
	const combined = sseReply(4, "The combined summary.\\n").join("\n");
	const srv = await fakeServer([s1, s2, combined]);
	try {
		const content = "a ".repeat(12 * 1024); // 24 KiB with a 16 KiB cap: two chunks
		const { text } = await summarize(client(srv.url), "sess-1", content, "text", { chunkBytes: 16 * 1024 });
		assert.equal(text, "The combined summary.\n");
		assert.equal(srv.bodies.length, 3);
		assert.match(srv.bodies[2]!, /single coherent summary/);
		assert.match(srv.bodies[2]!, /Section one summary\./);
	} finally {
		await srv.close();
	}
});
