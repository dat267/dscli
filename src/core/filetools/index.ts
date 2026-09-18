/**
 * Format- and content-handling helpers shared by the translate engine:
 * text/EPUB detection and extraction, and byte-for-byte verification of
 * subtitle/lyric structure after a translation. Port of
 * internal/filetools/filetools.go.
 */
import { unzipSync } from "fflate";
import { dirname, extname, join } from "node:path";
import { readFileSync } from "node:fs";

/** Caps how much extracted EPUB text (or any single content read) is kept. */
export const DEFAULT_MAX_READ_BYTES = 512 * 1024;
export let maxReadBytes = DEFAULT_MAX_READ_BYTES;

export function setMaxReadBytes(n: number): void {
	maxReadBytes = n;
}

/** How many leading bytes are inspected to classify text vs binary (a NUL byte means binary). */
const BINARY_PROBE_SIZE = 8000;

/** IsText: no NUL byte in the probe. */
export function isText(data: Uint8Array): boolean {
	const probe = data.length > BINARY_PROBE_SIZE ? data.subarray(0, BINARY_PROBE_SIZE) : data;
	return !probe.includes(0);
}

/** DetectFormat: the translation format for a path, else "text". */
export function detectFormat(path: string, _content: Uint8Array): "lrc" | "srt" | "vtt" | "ass" | "ttml" | "markdown" | "text" {
	switch (extname(path).toLowerCase()) {
		case ".lrc":
			return "lrc";
		case ".srt":
			return "srt";
		case ".vtt":
			return "vtt";
		case ".ass":
		case ".ssa":
			return "ass";
		case ".ttml":
			return "ttml";
		case ".md":
		case ".markdown":
			return "markdown";
		default:
			return "text";
	}
}

const LRC_TIMECODE_RE = /\[\d{1,2}:\d{2}(?:\.\d+)?\]/g;
const XML_TAG_RE = /<\/?[a-zA-Z][^>]*>/g;
const ASS_DIALOGUE_RE = /^(?:dialogue|comment):/i;

/**
 * ProtectedLines: the structural tokens of a format that MUST survive a
 * translation byte-for-byte — LRC timestamps, SRT/VTT timing and header
 * lines, ASS everything outside the dialogue text field, TTML tags in
 * order. Empty for plain text.
 */
export function protectedLines(format: string, content: string): string[] {
	switch (format) {
		case "lrc":
			return content.match(LRC_TIMECODE_RE) ?? [];
		case "srt": {
			const out: string[] = [];
			for (const line of content.split("\n")) {
				const trim = line.trim();
				if (trim.includes("-->")) out.push(trim);
			}
			return out;
		}
		case "vtt": {
			const out: string[] = [];
			for (const line of content.split("\n")) {
				const trim = line.trim();
				if (trim.includes("-->") || trim === "WEBVTT" || trim.startsWith("NOTE")) out.push(trim);
			}
			return out;
		}
		case "ass": {
			const out: string[] = [];
			for (const line of content.split("\n")) {
				const trim = line.trim();
				if (trim === "") continue;
				if (ASS_DIALOGUE_RE.test(trim)) out.push(assDialoguePrefix(trim));
				else out.push(trim); // script info, style headers, Format:, Style:
			}
			return out;
		}
		case "ttml":
			return content.match(XML_TAG_RE) ?? [];
		default:
			return [];
	}
}

/** assDialoguePrefix: the Dialogue/Comment prefix through the 9th comma (the text field after it is translated). */
export function assDialoguePrefix(line: string): string {
	let n = 0;
	for (let i = 0; i < line.length; i++) {
		if (line[i] === ",") {
			n++;
			if (n === 9) return line.slice(0, i + 1);
		}
	}
	return line;
}

/** VerifyProtected: the protected lines of a translation must exactly equal the original's. */
export function verifyProtected(format: string, original: string, translated: string): Error | undefined {
	const orig = protectedLines(format, original);
	const trans = protectedLines(format, translated);
	if (orig.length !== trans.length) {
		return new Error(`protected line count changed (${orig.length} → ${trans.length}); timestamps/headers must stay identical`);
	}
	for (let i = 0; i < orig.length; i++) {
		if (orig[i] !== trans[i]) {
			return new Error(`protected line ${i + 1} changed; timestamps/headers must stay identical`);
		}
	}
	return undefined;
}

/** ReadEpub: the chapter text of an EPUB (ZIP of XHTML) in spine order, stripped of markup, capped. */
export function readEpub(path: string): string {
	const data = readFileSync(path);
	const files = unzipSync(data);
	// The ZIP central directory stores names as UTF-8 strings.
	const entries = new Map<string, Uint8Array>();
	for (const [name, bytes] of Object.entries(files)) entries.set(name, bytes);
	const find = (name: string): Uint8Array | undefined => {
		for (const [k, v] of entries) {
			if (k.toLowerCase() === name.toLowerCase()) return v;
		}
		return undefined;
	};
	const container = find("META-INF/container.xml");
	if (!container) throw new Error("missing META-INF/container.xml");
	const opfPath = attr(decodeUtf8(container).match(/<rootfile\b[^>]*>/)?.[0] ?? "", "full-path");
	if (!opfPath) throw new Error("container.xml has no rootfile");
	const opfBytes = find(opfPath);
	if (!opfBytes) throw new Error(`OPF "${opfPath}" not found`);
	const opf = decodeUtf8(opfBytes);

	const hrefs = new Map<string, string>();
	for (const itemTag of opf.match(/<item\b[^>]*\/?>/g) ?? []) {
		const id = attr(itemTag, "id");
		const href = attr(itemTag, "href");
		if (id && href) hrefs.set(id, href);
	}
	const base = dirname(opfPath);
	let out = "";
	for (const refTag of opf.match(/<itemref\b[^>]*\/?>/g) ?? []) {
		const idref = attr(refTag, "idref");
		if (!idref) continue;
		const href = hrefs.get(idref);
		if (!href) continue;
		const chapter = find(join(base, href).replaceAll("\\", "/"));
		if (!chapter) continue;
		if (chapter.length > maxReadBytes + 1) continue; // cap per chapter read
		const text = stripHTML(decodeUtf8(chapter));
		if (out.length + text.length > maxReadBytes) {
			out += text.slice(0, maxReadBytes - out.length);
			break;
		}
		out += text;
	}
	return out;
}

/** attr extracts an XML attribute value from a tag string. */
function attr(tag: string, name: string): string | undefined {
	const m = new RegExp(`\\b${name}\\s*=\\s*"([^"]*)"`).exec(tag) ?? new RegExp(`\\b${name}\\s*=\\s*'([^']*)'`).exec(tag);
	return m?.[1];
}

function decodeUtf8(bytes: Uint8Array): string {
	return new TextDecoder("utf-8").decode(bytes);
}

/** stripHTML removes markup and decodes common entities, returning visible text. */
export function stripHTML(s: string): string {
	let r = "";
	let inTag = false;
	let inScript = false;
	let i = 0;
	const lower = s.toLowerCase();
	while (i < s.length) {
		if (inTag) {
			if (s[i] === ">") inTag = false;
			i++;
			continue;
		}
		if (!inTag && !inScript && lower.startsWith("<script", i)) {
			inTag = true;
			inScript = true;
			i++;
			continue;
		}
		if (inScript && lower.startsWith("</script", i)) {
			inScript = false;
			inTag = true;
			i++;
			continue;
		}
		if (inScript) {
			// Script/style content is not visible text. (The Go original's
			// guard never fired due to a length bug, so it leaked script
			// bodies; the port implements the documented intent.)
			i++;
			continue;
		}
		if (s[i] === "<") {
			inTag = true;
			i++;
			continue;
		}
		if (s[i] === "&") {
			const end = s.indexOf(";", i);
			if (end >= 0 && end - i <= 8) {
				const name = s.slice(i + 1, end);
				switch (name) {
					case "amp": r += "&"; break;
					case "lt": r += "<"; break;
					case "gt": r += ">"; break;
					case "quot": r += '"'; break;
					case "#39":
					case "apos": r += "'"; break;
					case "nbsp": r += " "; break;
					default: r += s.slice(i, end + 1);
				}
				i = end + 1;
				continue;
			}
			r += "&";
			i++;
			continue;
		}
		r += s[i];
		i++;
	}
	return r;
}
