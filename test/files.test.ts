import { strict as assert } from "node:assert";
import { test } from "node:test";
import * as http from "node:http";
import { join } from "node:path";
import { existsSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { fileCommand } from "../src/cli/commands/files.js";

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

async function fakeServer(reply: string): Promise<Fake> {
	const bodies: string[] = [];
	const deleted: string[] = [];
	let created = 0;
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

const base = (cfgPath: string, url: string) => ({
	cfgPath,
	files: [],
	from: "auto",
	to: "Japanese",
	output: "",
	force: false,
	inPlace: false,
	chunkBytes: 0,
	instructions: "",
	glossary: "",
	timeoutMs: 0,
	model: "",
	thinking: false,
	parallel: false,
	persist: false,
	token: "tok",
	cookie: "",
	userAgent: "",
	clientBase: url,
});

test("translate writes <base>.translated.<lang>.<ext> and deletes the ephemeral session", async () => {
	const srv = await fakeServer(frames(2, "\\u3053\\u3093\\u306b\\u3061\\u306f\\n")); // こんにちは
	try {
		const dir = tmpDir();
		const input = join(dir, "chapter.md");
		writeFileSync(input, "Hello\n");
		const cfg = tmpCfg();
		await fileCommand("translate", { ...base(cfg, srv.url), files: [input] });
		const out = join(dir, "chapter.translated.ja.md");
		assert.ok(existsSync(out), "default output path used");
		assert.equal(readFileSync(out, "utf8"), "こんにちは\n");
		assert.equal(srv.created, 1);
		assert.equal(srv.deleted.length, 1, "ephemeral session deleted");
	} finally {
		await srv.close();
	}
});

test("translate refuses to overwrite without -f, proceeds with it", async () => {
	const srv = await fakeServer(frames(2, "x\\n"));
	try {
		const dir = tmpDir();
		const input = join(dir, "a.txt");
		writeFileSync(input, "Hello\n");
		const cfg = tmpCfg();
		// Pre-create the default output to trigger the refusal.
		const out = join(dir, "a.translated.en.txt");
		writeFileSync(out, "existing\n");
		await assert.rejects(
			() => fileCommand("translate", { ...base(cfg, srv.url), files: [input], to: "en" }),
			/already exists \(use -f to overwrite\)/,
		);
		assert.equal(readFileSync(out, "utf8"), "existing\n", "refusal left the output untouched");
		// -f overwrites.
		await fileCommand("translate", { ...base(cfg, srv.url), files: [input], to: "en", force: true });
		assert.equal(readFileSync(out, "utf8"), "x\n");
	} finally {
		await srv.close();
	}
});

test("summarize prints to stdout by default", async () => {
	const srv = await fakeServer(frames(2, "The gist.\\n"));
	try {
		const dir = tmpDir();
		const input = join(dir, "doc.md");
		writeFileSync(input, "Long document text.\n");
		const cfg = tmpCfg();
		const chunks: string[] = [];
		const cap = {
			write: (c: string | Uint8Array) => {
				chunks.push(typeof c === "string" ? c : Buffer.from(c).toString("utf8"));
				return true;
			},
		} as unknown as NodeJS.WriteStream;
		await fileCommand("summarize", { ...base(cfg, srv.url), to: "", files: [input], stdout: cap });
		assert.equal(chunks.join(""), "The gist.\n");
		assert.ok(!existsSync(join(dir, "doc.translated.en.md")));
	} finally {
		await srv.close();
	}
});

test("improve rewrites the file in place and rejects epub", async () => {
	const srv = await fakeServer(frames(2, "Better prose.\\n"));
	try {
		const dir = tmpDir();
		const input = join(dir, "note.md");
		writeFileSync(input, "bad prose\n");
		const cfg = tmpCfg();
		await fileCommand("improve", { ...base(cfg, srv.url), to: "", files: [input] });
		assert.equal(readFileSync(input, "utf8"), "Better prose.\n");

		const epub = join(dir, "book.epub");
		writeFileSync(epub, "binary");
		await assert.rejects(
			() => fileCommand("improve", { ...base(cfg, srv.url), to: "", files: [epub] }),
			/does not support epub/,
		);
	} finally {
		await srv.close();
	}
});

test("file commands without a token fail with the configuration hint", async () => {
	await assert.rejects(
		() => fileCommand("translate", { ...base(tmpCfg(), "http://127.0.0.1:1"), token: "", files: ["x"] }),
		/no DeepSeek session configured/,
	);
});
