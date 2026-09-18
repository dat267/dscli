/**
 * The chunked, format-aware translation engine behind `dscli translate`,
 * `improve-writing` and `summarize`. Port of internal/translate/translate.go
 * (chunking, prompts, per-chunk verification and the adaptive sizer loop).
 */
import { readFileSync, existsSync } from "node:fs";
import { extname, join } from "node:path";
import { homedir } from "node:os";
import { DeepSeekClient } from "../deepseek/client.js";
import { detectFormat, isText, readEpub, verifyProtected } from "../filetools/index.js";
import { newSizer, DEFAULT_CHUNK_BYTES, type ChunkSizer } from "./sizer.js";

/** Caps a full translate job on the CLI (64 MiB). */
export const MAX_INPUT_BYTES = 64 << 20;

/** Task values for Options.task, selecting what the model does with each chunk. */
export const TASK_TRANSLATE = "";
export const TASK_IMPROVE = "improve";
export const TASK_SUMMARIZE = "summarize";

export interface Options {
	from?: string;
	to?: string;
	model?: string;
	chunkBytes?: number;
	/** Custom per-pair translation instructions appended to every chunk prompt. */
	style?: string;
	/** DeepThink reasoning for each chunk. */
	thinking?: boolean;
	/** Translate (default), improve, or summarize. */
	task?: string;
	/** Progress: chunks done and a live estimate of the total. */
	onChunk?: (chunk: number, total: number) => void;
}

/** Load reads and classifies an input file: plain text or EPUB text extraction. */
export function load(path: string, maxBytes: number): { content: string; format: string } {
	const data = readFileSync(path);
	if (data.length > maxBytes) {
		throw new Error(`input is ${data.length} bytes, over the ${maxBytes} byte translate limit`);
	}
	if (extname(path).toLowerCase() === ".epub") {
		return { content: readEpub(path), format: "text" };
	}
	if (!isText(data)) {
		throw new Error("input is not a text file; translate supports txt/md/lrc/srt/vtt/ass/ttml (and epub)");
	}
	return { content: data.toString("utf8"), format: detectFormat(path, data) };
}

const TRANSLATED_SUFFIX_RE = /\.translated(\.[a-z0-9]{1,8})?$/;

const LANG_CODES: Record<string, string> = {
	english: "en", japanese: "ja", chinese: "zh", korean: "ko",
	spanish: "es", french: "fr", german: "de", italian: "it",
	portuguese: "pt", russian: "ru", arabic: "ar", hindi: "hi",
	thai: "th", vietnamese: "vi", indonesian: "id", dutch: "nl",
	polish: "pl", turkish: "tr", ukrainian: "uk", romanian: "ro",
};

/** pairKey normalises a language label: lowercase, letters and digits only. */
export function pairKey(s: string): string {
	return s.toLowerCase().replace(/[^a-z0-9]/g, "");
}

/** LangCode: ISO 639-1 when known, else the lowercased alphanumeric label. */
export function langCode(label: string): string {
	const key = pairKey(label);
	return LANG_CODES[key] ?? key;
}

/**
 * DefaultOutput: <base>.translated.<lang>.<ext> (i18n name-coding), EPUBs
 * to .txt. An existing ".translated[.<lang>]" suffix is stripped first, so
 * re-translating a translation never stacks suffixes.
 */
export function defaultOutput(input: string, toLabel: string): string {
	let ext = extname(input);
	const base = input.slice(0, input.length - ext.length).replace(TRANSLATED_SUFFIX_RE, "");
	if (ext.toLowerCase() === ".epub") ext = ".txt";
	const code = langCode(toLabel);
	return code === "" ? `${base}.translated${ext}` : `${base}.translated.${code}${ext}`;
}

/**
 * FirstChunk splits off the first chunk of text (roughly approxBytes),
 * keeping lines intact and preserving the source bytes exactly. A single
 * line longer than approxBytes is hard-split. Returns "" only when text is
 * empty.
 */
export function firstChunk(text: string, approxBytes: number): string {
	if (approxBytes <= 0) approxBytes = DEFAULT_CHUNK_BYTES;
	if (approxBytes < 1) approxBytes = 1;
	const endsWithNewline = text.endsWith("\n");
	const lines = text.split("\n");
	let b = "";
	let curBytes = 0;
	for (let i = 0; i < lines.length; i++) {
		let l = lines[i]!;
		if (i < lines.length - 1 || endsWithNewline) l += "\n";
		const bytes = Buffer.byteLength(l, "utf8");
		if (bytes > approxBytes) {
			// Hard-split an over-long line at a code-point boundary.
			if (curBytes > 0) return b;
			let out = "";
			let outBytes = 0;
			for (const ch of l) {
				const cb = Buffer.byteLength(ch, "utf8");
				if (outBytes + cb > approxBytes) break;
				out += ch;
				outBytes += cb;
			}
			return b + out;
		}
		if (curBytes > 0 && curBytes + bytes > approxBytes) return b;
		b += l;
		curBytes += bytes;
		if (curBytes >= approxBytes) return b;
	}
	return curBytes > 0 ? b : "";
}

/** ChunkText: the full split; a file with no content yields one empty chunk. */
export function chunkText(text: string, approxBytes: number): string[] {
	const chunks: string[] = [];
	let rest = text;
	for (;;) {
		const c = firstChunk(rest, approxBytes);
		if (c === "") break;
		chunks.push(c);
		rest = rest.slice(Buffer.byteLength(c, "utf8"));
		// Careful: slicing by byte length can split a multi-byte char if the
		// chunker ever cut mid-rune; firstChunk never does.
	}
	if (chunks.length === 0) chunks.push("");
	return chunks;
}

// --- Prompts ---------------------------------------------------------------

/** formatName: the human name of a format for the prompts. */
export function formatName(format: string): string {
	switch (format) {
		case "lrc": return "LRC lyrics";
		case "srt": return "SRT subtitles";
		case "vtt": return "WebVTT subtitles";
		case "ass": return "ASS/SSA subtitles";
		case "ttml": return "TTML subtitles";
		case "markdown": return "Markdown document";
		default: return "text file";
	}
}

/** formatPreserveRules: the per-format instruction on which lines must stay byte-for-byte identical. */
export function formatPreserveRules(format: string): string {
	switch (format) {
		case "lrc":
			return "This is an LRC lyrics file. Preserve every timestamp and the [ti:][ar:][al:] metadata tags EXACTLY as they are (character for character). Never merge, drop or alter timestamp lines.\n";
		case "srt":
			return "This is an SRT subtitle file. Preserve the cue index numbers and every 'HH:MM:SS,mmm --> HH:MM:SS,mmm' timing line EXACTLY as they are. Never merge, drop or alter timing lines.\n";
		case "vtt":
			return "This is a WebVTT subtitle file. Preserve the WEBVTT header, NOTE blocks, cue identifiers and every 'hh:mm:ss.mmm --> hh:mm:ss.mmm' timing line EXACTLY as they are. Never merge, drop or alter timing lines.\n";
		case "ass":
			return "This is an ASS/SSA subtitle file. Preserve ALL other lines (Script Info, Style headers, Format:, Style:) and every Dialogue: line's prefix fields (layer, start, end, style, name, margins, effect) EXACTLY as they are — byte for byte, including punctuation. Keep \\N and \\h override tags inside the text.\n";
		case "ttml":
			return "This is a TTML XML subtitle file. Preserve every XML tag and its attributes (begin/end/dur etc.) EXACTLY as they are; never add, remove or reorder elements; the result must remain valid XML.\n";
		case "markdown":
			return "This is a Markdown file. Preserve code blocks, URLs, link/image syntax, heading and list markers; change only the visible prose text.\n";
		default:
			return "Plain text.\n";
	}
}

function appendStyle(b: string[], style: string): void {
	if (style !== "") {
		b.push(style);
		if (!style.endsWith("\n")) b.push("\n");
	}
}

/** Prompt: the per-chunk translation instruction. */
export function prompt(format: string, from: string, to: string, reminder: boolean, style: string): string {
	const f = from === "" ? "auto" : from;
	const t = to === "" ? "English" : to;
	const b: string[] = [];
	b.push(`Translate the following ${formatName(format)} from ${f} to ${t}.\n`);
	b.push(formatPreserveRules(format));
	if (reminder) {
		b.push("TRANSLATION VERIFICATION FAILED LAST TIME because structural lines were altered. They must stay byte-for-byte identical.\n");
	}
	appendStyle(b, style);
	b.push("Reply with ONLY the translated content — no preamble, no commentary, no code fences.\n\n");
	return b.join("");
}

/** improvePrompt: fix grammar/flow/clarity, never translate. */
export function improvePrompt(format: string, reminder: boolean, style: string): string {
	const b: string[] = [];
	b.push(`Improve the writing of the following ${formatName(format)}. Fix grammar, spelling, punctuation, clarity, flow and word choice; preserve meaning, tone, register, facts and names. Do not translate.\n`);
	b.push(formatPreserveRules(format));
	if (reminder) {
		b.push("IMPROVEMENT VERIFICATION FAILED LAST TIME because structural lines were altered. They must stay byte-for-byte identical.\n");
	}
	appendStyle(b, style);
	b.push("Reply with ONLY the improved content — no preamble, no commentary, no code fences.\n\n");
	return b.join("");
}

/** summarizePrompt: condense; deliberately omits formatPreserveRules (a summary never reproduces structural lines). */
export function summarizePrompt(format: string, reminder: boolean, style: string): string {
	const b: string[] = [];
	b.push(`Summarize the following ${formatName(format)}. Capture the main points, events, names and conclusions; be concise; do not translate or rewrite the text.\n`);
	if (reminder) {
		b.push("THE PREVIOUS ATTEMPT FAILED. Retry the chunk.\n");
	}
	appendStyle(b, style);
	b.push("Reply with ONLY the summary — no preamble, no commentary, no code fences.\n\n");
	return b.join("");
}

// --- Instruction (style) files --------------------------------------------

export function defaultStyle(): string {
	return `[STYLE: GENERAL]
Translate into natural, unambiguous, and contextually accurate target-language text.
1. Resolve dropped or ambiguous subjects/objects from context; never default to "he" or random pronouns.
2. If true ambiguity remains, stay faithful or append a bracketed [TN: ...] note rather than guessing.
3. Prefer active voice; keep the passive only when the agent is unknown or the focus must stay on the receiver.
4. Match the source's register: honorifics, casual speech and business distance map to equivalent constructions in the target language.
5. Beware false friends, coined loanwords and direct calques — translate meaning, not literal form.
6. Avoid mechanical connectors; vary transitions or omit them when the logical flow is clear.
7. Follow the source's structure: headings, lists, blank lines, scene breaks and unlocalisable lines (URLs, tags) preserved exactly.
8. Keep names and terminology consistent throughout the document.
9. Output only the translation — no commentary, no meta text, no code fences.
`;
}

export function defaultImproveStyle(): string {
	return `[STYLE: IMPROVE WRITING]
Improve the prose without changing its meaning, tone, register or facts.
1. Fix grammar, spelling, punctuation and awkward phrasing.
2. Tighten wordy sentences; prefer active voice and concrete nouns.
3. Vary sentence rhythm; cut filler ("very", "really", "in order to", "that of").
4. Keep names, terms, numbers and all formatting exactly as given.
5. Do not translate or localise; keep the source language.
6. Output only the improved text — no commentary, no meta text, no code fences.
`;
}

export function defaultSummarizeStyle(): string {
	return `[STYLE: SUMMARIZE]
Write a compact, readable summary of the text.
1. Open with what the text is about and its main outcome or conclusion.
2. Cover key events, arguments, names and numbers; skip minor detail.
3. Keep the source language; do not translate.
4. Use flowing prose or short paragraphs, not lists, unless the source is a list.
5. Output only the summary — no commentary, no meta text, no code fences.
`;
}

/** styleDirs: searched for instruction files, in order (cwd first, then the config dir). */
export function styleDirs(kind: string): string[] {
	return [kind, join(homedir(), ".config", "dscli", kind)];
}

/** FindStyleFile searches the dirs for <pair>.md then default.md. */
export function findStyleFile(from: string, to: string): string {
	const key = `${pairKey(from)}-${pairKey(to)}`;
	for (const dir of styleDirs("translate")) {
		for (const name of [`${key}.md`, "default.md"]) {
			const p = join(dir, name);
			if (existsSync(p)) return p;
		}
	}
	return "";
}

/** ResolveStyle: an explicit file, else a discovered per-pair file, else the built-in default. */
export function resolveStyle(explicit: string, from: string, to: string): string {
	if (explicit !== "") return readFileSync(explicit, "utf8").trim();
	const p = findStyleFile(from, to);
	if (p !== "") return readFileSync(p, "utf8").trim();
	return defaultStyle().trim();
}

/** ResolveImproveStyle: an explicit file, else improve-writing/default.md, else the built-in default. */
export function resolveImproveStyle(explicit: string): string {
	if (explicit !== "") return readFileSync(explicit, "utf8").trim();
	for (const dir of styleDirs("improve-writing")) {
		const p = join(dir, "default.md");
		if (existsSync(p)) return readFileSync(p, "utf8").trim();
	}
	return defaultImproveStyle().trim();
}

/** ResolveSummarizeStyle: an explicit file, else summarize/default.md, else the built-in default. */
export function resolveSummarizeStyle(explicit: string): string {
	if (explicit !== "") return readFileSync(explicit, "utf8").trim();
	for (const dir of styleDirs("summarize")) {
		const p = join(dir, "default.md");
		if (existsSync(p)) return readFileSync(p, "utf8").trim();
	}
	return defaultSummarizeStyle().trim();
}

// --- The engine ------------------------------------------------------------

/**
 * translateChunk sends one chunk in the session thread and returns the
 * translated text plus the conversation id for the next chunk. When the
 * reply was cut off at the output limit, text holds the partial reply and
 * truncated=true so the caller retries with a smaller chunk. A
 * content-filtered reply keeps its partial content (retrying re-triggers
 * the filter); an empty filtered reply is a hard failure.
 */
export async function translateChunk(
	client: DeepSeekClient,
	conversation: string,
	promptText: string,
	model: string,
	thinking: boolean,
): Promise<{ text: string; convId: string; truncated: boolean }> {
	let sessionId = conversation;
	let parentId: number | null = null;
	const colon = conversation.indexOf(":");
	if (colon >= 0) {
		sessionId = conversation.slice(0, colon);
		const n = Number(conversation.slice(colon + 1));
		if (Number.isInteger(n) && Number.isFinite(n)) parentId = n;
	}
	let target = parentId;
	let modelType: string | undefined;
	if (parentId === null) {
		modelType = model;
	} else {
		target = parentId;
	}
	if (target === null) {
		// First turn: modelType set, parent stays null.
	} else {
		modelType = undefined;
	}

	let buf = "";
	const reply = await client.streamCompletion(
		{
			chatSessionId: sessionId,
			parentMessageId: target,
			prompt: promptText,
			modelType,
			thinkingEnabled: thinking,
			searchEnabled: false,
		},
		(d) => {
			buf += d;
		},
	);
	const nextOf = (): string => {
		if (reply.messageId !== 0) return `${sessionId}:${reply.messageId}`;
		if (parentId !== null) return `${sessionId}:${parentId}`;
		return sessionId;
	};
	if (reply.filtered) {
		const partial = buf.trim();
		if (partial === "") {
			throw new Error("reply was filtered by DeepSeek (content policy); no content produced");
		}
		return { text: partial, convId: nextOf(), truncated: false };
	}
	if (reply.truncated) {
		// The model stopped at its output limit: return the partial text and
		// truncated so the caller shrinks the chunk and retries.
		return { text: buf, convId: "", truncated: true };
	}
	return { text: buf.trim(), convId: nextOf(), truncated: false };
}

/**
 * Translate runs the chunked translation over sessionId (which must already
 * exist), carrying the conversation id turn by turn. Structural formats are
 * verified per chunk and retried once when corrupted. Returns the assembled
 * translated text (always newline-terminated) and the final conversation id.
 *
 * Chunk sizes are adaptive: the binding limit is the model's per-response
 * OUTPUT budget, so the engine probes a small first chunk, learns the real
 * output/input byte ratio, then sizes the remaining chunks to fill the
 * output budget — and shrinks whenever a reply is still truncated. Every
 * completed chunk is kept, so nothing is discarded and re-translated.
 */
export async function translate(
	client: DeepSeekClient,
	sessionId: string,
	content: string,
	format: string,
	opts: Options,
): Promise<{ text: string; convId: string }> {
	let maxChunk = opts.chunkBytes ?? 0;
	if (maxChunk <= 0) maxChunk = DEFAULT_CHUNK_BYTES;
	const model = opts.model === undefined || opts.model === "" ? "default" : opts.model;
	const src = content;
	const task = opts.task ?? TASK_TRANSLATE;

	let conversation = sessionId;
	const translated: string[] = [];

	const promptFor = (reminder: boolean): string => {
		switch (task) {
			case TASK_IMPROVE:
				return improvePrompt(format, reminder, opts.style ?? "");
			case TASK_SUMMARIZE:
				return summarizePrompt(format, reminder, opts.style ?? "");
			default:
				return prompt(format, opts.from ?? "", opts.to ?? "", reminder, opts.style ?? "");
		}
	};

	const sizer: ChunkSizer = newSizer(task, opts.thinking ?? false, maxChunk);

	let offset = 0;
	while (offset < src.length) {
		const chunk = firstChunk(src.slice(offset), sizer.size());
		if (chunk === "") break;
		let result: { text: string; convId: string; truncated: boolean };
		try {
			result = await translateChunk(client, conversation, promptFor(false) + chunk, model, opts.thinking ?? false);
		} catch (err) {
			throw new Error(`chunk (${Buffer.byteLength(chunk, "utf8")} bytes): ${err instanceof Error ? err.message : err}`);
		}
		if (result.truncated) {
			// Reply hit the output limit: learn from the partial text, shrink
			// the chunk, and re-split the remaining text from this offset.
			// The sizer gives up (false) at the minimum chunk size.
			if (!sizer.truncated(Buffer.byteLength(result.text, "utf8"))) {
				throw new Error(`chunk (${Buffer.byteLength(chunk, "utf8")} bytes): the reply hits the output limit even at the minimum chunk size`);
			}
			continue;
		}
		conversation = result.convId;

		// Structural formats must keep their timestamps/header markup
		// byte-for-byte. A summary never reproduces the structural lines, so
		// it skips verification entirely.
		if (task !== TASK_SUMMARIZE) {
			const verr = verifyProtected(format, chunk, result.text);
			if (verr) {
				const strict =
					promptFor(true) +
					"The previous attempt changed a protected (timestamps/header) line.\n" +
					"Keep every line with a timestamp or the WEBVTT/header syntax EXACTLY as in the original. Retry the chunk:\n\n" +
					chunk;
				let retry: { text: string; convId: string; truncated: boolean };
				try {
					retry = await translateChunk(client, conversation, strict, model, opts.thinking ?? false);
				} catch (err) {
					throw new Error(`chunk (${Buffer.byteLength(chunk, "utf8")} bytes): ${err instanceof Error ? err.message : err}`);
				}
				conversation = retry.convId;
				const verr2 = verifyProtected(format, chunk, retry.text);
				if (verr2) {
					throw new Error(`chunk (${Buffer.byteLength(chunk, "utf8")} bytes): ${verr.message}`);
				}
				result.text = retry.text;
			}
		}

		sizer.success(Buffer.byteLength(chunk, "utf8"), Buffer.byteLength(result.text, "utf8"));
		translated.push(result.text);
		offset += Buffer.byteLength(chunk, "utf8");
		if (opts.onChunk) {
			const remaining = Buffer.byteLength(src, "utf8") - offset;
			let total = translated.length;
			if (remaining > 0) total += Math.ceil(remaining / sizer.size());
			opts.onChunk(translated.length, total);
		}
	}
	return { text: translated.join("\n").trim() + "\n", convId: conversation };
}

/**
 * Summarize condenses content into a summary using the chunk engine with
 * TaskSummarize (verification skipped). When the document needed more than
 * one chunk, the per-chunk summaries are combined in a final pass.
 */
export async function summarize(
	client: DeepSeekClient,
	sessionId: string,
	content: string,
	format: string,
	opts: Options,
): Promise<{ text: string; convId: string }> {
	let chunks = 0;
	const prev = opts.onChunk;
	opts.task = TASK_SUMMARIZE;
	opts.onChunk = (done, total) => {
		chunks = done;
		if (prev) prev(done, total);
	};
	const { text, convId } = await translate(client, sessionId, content, format, opts);
	if (chunks <= 1) return { text, convId };

	const model = opts.model === undefined || opts.model === "" ? "default" : opts.model;
	const promptText =
		"The sections below are summaries of consecutive parts of one document, in order. " +
		"Combine them into a single coherent summary that keeps every key point, name and conclusion; " +
		"drop repetition; keep the ordering. Reply with ONLY the combined summary — no preamble, no commentary.\n\n" +
		text;
	const combined = await translateChunk(client, convId, promptText, model, opts.thinking ?? false);
	return { text: combined.text.trim() + "\n", convId: combined.convId };
}
