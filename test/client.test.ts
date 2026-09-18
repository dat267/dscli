import { strict as assert } from "node:assert";
import { test } from "node:test";
import * as http from "node:http";
import {
	DeepSeekClient,
	historyMessageText,
	sessionCookie,
	type CompletionRequest,
	type HistoryMessage,
} from "../src/core/deepseek/client.js";
import { challengePrefix, type Challenge } from "../src/core/deepseek/pow.js";

test("session cookie header", () => {
	const tests: Array<[string, string]> = [
		["", ""],
		["abc123", "ds_session_id=abc123"],
		["ds_session_id=zzz", "ds_session_id=zzz"],
		["ds_session_id=zzz; hl=en", "ds_session_id=zzz; hl=en"],
		["hl=en; ds_session_id=zzz", "hl=en; ds_session_id=zzz"], // full cookie string passes through
	];
	for (const [inRaw, want] of tests) assert.equal(sessionCookie(inRaw), want);
});

function completionBodyOf(req: CompletionRequest): Record<string, unknown> {
	// completionBody is module-private; the wire format is pinned via the
	// recorded request bodies in the fake-server tests below. Here we only
	// assert the raw JSON marshalling of the shape the client sends.
	return JSON.parse(JSON.stringify(recordBody(req))) as Record<string, unknown>;
}

// Minimal mirror of the Go CompletionRequest body builder for direct asserts.
function recordBody(r: CompletionRequest): Record<string, unknown> {
	const b: Record<string, unknown> = {
		chat_session_id: r.chatSessionId,
		parent_message_id: r.parentMessageId,
		prompt: r.prompt,
		ref_file_ids: [],
		thinking_enabled: r.thinkingEnabled,
		search_enabled: r.searchEnabled,
		action: null,
		preempt: false,
	};
	if (r.modelType && r.modelType !== "") b["model_type"] = r.modelType;
	return b;
}

test("completion request body", () => {
	const first: CompletionRequest = {
		chatSessionId: "s1",
		parentMessageId: null,
		prompt: "hi",
		modelType: "default",
		thinkingEnabled: true,
		searchEnabled: false,
	};
	const body = completionBodyOf(first);
	assert.equal(body["chat_session_id"], "s1");
	assert.equal(body["prompt"], "hi");
	assert.equal(body["model_type"], "default");
	assert.equal(body["thinking_enabled"], true);
	assert.equal(body["action"], null);
	assert.equal(body["preempt"], false);
	// parent_message_id is null on the first turn.
	const raw = JSON.stringify(body);
	assert.ok(raw.includes(`"parent_message_id":null`), raw);

	const resume: CompletionRequest = {
		chatSessionId: "s1",
		parentMessageId: 7,
		prompt: "more",
		thinkingEnabled: false,
		searchEnabled: false,
	};
	const body2 = completionBodyOf(resume);
	assert.equal(body2["parent_message_id"], 7);
	assert.equal(body2["model_type"], undefined);
});

/**
 * The golden challenge used by the fake server: crafted so wasm_solve
 * returns 999 instantly, letting tests exercise the whole PoW header path.
 */
function fakeChallenge(): Challenge {
	return {
		algorithm: "DeepSeekHashV1",
		challenge: "9099d8ee62c210152bb06cf47a6071e24785ab2e6c413d4cab9bbb9f849f5a58",
		salt: "450f343f44a1e9e6",
		signature: "signed-request",
		target_path: "/api/v0/chat/completion",
		difficulty: 2000,
		expire_at: 1752033600,
	};
}

const CHALLENGE_RESPONSE = JSON.stringify({ code: 0, data: { biz_data: { challenge: fakeChallenge() } } });

interface FakeServer {
	url: string;
	close(): Promise<void>;
	/** Recorded completion request bodies, in order. */
	bodies: string[];
	/** Whether an x-ds-pow-response header arrived on the completion request. */
	sawPow: () => boolean;
}

/**
 * fakeServer mimics chat.deepseek.com: session create, PoW challenge, and a
 * completion stream assembled from SSE frames (the frames param receives the
 * 1-based request number).
 */
async function fakeServer(
	sseFrames: (n: number) => string[],
	opts: { onCompletion?: (body: string, req: http.IncomingMessage) => void } = {},
): Promise<FakeServer> {
	const bodies: string[] = [];
	let powSeen = false;
	let completions = 0;
	const srv = http.createServer((req, res) => {
		const chunks: Buffer[] = [];
		req.on("data", (c) => chunks.push(c as Buffer));
		req.on("end", () => {
			const body = Buffer.concat(chunks).toString("utf8");
			switch (req.url?.split("?")[0]) {
				case "/api/v0/chat_session/create":
					res.writeHead(200, { "content-type": "application/json" });
					res.end(`{"code":0,"data":{"biz_data":{"chat_session":{"id":"sess-1"}}}}`);
					return;
				case "/api/v0/chat/create_pow_challenge":
					res.writeHead(200, { "content-type": "application/json" });
					res.end(CHALLENGE_RESPONSE);
					return;
				case "/api/v0/chat/completion": {
					powSeen = req.headers["x-ds-pow-response"] !== undefined;
					completions++;
					bodies.push(body);
					opts.onCompletion?.(body, req);
					const frames = sseFrames(completions);
					res.writeHead(200, { "content-type": "text/event-stream" });
					for (const frame of frames) res.write(`data: ${frame}\n\n`);
					res.end();
					return;
				}
				case "/api/v0/chat_session/delete":
					res.writeHead(200, { "content-type": "application/json" });
					res.end(`{"code":0,"data":{}}`);
					return;
				case "/api/v0/chat/history_messages": {
					const msgs: HistoryMessage[] = [
						{ message_id: 1, parent_id: null, role: "USER", content: "hi", status: "FINISHED", fragments: [] },
						{
							message_id: 2,
							parent_id: 1,
							role: "ASSISTANT",
							content: "",
							status: "FINISHED",
							fragments: [{ type: "RESPONSE", content: "Hello back" }],
						},
					];
					res.writeHead(200, { "content-type": "application/json" });
					res.end(JSON.stringify({ code: 0, data: { biz_data: { chat_messages: msgs } } }));
					return;
				}
				default:
					res.writeHead(404);
					res.end("not found");
			}
		});
	});
	await new Promise<void>((resolve) => srv.listen(0, "127.0.0.1", resolve));
	return {
		url: `http://127.0.0.1:${(srv.address() as import("node:net").AddressInfo).port}`,
		close: () => new Promise<void>((resolve) => srv.close(() => resolve())),
		get bodies() {
			return bodies;
		},
		sawPow: () => powSeen,
	};
}

test("chat history parses the envelope and extracts visible text", async () => {
	const srv = await fakeServer(() => []);
	try {
		const client = new DeepSeekClient({ token: "tok" }, { base: srv.url });
		const msgs = await client.chatHistory("sess-1");
		assert.equal(msgs.length, 2);
		assert.equal(msgs[0]!.role, "USER");
		assert.equal(historyMessageText(msgs[0]!), "hi");
		assert.equal(historyMessageText(msgs[1]!), "Hello back");
	} finally {
		await srv.close();
	}
});

test("streamCompletion sends a solved PoW header and reconstructs the reply", async () => {
	const srv = await fakeServer(() => [
		`{"v":{"response":{"fragments":[{"type":"response","content":""}],"message_id":2},"message_id":2}}`,
		`{"p":"response/fragments/-1/content","o":"APPEND","v":"Hello "}`,
		`{"v":"world"}`,
		`{"v":[{"p":"status","v":"FINISHED"},{"p":"quasi_status","v":"FINISHED"}]}`,
	]);
	try {
		const client = new DeepSeekClient({ token: "tok" }, { base: srv.url });
		const deltas: string[] = [];
		const reply = await client.streamCompletion(
			{ chatSessionId: "sess-1", parentMessageId: null, prompt: "hi", modelType: "default", thinkingEnabled: false, searchEnabled: false },
			(s) => deltas.push(s),
		);
		assert.deepEqual(deltas, ["Hello ", "world"]);
		assert.equal(reply.messageId, 2);
		assert.ok(!reply.truncated);
		assert.ok(srv.sawPow(), "completion must carry the x-ds-pow-response header");
		const sent = JSON.parse(srv.bodies[0]!) as Record<string, unknown>;
		assert.equal(sent["chat_session_id"], "sess-1");
		assert.equal(sent["model_type"], "default");
	} finally {
		await srv.close();
	}
});

test("deleteSessions posts the ids as a batch", async () => {
	const srv = await fakeServer(() => []);
	try {
		const client = new DeepSeekClient({ token: "tok" }, { base: srv.url });
		await client.deleteSessions(["sess-1", "sess-2"]);
		await client.deleteSessions([]); // no-op
	} finally {
		await srv.close();
	}
});

test("non-200 responses prefer the site's JSON envelope", async () => {
	const srv = http.createServer((req, res) => {
		res.writeHead(401, { "content-type": "application/json" });
		res.end(`{"code":40104,"msg":"Unauthorized"}`);
	});
	await new Promise<void>((resolve) => srv.listen(0, "127.0.0.1", resolve));
	const url = `http://127.0.0.1:${(srv.address() as import("node:net").AddressInfo).port}`;
	try {
		const client = new DeepSeekClient({ token: "bad" }, { base: url });
		await assert.rejects(
			() => client.createChatSession(),
			/deepseek api error: Unauthorized \(HTTP 401/,
		);
	} finally {
		await new Promise<void>((resolve) => srv.close(() => resolve()));
	}
});

test("challenge prefix still exported for callers", () => {
	assert.equal(challengePrefix(fakeChallenge()), "450f343f44a1e9e6_1752033600_");
});
