/**
 * The file commands: translate, improve-writing, summarize. Port of
 * cmd/translate.go, cmd/improve_writing.go, cmd/summarize.go and
 * cmd/runfiles.go — one shared runner drives a per-file pipeline over
 * ephemeral (default) or persisted sessions.
 */
import process from "node:process";
import { existsSync, readFileSync, writeFileSync, statSync } from "node:fs";
import { DeepSeekClient } from "../../core/deepseek/client.js";
import {
	effectiveModel,
	persistConversation,
	recoverStaleSession,
	resolveDefaultSession,
} from "../../core/session.js";
import {
	defaultOutput,
	load,
	resolveImproveStyle,
	resolveStyle,
	resolveSummarizeStyle,
	summarize,
	translate,
	MAX_INPUT_BYTES,
	type Options as TranslateOptions,
} from "../../core/translate/engine.js";
import { stderrNote } from "../../ui/notes.js";

export interface FileCommandOptions {
	cfgPath: string;
	files: string[];
	from: string;
	to: string;
	output: string;
	force: boolean;
	inPlace: boolean;
	chunkBytes: number;
	instructions: string;
	glossary: string;
	timeoutMs: number;
	model: string;
	thinking: boolean;
	parallel: boolean;
	persist: boolean;
	token: string;
	cookie: string;
	userAgent: string;
	/** Test hook: overrides the API base URL. */
	clientBase?: string;
	/** Output stream for stdout printing (defaults to process.stdout). */
	stdout?: NodeJS.WriteStream;
}

/** runFiles: sequential over one client (first error stops) or concurrent with a fresh client per file (first error received). */
export async function runFiles(
	files: string[],
	parallel: boolean,
	newClient: () => DeepSeekClient,
	run: (client: DeepSeekClient, file: string) => Promise<void>,
): Promise<void> {
	if (!parallel) {
		const client = newClient();
		for (const file of files) {
			await run(client, file);
		}
		return;
	}
	const results = await Promise.allSettled(files.map((file) => run(newClient(), file)));
	const firstErr = results.find((r) => r.status === "rejected") as PromiseRejectedResult | undefined;
	if (firstErr) throw firstErr.reason;
}

/** parseDurationMs: Go-style durations ("15m", "90s", "1h30m"); "0" or "" = no limit. */
export function parseDurationMs(s: string): number {
	if (s === "" || s === "0") return 0;
	let total = 0;
	let rest = s;
	const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/y;
	while (rest !== "") {
		const m = re.exec(rest);
		if (!m) throw new Error(`invalid duration ${s}`);
		const n = Number(m[1]!);
		switch (m[2]) {
			case "h": total += n * 3_600_000; break;
			case "m": total += n * 60_000; break;
			case "s": total += n * 1000; break;
			case "ms": total += n; break;
			case "us":
			case "µs": total += n / 1000; break;
			case "ns": total += n / 1e6; break;
		}
		rest = rest.slice(m[0].length);
	}
	return total;
}

export type FileTask = "translate" | "improve" | "summarize";

export async function fileCommand(task: FileTask, opts: FileCommandOptions): Promise<void> {
	if (opts.token === "") {
		throw new Error(
			"no DeepSeek session configured: pass --token/--cookie (or DS_TOKEN/DS_COOKIE) or run 'dscli login' and save the values with 'dscli config set'",
		);
	}
	if (opts.files.length === 0) {
		throw new Error(`give at least one file to ${task === "translate" ? "translate" : task}`);
	}
	if (task === "improve") {
		for (const f of opts.files) {
			if (f.toLowerCase().endsWith(".epub")) {
				throw new Error("improve-writing --in-place does not support epub (Load returns extracted text, which cannot be written back as a binary epub); improve the extracted .txt instead");
			}
		}
	}

	// Instructions: explicit file, else per-task discovery, else built-in.
	let style: string;
	if (task === "translate") style = resolveStyle(opts.instructions, opts.from, opts.to);
	else if (task === "improve") style = resolveImproveStyle(opts.instructions);
	else style = resolveSummarizeStyle(opts.instructions);
	if (task !== "summarize" && opts.glossary !== "" && opts.glossary !== undefined) {
		style = style + "\n\n## Project glossary\n" + readFileSync(opts.glossary, "utf8");
	}

	const newClient = (): DeepSeekClient =>
		new DeepSeekClient(
			{ token: opts.token, cookie: opts.cookie, userAgent: opts.userAgent },
			{ timeoutMs: opts.timeoutMs, base: opts.clientBase },
		);

	await runFiles(opts.files, opts.parallel, newClient, (client, file) =>
		runOne(task, client, file, style, opts),
	);
}

async function runOne(
	task: FileTask,
	client: DeepSeekClient,
	file: string,
	style: string,
	opts: FileCommandOptions,
): Promise<void> {
	const { content, format } = load(file, MAX_INPUT_BYTES);

	let out = "";
	if (task === "translate") {
		out = opts.output !== "" ? opts.output : defaultOutput(file, opts.to);
		if (!opts.force && existsSync(out)) {
			throw new Error(`output ${out} already exists (use -f to overwrite)`);
		}
	} else if (task === "summarize" && opts.output !== "") {
		out = opts.output;
		if (!opts.force && existsSync(out)) {
			throw new Error(`output ${out} already exists (use -f to overwrite)`);
		}
	}

	const { sessionId, trusted, cleanup } = await resolveDefaultSession(client, opts.cfgPath, opts.persist);
	try {
		if (task === "translate") {
			stderrNote(`translating ${file} → ${out} (${format} from ${opts.from} to ${opts.to})\n`);
		} else if (task === "improve") {
			stderrNote(`improving ${file} → ${file} (${format})\n`);
		} else if (out !== "") {
			stderrNote(`summarizing ${file} → ${out} (${format})\n`);
		} else {
			stderrNote(`summarizing ${file} (${format})\n`);
		}

		const engineOpts: TranslateOptions = {
			from: opts.from,
			to: opts.to,
			model: effectiveModel(opts.model),
			chunkBytes: opts.chunkBytes,
			style,
			thinking: opts.thinking,
			task: task === "translate" ? undefined : task,
			onChunk: (chunk, total) => stderrNote(`  chunk ${chunk}/${total} ok\n`),
		};

		let result = "";
		let convId = "";
		const { used } = await recoverStaleSession(client, opts.cfgPath, sessionId, trusted, async (sid) => {
			if (task === "summarize") {
				const r = await summarize(client, sid, content, format, engineOpts);
				result = r.text;
				convId = r.convId;
			} else {
				const r = await translate(client, sid, content, format, engineOpts);
				result = r.text;
				convId = r.convId;
			}
		});
		// The engine's final conversation id already reflects the session the
		// successful run used (possibly the recovery session).
		persistConversation(opts.cfgPath, opts.persist, convId);

		if (task === "summarize" && out === "") {
			(opts.stdout ?? process.stdout).write(result);
			return;
		}
		const target = task === "improve" ? file : out;
		writeFileSync(target, result, { mode: 0o644 });
		stderrNote(`done → ${target} (${Buffer.byteLength(result, "utf8")} bytes)\n`);
	} finally {
		if (cleanup) await cleanup();
	}
}

/** statOrThrow used by tests to confirm outputs. */
export function fileSize(path: string): number {
	return statSync(path).size;
}
