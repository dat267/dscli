import { strict as assert } from "node:assert";
import { test } from "node:test";
import * as http from "node:http";
import { join } from "node:path";
import { mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { chatCommand, hasContinuation, onoff, resumePrompt, toggleState } from "../src/cli/commands/chat.js";
import { loadSavedSession } from "../src/core/session.js";
import { loadTranscript } from "../src/core/transcript.js";

const tmpDir = () => mkdtempSync(join(tmpdir(), "dscli-"));
const tmpCfg = () => join(tmpDir(), "dscli.json");

const frames = (id: number, content: string): string =>
	[
		`{"v":{"response":{"fragments":[{"type":"response","content":""}],"message_id":${id}},"message_id":${id}}}`,
		`{"p":"response/fragments/-1/content","o":"APPEND","v":"${content}"}`,
		`{"v":[{"p":"status","v":"FINISHED"},{"p":"quasi_status","v":"FINISHED"}]}`,
	].join("\n");

interface Fake {
	url: string;
	close(): Promise<void>;
	bodies: string[];
	created: number;
	deleted: string[];
}

async function fakeServer(replies: string[]): Promise<Fake> {
	const bodies: string[] = [];
	const deleted: string[] = [];
	let created = 0;
	let n = 0;
	const srv = http.createServer((req, res) => {
		const chunks: Buffer[] = [];
		req.on("data", (c) => chunks.push(c as Buffer));
		req.on("end", () => {
			if (req.url === "/api/v0/chat_session/create") {
				created++;
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
				bodies.push(Buffer.concat(chunks).toString("utf8"));
				const reply = replies[Math.min(n, replies.length - 1)]!;
				n++;
				res.writeHead(200, { "content-type": "text/event-stream" });
				for (const line of reply.split("\n")) res.write(`data: ${line}\n`);
				res.write("\n");
				res.end();
				return;
			}
			if (req.url === "/api/v0/chat_session/delete") {
				deleted.push(Buffer.concat(chunks).toString("utf8"));
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

function captureStream(): { stream: NodeJS.WriteStream; text: () => string } {
	const chunks: string[] = [];
	return {
		stream: {
			write: (c: string | Uint8Array) => {
				chunks.push(typeof c === "string" ? c : Buffer.from(c).toString("utf8"));
				return true;
			},
		} as unknown as NodeJS.WriteStream,
		text: () => chunks.join(""),
	};
}

const base = (cfgPath: string, url: string) => ({
	cfgPath,
	prompt: [] as string[],
	conversation: "",
	model: "",
	thinking: false,
	search: false,
	jsonOut: false,
	timeoutMs: 0,
	persist: false,
	noTranscript: false,
	token: "tok",
	cookie: "",
	userAgent: "",
	clientBase: url,
});

async function* feed(lines: string[]): AsyncIterable<string> {
	for (const l of lines) yield l + "\n";
}

test("chat helpers", () => {
	assert.equal(onoff(true), "on");
	assert.equal(onoff(false), "off");
	assert.equal(toggleState("/search", "/search", true), false);
	assert.equal(toggleState("/search on", "/search", false), true);
	assert.equal(toggleState("/search bogus", "/search", true), true);
	assert.equal(hasContinuation("hello \\"), true);
	assert.equal(hasContinuation("hello \\\\"), false);
	assert.match(resumePrompt("partial text", "keep it short"), /do not restate/);
	assert.match(resumePrompt("partial text", ""), /partial text/);
});

test("chat one-shot prints the answer and the conversation id", async () => {
	const srv = await fakeServer([frames(2, "Hi there")]);
	try {
		const cap = captureStream();
		await chatCommand({ ...base(tmpCfg(), srv.url), prompt: ["hello"], stdout: cap.stream });
		assert.ok(cap.text().startsWith("Hi there"), cap.text());
	} finally {
		await srv.close();
	}
});

test("chat one-shot --json-out ends with a done line", async () => {
	const srv = await fakeServer([frames(2, "Hi")]);
	try {
		const cap = captureStream();
		await chatCommand({ ...base(tmpCfg(), srv.url), prompt: ["hello"], jsonOut: true, stdout: cap.stream });
		const lines = cap.text().trim().split("\n");
		const done = JSON.parse(lines[lines.length - 1]!) as Record<string, unknown>;
		assert.equal(done["done"], true);
		assert.equal(done["conversation_id"], "sess-1:2");
	} finally {
		await srv.close();
	}
});

test("repl: two turns keep the thread, /exit prints the conversation", async () => {
	const srv = await fakeServer([frames(2, "one"), frames(4, "two")]);
	try {
		const cfg = tmpCfg();
		const cap = captureStream();
		await chatCommand({
			...base(cfg, srv.url),
			input: feed(["first", "second", "/exit"]),
			stdout: cap.stream,
		});
		assert.equal(srv.created, 1, "one session for the whole repl");
		assert.equal(srv.deleted.length, 1, "ephemeral session deleted at exit");
		assert.equal(srv.bodies.length, 2);
		const second = JSON.parse(srv.bodies[1]!) as Record<string, unknown>;
		assert.equal(second["chat_session_id"], "sess-1");
		assert.equal(second["parent_message_id"], 2, "turn 2 resumes from turn 1's message");
		assert.ok(cap.text().includes("one") && cap.text().includes("two"), cap.text());
	} finally {
		await srv.close();
	}
});

test("repl: /new spawns a fresh session; /model switch too", async () => {
	const srv = await fakeServer([frames(2, "a"), frames(2, "b"), frames(2, "c")]);
	try {
		const cfg = tmpCfg();
		const cap2 = captureStream();
		await chatCommand({
			...base(cfg, srv.url),
			input: feed(["one", "/new", "two", "/model expert", "three", "/exit"]),
			stdout: cap2.stream,
		});
		assert.equal(srv.created, 3, "one session per /new or /model reset");
		// Deletion is one batched request carrying every owned session.
		assert.equal(srv.deleted.length, 1);
		const batch = JSON.parse(srv.deleted[0]!) as { chat_session_ids?: string[] };
		assert.equal(batch["chat_session_ids"]?.length, 3);
		const third = JSON.parse(srv.bodies[2]!) as Record<string, unknown>;
		assert.equal(third["model_type"], "expert", "the reset turn sends the new model");
	} finally {
		await srv.close();
	}
});

test("repl: persisted mode saves position and transcripts; /sessions and /session work", async () => {
	const srv = await fakeServer([frames(2, "reply text")]);
	try {
		const cfg = tmpCfg();
		await chatCommand({
			...base(cfg, srv.url),
			persist: true,
			input: feed(["hello", "/sessions", "/session", "/exit"]),
			stdout: captureStream().stream,
		});
		assert.equal(loadSavedSession(cfg), "sess-1:2");
		const entries = loadTranscript(cfg, "sess-1")!;
		assert.equal(entries.length, 2);
		assert.equal(entries[1]!.text, "reply text");
		assert.equal(srv.deleted.length, 0, "persisted sessions are not deleted");
	} finally {
		await srv.close();
	}
});

test("repl: multiline continuation joins lines into one message", async () => {
	const srv = await fakeServer([frames(2, "ok")]);
	try {
		await chatCommand({
			...base(tmpCfg(), srv.url),
			input: feed(["first line \\", "second line", "/exit"]),
			stdout: captureStream().stream,
		});
		assert.equal(srv.bodies.length, 1);
		const sent = JSON.parse(srv.bodies[0]!) as Record<string, unknown>;
		assert.equal(sent["prompt"], "first line \nsecond line");
	} finally {
		await srv.close();
	}
});
