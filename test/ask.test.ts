import { strict as assert } from "node:assert";
import { test } from "node:test";
import * as http from "node:http";
import { join } from "node:path";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { askCommand } from "../src/cli/commands/ask.js";

const tmp = () => join(mkdtempSync(join(tmpdir(), "dscli-")), "dscli.json");

function fakeChallenge(): object {
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

interface FakeServer {
	url: string;
	close(): Promise<void>;
	bodies: string[];
	created: number;
	deleted: string[];
}

async function fakeServer(frames: string[]): Promise<FakeServer> {
	const bodies: string[] = [];
	const deleted: string[] = [];
	let created = 0;
	const srv = http.createServer((req, res) => {
		const chunks: Buffer[] = [];
		req.on("data", (c) => chunks.push(c as Buffer));
		req.on("end", () => {
			const body = Buffer.concat(chunks).toString("utf8");
			switch (req.url?.split("?")[0]) {
				case "/api/v0/chat_session/create":
					created++;
					res.writeHead(200, { "content-type": "application/json" });
					res.end(`{"code":0,"data":{"biz_data":{"chat_session":{"id":"sess-1"}}}}`);
					return;
				case "/api/v0/chat/create_pow_challenge":
					res.writeHead(200, { "content-type": "application/json" });
					res.end(JSON.stringify({ code: 0, data: { biz_data: { challenge: fakeChallenge() } } }));
					return;
				case "/api/v0/chat/completion":
					bodies.push(body);
					res.writeHead(200, { "content-type": "text/event-stream" });
					for (const frame of frames) res.write(`data: ${frame}\n\n`);
					res.end();
					return;
				case "/api/v0/chat_session/delete":
					deleted.push(body);
					res.writeHead(200, { "content-type": "application/json" });
					res.end(`{"code":0,"data":{}}`);
					return;
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
		get created() {
			return created;
		},
		get deleted() {
			return deleted;
		},
	};
}

const replyFrames = (text: string): string[] => [
	`{"v":{"response":{"fragments":[{"type":"response","content":""}],"message_id":2},"message_id":2}}`,
	`{"p":"response/fragments/-1/content","o":"APPEND","v":"${text}"}`,
	`{"v":[{"p":"status","v":"FINISHED"},{"p":"quasi_status","v":"FINISHED"}]}`,
];

/** captureStream: a minimal WriteStream collecting writes into a string. */
function captureStream(): { stream: NodeJS.WriteStream; text: () => string } {
	const chunks: string[] = [];
	const stream = {
		write: (chunk: string | Uint8Array) => {
			chunks.push(typeof chunk === "string" ? chunk : Buffer.from(chunk).toString("utf8"));
			return true;
		},
	} as unknown as NodeJS.WriteStream;
	return { stream, text: () => chunks.join("") };
}

test("ask: ephemeral by default — fresh session, deleted at the end, nothing saved", async () => {
	const srv = await fakeServer(replyFrames("Hello back"));
	try {
		const cfg = tmp();
		const cap = captureStream();
		await askCommand({
				cfgPath: cfg,
				stdout: cap.stream,
				prompt: ["hi"],
				model: "",
				thinking: false,
				search: false,
				persist: false,
				noTranscript: false,
				jsonOut: false,
				timeoutMs: 0,
				token: "tok",
				cookie: "",
				userAgent: "",
				clientBase: srv.url,
			});
		assert.equal(cap.text(), "Hello back\n");
		assert.equal(srv.created, 1);
		assert.equal(srv.deleted.length, 1, "ephemeral session is deleted server-side");
		assert.equal(srv.bodies.length, 1);
		const sent = JSON.parse(srv.bodies[0]!) as Record<string, unknown>;
		assert.equal(sent["chat_session_id"], "sess-1");
		assert.equal(sent["model_type"], "default");
		assert.equal(sent["parent_message_id"], null);
	} finally {
		await srv.close();
	}
});

test("ask: --persist saves the advanced position and the transcript", async () => {
	const srv = await fakeServer(replyFrames("one"));
	try {
		const cfg = tmp();
		await askCommand({
				cfgPath: cfg,
				stdout: captureStream().stream,
				prompt: ["first"],
				model: "",
				thinking: false,
				search: false,
				persist: true,
				noTranscript: false,
				jsonOut: false,
				timeoutMs: 0,
				token: "tok",
				cookie: "",
				userAgent: "",
				clientBase: srv.url,
			});
		const { loadSavedSession } = await import("../src/core/session.js");
		assert.equal(loadSavedSession(cfg), "sess-1:2");
		const { loadTranscript } = await import("../src/core/transcript.js");
		const entries = loadTranscript(cfg, "sess-1")!;
		assert.equal(entries.length, 2);
		assert.equal(entries[0]!.role, "user");
		assert.equal(entries[0]!.text, "first");
		assert.equal(entries[1]!.text, "one");
		// Persisted runs do not delete the session.
		assert.equal(srv.deleted.length, 0);
	} finally {
		await srv.close();
	}
});

test("ask: --json-out emits one delta line per chunk plus sources", async () => {
	const frames = [
		`{"v":{"response":{"fragments":[{"type":"response","content":""}],"message_id":2},"message_id":2}}`,
		`{"p":"response/fragments/-1/content","o":"APPEND","v":"Hel"}`,
		`{"v":"lo"}`,
		`{"v":{"response":{"fragments":[{"type":"response","content":""},{"type":"tool_search","references":[{"url":"https://ex.com/a","title":"A"}]}]}}}`,
		`{"v":[{"p":"status","v":"FINISHED"}]}`,
	];
	const srv = await fakeServer(frames);
	try {
		const cap = captureStream();
		await askCommand({
				cfgPath: tmp(),
				stdout: cap.stream,
				prompt: ["hi"],
				model: "",
				thinking: false,
				search: true,
				persist: false,
				noTranscript: false,
				jsonOut: true,
				timeoutMs: 0,
				token: "tok",
				cookie: "",
				userAgent: "",
				clientBase: srv.url,
			});
		const lines = cap.text().trim().split("\n").map((l) => JSON.parse(l) as Record<string, unknown>);
		assert.deepEqual(
			lines.filter((l) => "delta" in l).map((l) => l["delta"]),
			["Hel", "lo"],
		);
		const sources = lines.find((l) => "sources" in l);
		assert.ok(sources, "sources line present");
	} finally {
		await srv.close();
	}
});

test("ask: no token is a configuration error", async () => {
	await assert.rejects(
		() =>
			askCommand({
				cfgPath: tmp(),
				prompt: ["hi"],
				model: "",
				thinking: false,
				search: false,
				persist: false,
				noTranscript: false,
				jsonOut: false,
				timeoutMs: 0,
				token: "",
				cookie: "",
				userAgent: "",
			}),
		/no DeepSeek session configured/,
	);
});

test("ask: empty prompt is an error", async () => {
	await assert.rejects(
		() =>
			askCommand({
				cfgPath: tmp(),
				prompt: [],
				model: "",
				thinking: false,
				search: false,
				persist: false,
				noTranscript: false,
				jsonOut: false,
				timeoutMs: 0,
				token: "tok",
				cookie: "",
				userAgent: "",
				readStdin: async () => "",
			}),
		/nothing to ask/,
	);
});
