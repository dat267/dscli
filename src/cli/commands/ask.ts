/**
 * `dscli ask`: a pure one-shot call — send the model an input, print the
 * answer, done. Port of cmd/ask.go. The session is ephemeral by default
 * (created per call, deleted afterwards); --persist opts into the saved
 * default conversation and transcripts.
 */
import process from "node:process";
import { DeepSeekClient, type Reply } from "../../core/deepseek/client.js";
import type { Source } from "../../core/deepseek/sse.js";
import {
	advanceConversation,
	effectiveModel,
	persistConversation,
	recoverStaleSession,
	resolveDefaultSession,
	splitConversation,
} from "../../core/session.js";
import { appendTranscript, transcriptsEnabled } from "../../core/transcript.js";
import { stderrNote } from "../../ui/notes.js";

export interface AskOptions {
	cfgPath: string;
	prompt: string[];
	model: string;
	thinking: boolean;
	search: boolean;
	persist: boolean;
	noTranscript: boolean;
	jsonOut: boolean;
	timeoutMs: number;
	token: string;
	cookie: string;
	userAgent: string;
	/** Test hook: overrides the API base URL. */
	clientBase?: string;
	/** Test hook: replaces the stdin reader (avoids hanging on an open pipe). */
	readStdin?: () => Promise<string>;
	/** Output stream (defaults to process.stdout; tests capture here). */
	stdout?: NodeJS.WriteStream;
}

/** renderSources prints citation footnotes (matching inline [citation:N] markers), numbered 1..N. */
export function renderSources(sources: Source[], out: NodeJS.WriteStream = process.stderr): void {
	if (sources.length === 0) return;
	out.write("\n");
	out.write("Sources:\n");
	for (let i = 0; i < sources.length; i++) {
		const s = sources[i]!;
		if (s.title !== "") out.write(`  [${i + 1}] ${s.title} — ${s.url}\n`);
		else out.write(`  [${i + 1}] ${s.url}\n`);
	}
}

export async function askCommand(opts: AskOptions): Promise<void> {
	if (opts.token === "") {
		throw new Error(
			"no DeepSeek session configured: pass --token/--cookie (or DS_TOKEN/DS_COOKIE) or run 'dscli login' and save the values with 'dscli config set'",
		);
	}
	if (opts.model !== "" && opts.model !== "default" && opts.model !== "expert") {
		throw new Error(`unknown model ${opts.model} (want default or expert)`);
	}

	let prompt = opts.prompt.join(" ").trim();
	if (prompt === "") {
		prompt = (await (opts.readStdin ?? readStdin)()).trim();
	}
	if (prompt === "") {
		throw new Error("nothing to ask: pass a prompt or pipe input on stdin");
	}

	const client = new DeepSeekClient(
		{ token: opts.token, cookie: opts.cookie, userAgent: opts.userAgent },
		{ timeoutMs: opts.timeoutMs, base: opts.clientBase },
	);

	const { sessionId, trusted, cleanup } = await resolveDefaultSession(client, opts.cfgPath, opts.persist);
	try {
		const out = opts.stdout ?? process.stdout;
		let last = "";
		let replyBuf = "";
		const write = (delta: string): void => {
			if (delta.length > 0) last = delta[delta.length - 1]!;
			replyBuf += delta;
			if (opts.jsonOut) out.write(JSON.stringify({ delta }) + "\n");
			else out.write(delta);
		};

		if (transcriptsEnabled(opts.cfgPath, opts.persist, opts.noTranscript)) {
			appendTranscript(opts.cfgPath, sessionId, "user", prompt);
		}
		let reply: Reply | undefined;
		const { used } = await recoverStaleSession(client, opts.cfgPath, sessionId, trusted, async (sid) => {
			const { sessionId: sess, parentId } = splitConversation(sid);
			const r = await client.streamCompletion(
				{
					chatSessionId: sess,
					parentMessageId: parentId,
					prompt,
					modelType: effectiveModel(opts.model),
					thinkingEnabled: opts.thinking,
					searchEnabled: opts.search,
				},
				write,
			);
			reply = r;
			if (reply.filtered) stderrNote("note: reply was filtered by DeepSeek (content policy)\n");
		});
		if (transcriptsEnabled(opts.cfgPath, opts.persist, opts.noTranscript)) {
			appendTranscript(opts.cfgPath, used, "assistant", replyBuf);
		}
		persistConversation(opts.cfgPath, opts.persist, advanceConversation(used, reply?.messageId ?? 0));
		if (opts.jsonOut) {
			if (reply && reply.sources.length > 0) {
				out.write(JSON.stringify({ sources: reply.sources }) + "\n");
			}
			return;
		}
		if (last !== "" && last !== "\n") out.write("\n");
		if (reply) renderSources(reply.sources, process.stderr);
	} finally {
		if (cleanup) await cleanup();
	}
}

/** readStdin drains piped stdin (returns "" for a TTY). */
async function readStdin(): Promise<string> {
	if (process.stdin.isTTY) return "";
	const chunks: Buffer[] = [];
	process.stdin.setEncoding("utf8");
	for await (const chunk of process.stdin) {
		chunks.push(Buffer.from(chunk as string, "utf8"));
	}
	return Buffer.concat(chunks).toString("utf8");
}
