/**
 * Session transcripts: one JSONL file per chat session in the transcripts
 * folder next to the config file, saved only for explicitly persisted runs.
 * Port of cmd/transcript.go.
 */
import { appendFileSync, existsSync, mkdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join } from "node:path";
import { stderrNote } from "../ui/notes.js";
import { splitConversation } from "./session.js";

const TRANSCRIPT_DIR_NAME = "transcripts";

/** One saved message in a session transcript. */
export interface TranscriptEntry {
	time: string;
	role: string;
	text: string;
}

/** transcriptsEnabled: only explicitly persisted runs with a config dir, not opted out via --no-transcript. */
export function transcriptsEnabled(cfgPath: string, persist: boolean, noTranscript: boolean): boolean {
	return cfgPath !== "" && persist && !noTranscript;
}

/**
 * transcriptPath: the transcripts/<session>.jsonl file next to the config
 * file; "" when there is no config file or no session id.
 */
export function transcriptPath(cfgPath: string, sessionId: string): string {
	if (cfgPath === "") return "";
	const { sessionId: sess } = splitConversation(sessionId);
	if (sess === "") return "";
	return join(dirname(cfgPath), TRANSCRIPT_DIR_NAME, `${sess}.jsonl`);
}

/** loadTranscript returns a session's saved messages, skipping malformed lines; null when there is no file. */
export function loadTranscript(cfgPath: string, sessionId: string): TranscriptEntry[] | null {
	const p = transcriptPath(cfgPath, sessionId);
	if (p === "" || !existsSync(p)) return null;
	const data = readFileSync(p, "utf8");
	const out: TranscriptEntry[] = [];
	for (const line of data.replace(/\n$/, "").split("\n")) {
		if (line.trim() === "") continue;
		try {
			const e = JSON.parse(line) as TranscriptEntry;
			// keep the file readable even if a line was corrupted
			if (e && typeof e.role === "string") out.push(e);
		} catch {
			continue;
		}
	}
	return out;
}

/**
 * appendTranscript appends one message to a session's transcript file,
 * creating the folder and file on first use. Failures are soft warnings — a
 * transcript problem never breaks the chat itself.
 */
export function appendTranscript(cfgPath: string, sessionId: string, role: string, text: string): void {
	if (text === "") return;
	const p = transcriptPath(cfgPath, sessionId);
	if (p === "") return;
	const entry = JSON.stringify({ time: new Date().toISOString(), role, text });
	try {
		mkdirSync(dirname(p), { recursive: true, mode: 0o700 });
		if (!existsSync(p)) {
			// 0600 on create (appendFileSync mode only applies at creation).
			appendFileSync(p, entry + "\n", { mode: 0o600 });
		} else {
			appendFileSync(p, entry + "\n");
		}
	} catch (err) {
		stderrNote(`warning: could not save transcript: ${err instanceof Error ? err.message : err}\n`);
	}
}

/** transcriptMtime: the file's modification time in ms, or 0 when absent. */
export function transcriptMtime(cfgPath: string, sessionId: string): number {
	const p = transcriptPath(cfgPath, sessionId);
	if (p === "") return 0;
	try {
		return statSync(p).mtimeMs;
	} catch {
		return 0;
	}
}
