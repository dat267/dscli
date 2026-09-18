/**
 * `dscli session`: show the persisted default conversation, list/select
 * sessions with saved texts, print or delete a transcript, forget or delete
 * the default. Port of cmd/sessioncmd.go.
 */
import process from "node:process";
import { existsSync, readFileSync, readdirSync, rmSync, statSync } from "node:fs";
import { dirname, join } from "node:path";
import { DeepSeekClient } from "../../core/deepseek/client.js";
import {
	clearSession,
	loadSavedSession,
	saveSession,
	splitConversation,
} from "../../core/session.js";
import { loadTranscript, transcriptPath } from "../../core/transcript.js";
import { stderrNote } from "../../ui/notes.js";

export interface SessionCommandOptions {
	cfgPath: string;
	/** Positional argument (session id for select/transcript). */
	session?: string;
	delete?: boolean;
	/** Credentials for `session delete` (server-side deletion). */
	token: string;
	cookie: string;
	userAgent: string;
	/** Test hook: overrides the API base URL. */
	clientBase?: string;
}

/** One locally known session for `session list` (and the chat /sessions command). */
export interface SessionRow {
	id: string;
	msgs: number;
	last: string;
	mtimeMs: number;
	def: boolean;
}

/** timeString renders a unix/ms timestamp in a compact local form. */
export function timeString(ms: number): string {
	const d = new Date(ms);
	const pad = (n: number) => String(n).padStart(2, "0");
	return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** localSessionRows: sessions with a saved transcript, most recently used first, marking the default. */
export function localSessionRows(cfgPath: string): SessionRow[] {
	const dir = join(dirname(cfgPath), "transcripts");
	let entries: string[];
	try {
		entries = readdirSync(dir);
	} catch {
		return [];
	}
	const { sessionId: saved } = splitConversation(loadSavedSession(cfgPath));
	const rows: SessionRow[] = [];
	for (const name of entries) {
		if (!name.endsWith(".jsonl")) continue;
		const path = join(dir, name);
		let mtimeMs: number;
		try {
			mtimeMs = statSync(path).mtimeMs;
		} catch {
			continue;
		}
		const { msgs, last } = transcriptSummary(path, mtimeMs);
		const id = name.slice(0, -".jsonl".length);
		rows.push({ id, msgs, last, mtimeMs, def: id === saved });
	}
	rows.sort((a, b) => b.mtimeMs - a.mtimeMs);
	return rows;
}

/** sessionRowText renders one session row for display. */
export function sessionRowText(r: SessionRow): string {
	const marker = r.def ? "  (default)" : "";
	return `${r.id.padEnd(24)} ${r.msgs} msgs  last ${r.last}${marker}`;
}

/** transcriptSummary counts a transcript file's messages and the timestamp of the last one. */
function transcriptSummary(path: string, fallbackMtimeMs: number): { msgs: number; last: string } {
	let msgs = 0;
	let last = timeString(fallbackMtimeMs);
	let data: string;
	try {
		data = readFileSync(path, "utf8");
	} catch {
		return { msgs, last };
	}
	for (const line of data.replace(/\n$/, "").split("\n")) {
		if (line.trim() === "") continue;
		msgs++;
		try {
			const e = JSON.parse(line) as { time?: string };
			if (e && typeof e.time === "string" && e.time !== "") last = e.time;
		} catch {
			// corrupted line: keep counting
		}
	}
	return { msgs, last };
}

export async function sessionCommand(
	sub: string | undefined,
	opts: SessionCommandOptions,
): Promise<void> {
	/* eslint-disable no-restricted-syntax */
	switch (sub) {
		case undefined:
			return sessionShow(opts);
		case "list":
			return sessionList(opts);
		case "select":
			return sessionSelect(opts);
		case "transcript":
			return sessionTranscript(opts);
		case "forget":
			return sessionForget(opts);
		case "delete":
			return sessionDelete(opts);
		default:
			throw new Error(`unknown session subcommand ${sub}`);
	}
}

function sessionShow(opts: SessionCommandOptions): void {
	const saved = loadSavedSession(opts.cfgPath);
	if (saved !== "") process.stdout.write(saved + "\n");
	else process.stdout.write("no persisted session\n");
}

function sessionList(opts: SessionCommandOptions): void {
	const rows = localSessionRows(opts.cfgPath);
	if (rows.length === 0) {
		process.stdout.write("no local sessions (nothing saved yet)\n");
		return;
	}
	for (const r of rows) process.stdout.write(sessionRowText(r) + "\n");
}

function sessionSelect(opts: SessionCommandOptions): void {
	const { sessionId: bare } = splitConversation((opts.session ?? "").trim());
	if (bare === "") {
		throw new Error("give a session id (run 'dscli session list' to see the saved ones)");
	}
	const entries = loadTranscript(opts.cfgPath, bare);
	if (entries && entries.length > 0) {
		process.stdout.write(`selected session ${bare} (${entries.length} saved messages)\n`);
	} else {
		process.stdout.write(`selected session ${bare} (no local transcript yet — first chat will create it)\n`);
	}
	const err = saveSession(opts.cfgPath, bare);
	if (err) throw err;
}

function sessionTranscript(opts: SessionCommandOptions): void {
	const sess = opts.session && opts.session !== "" ? opts.session : loadSavedSession(opts.cfgPath);
	const { sessionId: bare } = splitConversation(sess);
	if (bare === "") {
		process.stdout.write(opts.delete ? "no session to delete\n" : "no session to show\n");
		return;
	}
	const p = transcriptPath(opts.cfgPath, bare);
	if (opts.delete) {
		if (!existsSync(p)) {
			process.stdout.write(`no saved texts for session ${bare} (nothing to delete)\n`);
			return;
		}
		rmSync(p);
		// Drop the transcripts folder too when it is now empty.
		try {
			if (readdirSync(dirname(p)).length === 0) rmSync(dirname(p));
		} catch {
			// best-effort
		}
		process.stdout.write(`deleted transcript for session ${bare}\n`);
		return;
	}
	const entries = loadTranscript(opts.cfgPath, bare);
	if (!entries || entries.length === 0) {
		process.stdout.write(`no saved texts for session ${bare} (${p} does not exist)\n`);
		return;
	}
	process.stdout.write(`session ${bare} · ${p}\n`);
	for (let i = 0; i < entries.length; i++) {
		if (i > 0) process.stdout.write("\n");
		const e = entries[i]!;
		process.stdout.write(`${e.time}  ${e.role}\n${e.text}\n`);
	}
}

function sessionForget(opts: SessionCommandOptions): void {
	const saved = loadSavedSession(opts.cfgPath);
	if (saved === "") {
		process.stdout.write("no persisted session to forget\n");
		return;
	}
	const err = clearSession(opts.cfgPath);
	if (err) throw err;
	process.stdout.write(`forgot session ${saved} (thread kept server-side)\n`);
}

async function sessionDelete(opts: SessionCommandOptions): Promise<void> {
	const saved = loadSavedSession(opts.cfgPath);
	if (saved === "") {
		process.stdout.write("no persisted session to delete\n");
		return;
	}
	const { sessionId: sess } = splitConversation(saved);
	if (opts.token === "") {
		stderrNote("warning: no credentials configured; thread not deleted server-side\n");
	} else {
		const client = new DeepSeekClient(
			{ token: opts.token, cookie: opts.cookie, userAgent: opts.userAgent },
			{ base: opts.clientBase },
		);
		try {
			await client.deleteSessions([sess]);
			process.stdout.write(`deleted session ${sess} server-side\n`);
		} catch (err) {
			stderrNote(`warning: server-side delete failed: ${err instanceof Error ? err.message : err}\n`);
		}
	}
	const err = clearSession(opts.cfgPath);
	if (err) throw err;
	process.stdout.write(`forgot session ${saved}\n`);
}
