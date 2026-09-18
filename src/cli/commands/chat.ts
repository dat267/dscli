/**
 * `dscli chat`: one-shot mode (prompt given) and the line-based REPL
 * fallback (pipes/scripts). The interactive TUI lands in modes/interactive.
 * Port of cmd/chat.go (ask, oneTurn, replLoop and helpers).
 */
import process from "node:process";
import { createInterface } from "node:readline";
import { DeepSeekClient, type Reply } from "../../core/deepseek/client.js";
import type { Source } from "../../core/deepseek/sse.js";
import {
	conversationID,
	effectiveModel,
	loadSavedSession,
	persistConversation,
	recoverStaleSession,
	resolveDefaultSession,
	saveSession,
	splitConversation,
} from "../../core/session.js";
import { appendTranscript, loadTranscript, transcriptsEnabled } from "../../core/transcript.js";
import { localSessionRows, sessionRowText } from "./session.js";
import { renderSources } from "./ask.js";
import { stderrNote } from "../../ui/notes.js";

export interface ChatOptions {
	cfgPath: string;
	prompt: string[];
	conversation: string;
	model: string;
	thinking: boolean;
	search: boolean;
	jsonOut: boolean;
	timeoutMs: number;
	persist: boolean;
	noTranscript: boolean;
	token: string;
	cookie: string;
	userAgent: string;
	/** Test hooks. */
	clientBase?: string;
	input?: AsyncIterable<string>;
	stdout?: NodeJS.WriteStream;
	interactive?: boolean;
}

/** onoff renders a boolean as "on"/"off". */
export function onoff(v: boolean): string {
	return v ? "on" : "off";
}

/** toggleState: bare "/cmd" flips; "/cmd on|off|1|0|yes|no|true|false" sets. */
export function toggleState(line: string, cmd: string, current: boolean): boolean {
	const arg = line.slice(cmd.length).trim();
	if (arg === "") return !current;
	switch (arg) {
		case "on":
		case "1":
		case "true":
		case "yes":
			return true;
		case "off":
		case "0":
		case "false":
		case "no":
			return false;
	}
	return current;
}

/** hasContinuation: a trailing single backslash continues the line; "\\" is literal. */
export function hasContinuation(s: string): boolean {
	return s.endsWith("\\") && !s.endsWith("\\\\");
}

/** resumePrompt seeds a new generation from the filtered partial text. */
export function resumePrompt(partial: string, instruction: string): string {
	let out =
		"The previous reply was cut off by a content-safety filter. " +
		"Continue the answer from where it stopped, keeping the same language, style and format — do not restate the text above:\n\n" +
		partial +
		"\n";
	if (instruction !== "") out += "\n" + instruction + "\n";
	return out;
}

/** oneTurn asks one question in the given conversation and returns the conversation id for the NEXT turn. */
export async function oneTurn(
	client: DeepSeekClient,
	conversation: string,
	promptText: string,
	model: string,
	thinking: boolean,
	search: boolean,
	write: (delta: string) => void,
): Promise<{ convId: string; filtered: boolean; sources: Source[] }> {
	const { sessionId, parentId } = splitConversation(conversation);
	let sid = sessionId;
	if (sid === "") {
		sid = await client.createChatSession();
	}
	// model_type is only sent (and only meaningful) on the first turn.
	const modelType = parentId === null ? model : undefined;
	const reply: Reply = await client.streamCompletion(
		{
			chatSessionId: sid,
			parentMessageId: parentId,
			prompt: promptText,
			modelType,
			thinkingEnabled: thinking,
			searchEnabled: search,
		},
		write,
	);
	if (reply.filtered) stderrNote("note: reply was filtered by DeepSeek (content policy)\n");
	return { convId: conversationID(sid, parentId, reply.messageId), filtered: reply.filtered, sources: reply.sources };
}

function printReplHelp(): void {
	stderrNote(`commands:
  /exit, /quit                leave the session
  /new                        start a fresh conversation
  /model <default|expert>     switch model (starts a fresh conversation)
  /thinking [on|off]          toggle DeepThink reasoning
  /search [on|off]            toggle web search
  /resume [instruction]       continue a reply the filter cut off, from its partial text
  /session [id]               show the current conversation; select a saved session to resume
  /sessions                   list sessions with saved texts
  /copy                       copy the chat text to the system clipboard (TUI only)
  /help                       this help

multiline: end a line with \\ to continue it on the next line; a lone \\ line
inserts a blank line and keeps going. A trailing \\\\ (two backslashes) does not
continue — the line is sent literally.
`);
}

async function* linesOf(input: AsyncIterable<string>): AsyncGenerator<string> {
	for await (const line of input) {
		yield line.replace(/\n$/, "");
	}
}

/** stdinLines: line iterator over the real stdin (REPL mode). */
function stdinLines(): AsyncIterable<string> {
	const rl = createInterface({ input: process.stdin });
	const gen = async function* () {
		for await (const line of rl) yield line;
	};
	return gen();
}

export async function chatCommand(opts: ChatOptions): Promise<void> {
	if (opts.token === "") {
		throw new Error(
			"no DeepSeek session configured: pass --token/--cookie (or DS_TOKEN/DS_COOKIE) or run 'dscli login' and save the values with 'dscli config set'",
		);
	}
	const client = new DeepSeekClient(
		{ token: opts.token, cookie: opts.cookie, userAgent: opts.userAgent },
		{ timeoutMs: opts.timeoutMs, base: opts.clientBase },
	);
	const promptText = opts.prompt.join(" ").trim();
	if (promptText === "") {
		await replLoop(opts, client);
		return;
	}
	await chatOneShot(opts, client, promptText);
}

/** chatOneShot: one question, one answer, done. */
async function chatOneShot(opts: ChatOptions, client: DeepSeekClient, promptText: string): Promise<void> {
	let conversation = opts.conversation;
	let trusted = false;
	let cleanup: (() => Promise<void>) | undefined;
	if (conversation === "") {
		const resolved = await resolveDefaultSession(client, opts.cfgPath, opts.persist);
		conversation = resolved.sessionId;
		trusted = resolved.trusted;
		cleanup = resolved.cleanup;
	}
	try {
		const transcriptsOn = transcriptsEnabled(opts.cfgPath, opts.persist, opts.noTranscript);
		if (transcriptsOn) appendTranscript(opts.cfgPath, conversation, "user", promptText);

		const out = opts.stdout ?? process.stdout;
		let replyBuf = "";
		let sources: Source[] = [];
		let convId = "";
		await recoverStaleSession(client, opts.cfgPath, conversation, trusted, async (sid) => {
			const r = await oneTurn(client, sid, promptText, effectiveModel(opts.model), opts.thinking, opts.search, (delta) => {
				replyBuf += delta;
				if (opts.jsonOut) out.write(JSON.stringify({ delta }) + "\n");
				else out.write(delta);
			});
			convId = r.convId;
			sources = r.sources;
		});
		if (transcriptsOn) appendTranscript(opts.cfgPath, convId, "assistant", replyBuf);
		persistConversation(opts.cfgPath, opts.persist, convId);
		if (opts.jsonOut) {
			const done: Record<string, unknown> = { done: true, conversation_id: convId };
			if (sources.length > 0) done["sources"] = sources;
			out.write(JSON.stringify(done) + "\n");
			return;
		}
		stderrNote(`\nconversation: ${convId}\n`);
		renderSources(sources);
	} finally {
		if (cleanup) await cleanup();
	}
}

/** replLoop: the interactive multi-turn line session. */
export async function replLoop(opts: ChatOptions, client: DeepSeekClient): Promise<void> {
	let model = effectiveModel(opts.model);
	let thinking = opts.thinking;
	let search = opts.search;
	let conversation = opts.conversation;

	// The persisted default session is recovered once if its first turn fails;
	// fresh and ephemeral sessions never are.
	let firstTurn = false;
	const persist = opts.persist && opts.cfgPath !== "";
	const out = opts.stdout ?? process.stdout;
	const owned: string[] = [];
	if (conversation === "") {
		const resolved = await resolveDefaultSession(client, opts.cfgPath, opts.persist);
		conversation = resolved.sessionId;
		firstTurn = resolved.trusted;
		if (resolved.cleanup && !persist) {
			// The REPL owns the ephemeral session: delete it when the loop ends.
			owned.push(conversation);
		}
	}
	const deleteOwned = async (): Promise<void> => {
		if (owned.length === 0) return;
		try {
			await client.deleteSessions(owned);
		} catch (err) {
			stderrNote(`warning: failed to delete session(s): ${err instanceof Error ? err.message : err}\n`);
		}
	};

	const status = (): void => {
		const mode = opts.conversation !== "" ? "continuing" : persist ? "persisted" : "ephemeral";
		stderrNote(
			`DeepSeek · model ${model} · thinking ${onoff(thinking)} · search ${onoff(search)} · ${mode}\n`,
		);
	};
	status();
	stderrNote("one question per line · /help for commands\n");

	const transcriptsOn = transcriptsEnabled(opts.cfgPath, opts.persist, opts.noTranscript);
	const input = opts.input ?? stdinLines();
	let turns = 0;
	// lastPartial keeps the most recent filtered reply so /resume can continue it.
	let lastPartial = "";
	let line = "";
	let nextLine: string | undefined;
	const iter = linesOf(input)[Symbol.asyncIterator]();
	const scan = async (): Promise<string | undefined> => {
		if (nextLine !== undefined) {
			const l = nextLine;
			nextLine = undefined;
			return l;
		}
		const r = await iter.next();
		return r.done ? undefined : r.value;
	};

	for (;;) {
		const raw = await scan();
		if (raw === undefined) break;
		line = raw.trim();
		switch (true) {
			case line === "":
				continue;
			case line === "/exit" || line === "/quit":
				if (turns > 0) stderrNote(`conversation: ${conversation}\n`);
				await deleteOwned();
				return;
			case line === "/new":
				conversation = "";
				stderrNote("new conversation\n");
				continue;
			case line === "/help":
				printReplHelp();
				stderrNote("\n");
				continue;
			case line === "/model" || line.startsWith("/model "): {
				const m = line.slice("/model".length).trim();
				if (m === "") {
					status();
					stderrNote(`model: ${model} (fixed per thread; /model <default|expert> starts a new conversation)\n`);
					continue;
				}
				if (m !== "default" && m !== "expert") {
					stderrNote(`unknown model "${m}" (want default or expert)\n`);
					continue;
				}
				model = m;
				conversation = "";
				status();
				stderrNote("new conversation\n");
				continue;
			}
			case line === "/thinking" || line.startsWith("/thinking "):
				thinking = toggleState(line, "/thinking", thinking);
				status();
				continue;
			case line === "/search" || line.startsWith("/search "):
				search = toggleState(line, "/search", search);
				status();
				continue;
			case line === "/resume" || line.startsWith("/resume "): {
				if (lastPartial === "") {
					stderrNote("nothing to resume: no filtered partial (or the last reply was accepted)\n");
					continue;
				}
				const instruction = line.slice("/resume".length).trim();
				line = resumePrompt(lastPartial, instruction);
				stderrNote("resuming from the filtered partial reply\n");
				break;
			}
			case line === "/sessions": {
				const rows = localSessionRows(opts.cfgPath);
				if (rows.length === 0) {
					stderrNote("no local sessions (nothing saved yet)\n");
					continue;
				}
				stderrNote("local sessions (most recent first; the default is resumed on launch):\n");
				for (const r of rows) stderrNote(sessionRowText(r) + "\n");
				stderrNote("\n");
				continue;
			}
			case line === "/session" || line.startsWith("/session "): {
				const arg = line.slice("/session".length).trim();
				if (arg === "") {
					const saved = loadSavedSession(opts.cfgPath);
					stderrNote((saved !== "" ? "conversation: " + saved : "no persisted session") + "\n\n");
					continue;
				}
				const { sessionId: bare } = splitConversation(arg);
				if (bare === "") {
					stderrNote("give a session id (see /sessions)\n");
					continue;
				}
				const err = saveSession(opts.cfgPath, bare);
				if (err) {
					stderrNote(`error: ${err.message}\n`);
					continue;
				}
				const entries = loadTranscript(opts.cfgPath, bare);
				if (entries && entries.length > 0) {
					stderrNote(`switched to session ${bare} (${entries.length} saved messages; resumes from its root)\n\n`);
				} else {
					stderrNote(`switched to session ${bare} (no local transcript yet)\n\n`);
				}
				conversation = bare;
				lastPartial = "";
				continue;
			}
			case line.startsWith("/"):
				stderrNote("unknown command (/help for commands)\n");
				continue;
		}

		// Multi-line prompt: a trailing single backslash continues.
		while (hasContinuation(line)) {
			line = line.slice(0, -1);
			const cont = await scan();
			if (cont === undefined) break;
			line += "\n" + cont.trim();
		}

		// A reset (/new, /model) leaves conversation empty: spawn a fresh
		// session. Persisted runs save it; ephemeral runs track it for deletion.
		if (conversation === "") {
			try {
				conversation = await client.createChatSession();
			} catch (err) {
				stderrNote(`error: create chat session: ${err instanceof Error ? err.message : err}\n`);
				continue;
			}
			if (persist) {
				const err = saveSession(opts.cfgPath, conversation);
				if (err) stderrNote(`warning: could not save session: ${err.message}\n`);
			} else {
				owned.push(conversation);
			}
		}

		let last = "";
		let sources: Source[] = [];
		let convId = "";
		let replyBuf = "";
		const turnSession = conversation;
		if (transcriptsOn) appendTranscript(opts.cfgPath, turnSession, "user", line);
		let filtered = false;
		const r = await recoverStaleSession(client, opts.cfgPath, conversation, firstTurn, async (sid) => {
			const t = await oneTurn(client, sid, line, model, thinking, search, (delta) => {
				if (delta.length > 0) last = delta[delta.length - 1]!;
				replyBuf += delta;
				out.write(delta);
			});
			convId = t.convId;
			filtered = t.filtered;
			sources = t.sources;
		});
		firstTurn = false;
		if (r.err) {
			if (last !== "\n" && last !== "") out.write("\n");
			stderrNote(`error: ${r.err.message}\n`);
			if (last !== "\n") out.write("\n");
			continue;
		}
		if (transcriptsOn) appendTranscript(opts.cfgPath, turnSession, "assistant", replyBuf);
		if (filtered) {
			if (replyBuf.length > 0) {
				lastPartial = replyBuf;
				stderrNote("hint: /resume continues from the partial reply (kept as context)\n");
			} else {
				lastPartial = "";
			}
		} else {
			lastPartial = "";
		}
		if (last !== "\n") out.write("\n");
		out.write("\n"); // blank line before the next prompt
		renderSources(sources);
		conversation = convId;
		persistConversation(opts.cfgPath, opts.persist, conversation);
		turns++;
	}
	await deleteOwned();
	if (turns > 0) stderrNote(`conversation: ${conversation}\n`);
}
