/**
 * Turns DeepSeek's SSE json-patch stream into reply-text deltas.
 *
 * The stream sends an initial snapshot frame whose v is the whole response
 * object (fragments[].type == "response" carries the content and the
 * assistant message_id lives at v.message_id / v.message.id or inside
 * v.response), then a series of append frames:
 *
 *   {"p":"response/fragments/-1/content","o":"APPEND","v":" He"}
 *   {"v":"llo"}
 *
 * Only text appended to a path ending in "/content" is treated as reply text.
 * Search-enabled replies may also carry TOOL_SEARCH fragments (with
 * references/results) or .../results patch paths; those are captured into
 * sources so the CLI can print the citations the model references inline as
 * [citation:N].
 *
 * Direct port of internal/deepseek/sse.go — the frame grammar and routing
 * rules are the tricky part, so the structure is kept line-for-line close.
 */

export interface Source {
	url: string;
	title: string;
}

/** One search citation. */
function sourceFromItem(it: unknown): Source {
	if (typeof it === "string") {
		if (it.startsWith("http://") || it.startsWith("https://")) return { url: it, title: "" };
		return { url: "", title: "" };
	}
	if (it && typeof it === "object") {
		const m = it as Record<string, unknown>;
		return {
			url: firstString(m, "url", "link", "href", "source"),
			title: firstString(m, "title", "name", "label"),
		};
	}
	return { url: "", title: "" };
}

function firstString(m: Record<string, unknown>, ...keys: string[]): string {
	for (const k of keys) {
		const v = m[k];
		if (typeof v === "string") return v;
	}
	return "";
}

function mapAny(v: unknown): Record<string, unknown> {
	return v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
}

/** numberToInt64: JSON numbers truncate to integer ids. */
function numberToInt64(v: unknown): number | undefined {
	if (typeof v === "number" && Number.isFinite(v)) return Math.trunc(v);
	if (typeof v === "string") {
		const n = Number(v);
		if (v.trim() !== "" && Number.isFinite(n)) return Math.trunc(n);
	}
	return undefined;
}

/** textOf extracts visible text from a chunk value: a plain string, or an object carrying a "text"/"content" string field. */
function textOf(v: unknown): string | undefined {
	if (typeof v === "string") return v;
	if (v && typeof v === "object" && !Array.isArray(v)) {
		const t = v as Record<string, unknown>;
		for (const k of ["text", "content"]) {
			if (typeof t[k] === "string") return t[k] as string;
		}
	}
	return undefined;
}

export class PatchParser {
	activePath = "";
	messageId: number | undefined;
	sources: Source[] = [];
	/** Completion state: FINISHED vs truncated (output limit / content filter). */
	finished = false;
	truncated = false;
	/** True when the stream ended with CONTENT_FILTER (censor, not length). */
	filtered = false;
	/** Fragment kinds in container order, to attribute content updates; THINK/SEARCH/TIP fragments never render as answer text. */
	private fragKinds: string[] = [];
	private fragEmitted: boolean[] = [];
	private sawSnapshot = false;

	/** noteStatus inspects a status patch: FINISHED is clean, INCOMPLETE/WIP/AUTO_CONTINUE cut short, CONTENT_FILTER censored. */
	private noteStatus(path: string, v: unknown): void {
		if (!path.endsWith("status") && path !== "quasi_status") return;
		if (typeof v !== "string") return;
		switch (v.trim().toUpperCase()) {
			case "FINISHED":
				// A clean end overrides any earlier transient WIP/INCOMPLETE signal.
				this.finished = true;
				this.truncated = false;
				this.filtered = false;
				break;
			case "CONTENT_FILTER":
				this.truncated = true;
				this.filtered = true;
				break;
			case "INCOMPLETE":
			case "WIP":
			case "AUTO_CONTINUE":
				this.truncated = true;
				break;
		}
	}

	/**
	 * Feed processes one SSE data payload (possibly several JSON patch frames
	 * concatenated) and calls emit for each reply-text delta. Malformed
	 * payloads are skipped, like the website's client does. Returns an error
	 * message when an upstream error frame arrives.
	 */
	feed(payload: string, emit: (text: string) => void): string | undefined {
		for (const value of parseJsonStream(payload)) {
			if (!value || typeof value !== "object" || Array.isArray(value)) continue;
			const err = this.feedOne(value as Record<string, unknown>, emit);
			if (err) return err;
		}
		return undefined;
	}

	private feedOne(obj: Record<string, unknown>, emit: (text: string) => void): string | undefined {
		// Upstream error frame (e.g. rate-limit or safety rejection). Without
		// this the reply silently comes back empty and "successful" — the CLI
		// would write a blank or cut-off file and claim it worked.
		const t = obj["type"];
		if (typeof t === "string" && t.toLowerCase() === "error") {
			let msg = typeof obj["content"] === "string" ? (obj["content"] as string) : "";
			const reason = typeof obj["finish_reason"] === "string" ? (obj["finish_reason"] as string) : "";
			if (reason !== "") msg = (msg + " (" + reason + ")").trim();
			if (msg === "") msg = "upstream error frame";
			return msg;
		}
		const hasV = "v" in obj;
		const v = obj["v"];

		// Snapshot frame: v is the whole response object.
		if (hasV) {
			const snap = mapAny(v);
			if ("response" in snap) {
				this.captureMessageID(snap);
				const frags = mapAny(snap["response"])["fragments"];
				if (Array.isArray(frags)) {
					for (const f of frags) {
						const fm = mapAny(f);
						if (Object.keys(fm).length === 0) continue;
						this.registerFragment(fm);
						const ft = typeof fm["type"] === "string" ? (fm["type"] as string) : "";
						if (ft.toLowerCase() === "response") {
							// The content path is live even when the snapshot
							// text is empty: the first real chunk may arrive
							// as a pathless {"v":...} before any "p" frame.
							this.activePath = "response/fragments/-1/content";
							const content = typeof fm["content"] === "string" ? (fm["content"] as string) : "";
							if (content === "") continue;
							// Only the first response fragment's content is
							// pre-generated; later text arrives as appends.
							if (!this.sawSnapshot) {
								this.sawSnapshot = true;
								emit(content);
								this.markEmitted(this.fragKinds.length - 1);
							}
						} else if (ft.toLowerCase() === "tool_search") {
							this.collectSources(fm);
						}
					}
				}
				return undefined;
			}
		}

		// Path-setting frame (single patch, or BATCH of nested patches).
		if (typeof obj["p"] === "string") {
			const path = obj["p"] as string;
			this.activePath = path;
			if (path.endsWith("message_id")) {
				const id = numberToInt64(v);
				if (id !== undefined) this.messageId = id;
			}
			const op = typeof obj["o"] === "string" ? (obj["o"] as string) : "";
			if (op === "BATCH") {
				if (Array.isArray(v)) {
					for (const it of v) {
						const m = mapAny(it);
						if (Object.keys(m).length === 0) continue;
						const pp = typeof m["p"] === "string" ? (m["p"] as string) : "";
						const oo = typeof m["o"] === "string" ? (m["o"] as string) : "";
						this.applyPatch(pp, m["v"], oo, emit);
					}
				}
				return undefined;
			}
			this.applyPatch(path, v, op, emit);
			return undefined;
		}

		// Bare (pathless) chunk: per upstream behaviour these are visible-text
		// candidates, and they may arrive before any path is active (this is
		// what used to eat the reply's first characters).
		this.applyPathless(v, emit);
		return undefined;
	}

	/** applyPatch handles one content/results patch operation. */
	private applyPatch(path: string, v: unknown, op: string, emit: (text: string) => void): void {
		this.noteStatus(path, v);
		if (path.endsWith("/results") || path.endsWith("/references")) {
			this.collectSources(v);
		}
		// Container append: new fragments arrive as
		// {"p":"response/fragments","o":"APPEND","v":[{fragment},...]}. The
		// answer's FIRST token often lives in a RESPONSE fragment appended this
		// way (the snapshot only carries THINK/TOOL_SEARCH fragments), so this
		// frame must not be skipped just because the path does not end in
		// "content".
		if ((path === "response/fragments" || path === "fragments") && op === "APPEND") {
			this.applyFragments(v, emit);
			return;
		}
		if (!path.endsWith("content")) return;
		const idx = this.fragIndexAt(path);
		// Content belonging to a non-answer fragment (thinking, search, tips)
		// is never rendered.
		if (idx >= 0 && this.fragType(idx).toLowerCase() !== "response") return;
		switch (op) {
			case "APPEND":
			case "":
				// APPEND continues the content slot. An op-less frame (no "o") is
				// ALSO a continuation: the real stream continues the RESPONSE
				// fragment's content with
				// {"p":"response/fragments/-1/content","v":...}
				// after the container-append emitted the first token — treating it
				// as a full-slot SET drops that chunk and corrupts the reply.
				{
					const txt = textOf(v);
					if (txt !== undefined) {
						this.markEmitted(idx);
						emit(txt);
					}
				}
				break;
			case "SET":
				// SET replaces the whole content slot of a fragment; it is the
				// initial text when we have not emitted that fragment's content
				// yet, and a no-op afterwards (no duplicates).
				if (!this.fragWasEmitted(idx)) {
					const txt = textOf(v);
					if (txt !== undefined) {
						emit(txt);
						this.markEmitted(idx);
					}
				}
				break;
		}
	}

	/** registerFragment records a fragment's type (container order) and emits nothing. */
	private registerFragment(fm: Record<string, unknown>): void {
		this.fragKinds.push(typeof fm["type"] === "string" ? (fm["type"] as string) : "");
		this.fragEmitted.push(false);
	}

	/**
	 * fragIndexAt resolves the container index a content path targets
	 * ("response/fragments/-1/content" -> the last RESPONSE fragment, else the
	 * literal last); -1 when unresolvable.
	 */
	private fragIndexAt(path: string): number {
		const i = path.indexOf("fragments/");
		if (i < 0) return -1;
		const rest = path.slice(i + "fragments/".length);
		const idxStr = rest.split("/", 1)[0] ?? "";
		if (idxStr === "-1") {
			// "-1" means "the fragment whose content is streaming": always the
			// most recent RESPONSE fragment. A THINK fragment appended afterwards
			// (interleaved reasoning) must not hijack the content routing.
			const r = this.lastResponseIdx();
			if (r >= 0) return r;
			return this.fragKinds.length - 1;
		}
		const n = Number(idxStr);
		if (!Number.isInteger(n) || n < 0 || n >= this.fragKinds.length) return -1;
		return n;
	}

	/** lastResponseIdx: content and pathless chunks continue the RESPONSE fragment, never a THINK/SEARCH fragment. */
	private lastResponseIdx(): number {
		for (let i = this.fragKinds.length - 1; i >= 0; i--) {
			if (this.fragKinds[i]!.toLowerCase() === "response") return i;
		}
		return -1;
	}

	private fragType(i: number): string {
		return this.fragKinds[i] ?? "";
	}

	private fragWasEmitted(i: number): boolean {
		if (i < 0 || i >= this.fragEmitted.length) return true; // unknown slot: assume emitted to avoid duplicates
		return this.fragEmitted[i]!;
	}

	private markEmitted(i: number): void {
		if (i >= 0 && i < this.fragEmitted.length) this.fragEmitted[i] = true;
	}

	/** applyFragments handles a container-appended array of fragment objects. */
	private applyFragments(v: unknown, emit: (text: string) => void): void {
		if (!Array.isArray(v)) return;
		for (const f of v) {
			const fm = mapAny(f);
			if (Object.keys(fm).length === 0) continue;
			const t = typeof fm["type"] === "string" ? (fm["type"] as string) : "";
			this.registerFragment(fm);
			if (t.toLowerCase() === "response") {
				this.activePath = "response/fragments/-1/content";
				const content = typeof fm["content"] === "string" ? (fm["content"] as string) : "";
				if (content === "") continue;
				emit(content);
				this.markEmitted(this.fragKinds.length - 1);
			} else if (t.toLowerCase() === "tool_search") {
				this.collectSources(fm);
			}
		}
	}

	/**
	 * applyPathless emits a pathless chunk. Pathless string chunks are visible
	 * answer text. Before the first RESPONSE fragment they belong to the
	 * still-streaming THINK/SEARCH fragments and must never render as answer
	 * text; once a RESPONSE fragment exists they continue it — even when a
	 * THINK fragment was appended in between (interleaved reasoning) or a
	 * status/results frame landed between two chunks (DeepThink streams do
	 * this). The active path is deliberately NOT consulted: a non-content
	 * frame interleaved between answer chunks must not swallow the next chunk.
	 */
	private applyPathless(v: unknown, emit: (text: string) => void): void {
		// A pathless terminal batch carries status/state patches as an array of
		// {p, v} objects (e.g. {"v":[{"p":"status","v":"CONTENT_FILTER"},...]}).
		// Extract any status signals so truncated replies are detected even
		// though the array itself is not reply text.
		if (Array.isArray(v)) {
			for (const it of v) {
				const m = mapAny(it);
				const pp = m["p"];
				if (typeof pp === "string") this.noteStatus(pp, m["v"]);
			}
			return;
		}
		const idx = this.lastResponseIdx();
		if (idx < 0 && this.fragKinds.length > 0) return;
		const txt = textOf(v);
		if (txt === undefined) return;
		this.markEmitted(idx);
		emit(txt);
	}

	/** collectSources appends citation sources found in a TOOL_SEARCH fragment or a .../results patch value, deduplicated by URL. */
	private collectSources(v: unknown): void {
		if (v && typeof v === "object" && !Array.isArray(v)) {
			const m = v as Record<string, unknown>;
			for (const key of ["references", "results", "result"]) {
				if (Array.isArray(m[key])) this.addSourceItems(m[key] as unknown[]);
			}
			return;
		}
		if (Array.isArray(v)) this.addSourceItems(v);
	}

	private addSourceItems(items: unknown[]): void {
		for (const it of items) {
			const s = sourceFromItem(it);
			if (s.url === "") continue;
			if (this.sources.some((have) => have.url === s.url)) continue;
			this.sources.push(s);
		}
	}

	/** captureMessageID best-effort: v.response first, then the snapshot root, accepting "message_id" or "id". */
	private captureMessageID(snap: Record<string, unknown>): void {
		for (const container of [mapAny(snap["response"]), snap]) {
			for (const key of ["message_id", "id"]) {
				const id = numberToInt64(container[key]);
				if (id !== undefined) {
					this.messageId = id;
					return;
				}
			}
		}
	}
}

/**
 * parseJsonStream splits a payload holding several concatenated JSON values
 * (upstream can batch multiple patch frames into one SSE event) and parses
 * each. Malformed values are skipped, like the Go json.Decoder loop.
 */
export function parseJsonStream(payload: string): unknown[] {
	const out: unknown[] = [];
	let i = 0;
	const n = payload.length;
	while (i < n) {
		// Skip whitespace between values.
		while (i < n && /\s/.test(payload[i]!)) i++;
		if (i >= n) break;
		// Scan one top-level value, tracking strings and nesting depth.
		const start = i;
		let depth = 0;
		let inString = false;
		let escaped = false;
		let ended = false;
		for (; i < n; i++) {
			const ch = payload[i]!;
			if (inString) {
				if (escaped) escaped = false;
				else if (ch === "\\") escaped = true;
				else if (ch === '"') inString = false;
				continue;
			}
			if (ch === '"') inString = true;
			else if (ch === "{" || ch === "[") depth++;
			else if (ch === "}" || ch === "]") {
				depth--;
				if (depth === 0) {
					i++;
					ended = true;
					break;
				}
			} else if (depth === 0 && /[,\s]/.test(ch)) {
				// A bare scalar (string/number/literal) ends at the separator.
				if (start < i) {
					ended = true;
					break;
				}
			}
		}
		const slice = payload.slice(start, ended ? i : n);
		if (slice.trim() === "") break;
		try {
			out.push(JSON.parse(slice));
		} catch {
			// Malformed tail: ignore, like the Go decoder returning at EOF.
			break;
		}
		if (!ended) break;
	}
	return out;
}
