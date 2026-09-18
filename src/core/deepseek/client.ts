/**
 * A stateful client for chat.deepseek.com's internal web API: session
 * lifecycle, PoW-challenged completions and SSE stream reconstruction.
 *
 * Port of internal/deepseek/client.go; the HTTP layer is Node's global fetch
 * (undici) with per-request timeouts via AbortSignal.
 */
import { appendFileSync, openSync } from "node:fs";
import { challengePrefix, powHeader, type Challenge } from "./pow.js";
import { PatchParser, type Source } from "./sse.js";

/** Endpoints and constants of chat.deepseek.com's internal web API. */
export const BASE_URL = "https://chat.deepseek.com";
export const COMPLETION_PATH = "/api/v0/chat/completion";
const POW_CHALLENGE_PATH = "/api/v0/chat/create_pow_challenge";
const SESSION_CREATE_PATH = "/api/v0/chat_session/create";
const SESSION_DELETE_PATH = "/api/v0/chat_session/delete";
const HISTORY_PATH = "/api/v0/chat/history_messages";

/** One-minute ceiling for the small JSON exchanges; the completion stream is bounded by the caller instead. */
const SHORT_TIMEOUT_MS = 30_000;

/** Mimics a current desktop Chrome so the WAF does not reject the plain HTTP client outright. */
export const DEFAULT_USER_AGENT =
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36";

/** Thrown when no usable DeepSeek session is configured. */
export class NoCredentialsError extends Error {
	constructor() {
		super("no DeepSeek session configured");
	}
}

/** sseDebugDump, when set via DSCLI_DEBUG_SSE=<file>, receives every raw SSE data payload (one per line). */
let sseDebugDump: number | undefined;
try {
	const path = process.env["DSCLI_DEBUG_SSE"];
	if (path) sseDebugDump = openSync(path, "a");
} catch {
	// Debug dump is best-effort.
}

/** The signed-in credentials captured from the website (localStorage.userToken, ds_session_id cookie). */
export interface Session {
	token: string;
	/** ds_session_id value, or a full "k=v; ..." cookie header. */
	cookie?: string;
	/** Optional; falls back to DEFAULT_USER_AGENT. */
	userAgent?: string;
}

/**
 * The cookie header: the config stores the bare ds_session_id value; a full
 * "k=v; k2=v2" string (any input containing "=") passes through untouched.
 */
export function sessionCookie(s: string): string {
	if (s === "") return "";
	if (s.includes("=")) return s;
	return `ds_session_id=${s}`;
}

/** One past message of a chat session, as returned by chatHistory. */
export interface HistoryMessage {
	message_id: number;
	parent_id: number | null;
	role: string;
	content: string;
	status: string;
	fragments: Array<{ type: string; content: string }>;
}

/**
 * The message's visible text: the content field when set, else the
 * fragments' content with thinking text excluded.
 */
export function historyMessageText(m: HistoryMessage): string {
	if (m.content !== "") return m.content;
	let out = "";
	for (const f of m.fragments) {
		switch (f.type.toUpperCase()) {
			case "THINK":
			case "THINKING":
			case "":
				continue;
		}
		out += f.content;
	}
	return out;
}

/** The body of POST /api/v0/chat/completion. */
export interface CompletionRequest {
	chatSessionId: string;
	/** null on the first turn (sent as JSON null). */
	parentMessageId: number | null;
	prompt: string;
	/** "default"/"expert" on the first turn; "" omits the field when resuming. */
	modelType?: string;
	thinkingEnabled: boolean;
	searchEnabled: boolean;
}

function completionBody(r: CompletionRequest): Record<string, unknown> {
	const b: Record<string, unknown> = {
		chat_session_id: r.chatSessionId,
		parent_message_id: r.parentMessageId,
		prompt: r.prompt,
		ref_file_ids: [],
		thinking_enabled: r.thinkingEnabled,
		search_enabled: r.searchEnabled,
		action: null,
		preempt: false,
	};
	if (r.modelType && r.modelType !== "") b["model_type"] = r.modelType;
	return b;
}

/** A completed stream: the assistant message_id needed to resume the thread. */
export interface Reply {
	messageId: number;
	/** Search citations for the reply's inline [citation:N] markers, in order. */
	sources: Source[];
	/** True when the reply stopped at its output limit (INCOMPLETE/WIP/AUTO_CONTINUE/CONTENT_FILTER) rather than FINISHED. */
	truncated: boolean;
	/** True when the stream ended with CONTENT_FILTER (a censor, not a length cut). */
	filtered: boolean;
}

export interface ClientOptions {
	/** Bounds the whole completion exchange including streaming; 0 = no bound. */
	timeoutMs?: number;
	/** Overrides the API base URL (tests, proxies). */
	base?: string;
}

/** Reads a non-200 body and turns it into an error, preferring the site's JSON {code, msg} envelope. */
async function httpStatusError(path: string, resp: Response): Promise<Error> {
	const text = await resp.text().catch(() => "");
	let msg = "";
	let code = 0;
	try {
		const env = JSON.parse(text) as { code?: number; msg?: string };
		code = env.code ?? 0;
		msg = env.msg ?? "";
	} catch {
		// Not JSON: fall through to the snippet.
	}
	if (code !== 0 || msg !== "") {
		const detail = msg === "" ? `code=${code}` : msg;
		return new Error(`deepseek api error: ${detail} (HTTP ${resp.status} for ${path})`);
	}
	let snippet = text.trim();
	if (snippet === "") snippet = `${resp.status} ${resp.statusText}`;
	if (snippet.length > 300) snippet = snippet.slice(0, 300) + "...";
	return new Error(`POST ${path} failed with HTTP ${resp.status}: ${snippet}`);
}

export class DeepSeekClient {
	private readonly base: string;
	private readonly sess: Session;
	private readonly ua: string;
	private readonly timeoutMs: number;

	constructor(sess: Session, opts: ClientOptions = {}) {
		this.base = opts.base && opts.base !== "" ? opts.base : BASE_URL;
		this.sess = sess;
		this.ua = sess.userAgent && sess.userAgent !== "" ? sess.userAgent : DEFAULT_USER_AGENT;
		this.timeoutMs = opts.timeoutMs ?? 0;
	}

	private headers(): Record<string, string> {
		const h: Record<string, string> = {
			authorization: `Bearer ${this.sess.token}`,
			"content-type": "application/json",
			accept: "*/*",
			"user-agent": this.ua,
			origin: BASE_URL,
			referer: `${BASE_URL}/`,
			"x-app-version": "2.0.0",
			"x-client-version": "2.0.0",
			"x-client-platform": "web",
			"x-client-bundle-id": "com.deepseek.chat",
			"x-client-locale": "en_US",
			"x-client-timezone-offset": "19800",
		};
		const cookie = sessionCookie(this.sess.cookie ?? "");
		if (cookie !== "") h["cookie"] = cookie;
		return h;
	}

	/** The signal bounding the completion stream, or none. */
	private completionSignal(): AbortSignal | undefined {
		return this.timeoutMs > 0 ? AbortSignal.timeout(this.timeoutMs) : undefined;
	}

	/** postJSON posts a small JSON exchange and checks the {code, data:{biz_data}} envelope. */
	private async postJSON(path: string, body: unknown, opts: { requireBizData?: boolean; signal?: AbortSignal } = {}): Promise<unknown> {
		const resp = await fetch(this.base + path, {
			method: "POST",
			headers: this.headers(),
			body: JSON.stringify(body),
			signal: opts.signal ?? AbortSignal.timeout(SHORT_TIMEOUT_MS),
		});
		if (!resp.ok) throw await httpStatusError(path, resp);
		const env = (await resp.json()) as { code?: number; msg?: string; data?: { biz_data?: unknown } };
		if ((env.code ?? 0) !== 0) {
			throw new Error(`deepseek api error: ${env.msg && env.msg !== "" ? env.msg : `code=${env.code}`}`);
		}
		const biz = env.data?.biz_data;
		if (opts.requireBizData !== false && (biz === undefined || biz === null)) {
			throw new Error("deepseek api error: missing data.biz_data");
		}
		return biz;
	}

	/** createChatSession starts a new chat session and returns its id. */
	async createChatSession(): Promise<string> {
		const biz = (await this.postJSON(SESSION_CREATE_PATH, {})) as { chat_session?: { id?: string } };
		const id = biz.chat_session?.id;
		if (!id) throw new Error("deepseek api error: chat session response missing id");
		return id;
	}

	/** deleteSessions removes chat sessions server-side; an empty list is a no-op. */
	async deleteSessions(ids: string[]): Promise<void> {
		if (ids.length === 0) return;
		// Only the standard envelope code matters (no biz_data is returned).
		await this.postJSON(SESSION_DELETE_PATH, { chat_session_ids: ids }, { requireBizData: false });
	}

	/** chatHistory fetches a session's past messages (for the UI's resume rendering). */
	async chatHistory(sessionId: string): Promise<HistoryMessage[]> {
		const u = this.base + HISTORY_PATH + "?chat_session_id=" + encodeURIComponent(sessionId);
		const resp = await fetch(u, { headers: this.headers(), signal: AbortSignal.timeout(SHORT_TIMEOUT_MS) });
		if (!resp.ok) throw await httpStatusError(HISTORY_PATH, resp);
		const env = (await resp.json()) as { code?: number; msg?: string; data?: { biz_data?: { chat_messages?: HistoryMessage[] } } };
		if ((env.code ?? 0) !== 0) {
			throw new Error(`deepseek api error: ${env.msg && env.msg !== "" ? env.msg : `code=${env.code}`}`);
		}
		return env.data?.biz_data?.chat_messages ?? [];
	}

	/** fetchChallenge fetches a PoW challenge for the completion endpoint. */
	private async fetchChallenge(): Promise<Challenge> {
		const biz = (await this.postJSON(POW_CHALLENGE_PATH, { target_path: COMPLETION_PATH })) as { challenge?: Challenge };
		const ch = biz.challenge;
		if (!ch || !ch.challenge) throw new Error("deepseek api error: pow challenge response missing challenge");
		return ch;
	}

	/** powHeader fetches a challenge and solves it, returning the base64 x-ds-pow-response header value. */
	private async powHeader(): Promise<string> {
		return powHeader(await this.fetchChallenge());
	}

	/**
	 * StreamCompletion runs the full completion flow — fetch+solve a fresh PoW
	 * challenge, POST the completion, and feed every reply-text delta to emit.
	 * The PoW challenge is short-lived, so a single automatic retry re-solves
	 * a fresh challenge when the first attempt fails with a transport error
	 * or an auth/pow-style HTTP error.
	 */
	async streamCompletion(req: CompletionRequest, emit: (text: string) => void): Promise<Reply> {
		let lastErr: unknown;
		for (let attempt = 0; attempt < 2; attempt++) {
			const pow = await this.powHeader();
			try {
				return await this.streamOnce(req, pow, emit);
			} catch (err) {
				lastErr = err;
				if (!retryable(err)) throw err;
			}
		}
		throw lastErr;
	}

	private async streamOnce(req: CompletionRequest, pow: string, emit: (text: string) => void): Promise<Reply> {
		const resp = await fetch(this.base + COMPLETION_PATH, {
			method: "POST",
			headers: { ...this.headers(), "x-ds-pow-response": pow },
			body: JSON.stringify(completionBody(req)),
			signal: this.completionSignal(),
		});
		if (!resp.ok) throw await httpStatusError(COMPLETION_PATH, resp);
		if (!resp.body) throw new Error("deepseek api error: empty completion body");

		const parser = new PatchParser();
		await readSSE(resp.body, (payload) => {
			if (sseDebugDump !== undefined) {
				try {
					appendFileSync(sseDebugDump, payload + "\n");
				} catch {
					// Best-effort debug dump.
				}
			}
			const err = parser.feed(payload, emit);
			if (err !== undefined) throw new Error(err);
		});
		return {
			messageId: parser.messageId ?? 0,
			sources: parser.sources,
			truncated: parser.truncated,
			filtered: parser.filtered,
		};
	}
}

/**
 * readSSE reads an SSE stream, joining each event's "data:" lines and calling
 * handle with the whole payload. "event:", "id:", "retry:" and ":" comment
 * lines are ignored, per the SSE spec.
 */
export async function readSSE(body: ReadableStream<Uint8Array>, handle: (payload: string) => void): Promise<void> {
	const decoder = new TextDecoder();
	let fields: string[] = [];
	let buf = "";
	const flush = () => {
		if (fields.length === 0) return;
		const payload = fields.join("\n");
		fields = [];
		handle(payload);
	};
	for await (const chunk of body as unknown as AsyncIterable<Uint8Array>) {
		buf += decoder.decode(chunk, { stream: true });
		let idx: number;
		while ((idx = buf.indexOf("\n")) >= 0) {
			const line = buf.slice(0, idx).replace(/\r$/, "");
			buf = buf.slice(idx + 1);
			if (line === "") {
				flush();
				continue;
			}
			if (line.startsWith("data:")) {
				// Per the SSE spec a single leading space is stripped.
				let content = line.slice("data:".length);
				if (content.startsWith(" ")) content = content.slice(1);
				fields.push(content);
			}
		}
	}
	buf += decoder.decode();
	if (buf !== "") {
		// A stream that ends without a trailing newline still flushes its
		// last line.
		const line = buf.replace(/\r$/, "");
		if (line.startsWith("data:")) {
			let content = line.slice("data:".length);
			if (content.startsWith(" ")) content = content.slice(1);
			fields.push(content);
		} else if (line !== "") {
			// Non-data trailing line: nothing to do.
		}
	}
	flush();
}

/** retryable: transport hiccups and anything resembling a challenge/rate-limit/auth rejection qualify for one retry. */
function retryable(err: unknown): boolean {
	if (err instanceof NoCredentialsError) return false;
	const msg = err instanceof Error ? err.message : String(err);
	if (
		msg.includes("pow challenge") ||
		msg.includes("challenge") ||
		msg.includes("HTTP 401") ||
		msg.includes("HTTP 403") ||
		msg.includes("HTTP 429")
	) {
		return true;
	}
	if (err instanceof Error && err.name === "TimeoutError") return true;
	if (err instanceof Error && err.name === "AbortError") return true;
	return false;
}

// Keep challengePrefix exported for callers that build prefixes directly.
export { challengePrefix };
