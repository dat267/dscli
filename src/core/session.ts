/**
 * The persisted default conversation and stale-session recovery.
 *
 * Port of cmd/session.go: the config's `session` key holds
 * "<session_id>[:<message_id>]" (the message id is the parent for the next
 * turn). Unless a run passes --persist, nothing is reused or saved: a fresh
 * session is created and deleted server-side at the end.
 */
import { stderrNote } from "../ui/notes.js";
import { loadConfigMap, saveConfigMap, SESSION_KEY } from "../config.js";
import type { DeepSeekClient } from "./deepseek/client.js";

/** splitConversation parses "<session_id>[:<message_id>]" into its parts; an empty session part means "start new". */
export function splitConversation(id: string): { sessionId: string; parentId: number | null } {
	if (id === "") return { sessionId: "", parentId: null };
	const colon = id.indexOf(":");
	if (colon < 0) return { sessionId: id, parentId: null };
	const session = id.slice(0, colon);
	const after = id.slice(colon + 1);
	if (session === "") return { sessionId: "", parentId: null };
	const n = Number(after);
	if (after !== "" && Number.isInteger(n) && Number.isFinite(n)) return { sessionId: session, parentId: n };
	return { sessionId: session, parentId: null };
}

/**
 * conversationID renders the id to pass on the NEXT turn: the freshly
 * produced assistant message id when available, else the id used to ask,
 * else the bare session.
 */
export function conversationID(sessionId: string, parentId: number | null, msgId: number): string {
	if (msgId !== 0) return `${sessionId}:${msgId}`;
	if (parentId !== null) return `${sessionId}:${parentId}`;
	return sessionId;
}

/** Maps an empty --model to the site's fast default. */
export function effectiveModel(m: string): string {
	return m === "" ? "default" : m;
}

/** loadSavedSession returns the persisted default conversation id, or "" when none is saved. */
export function loadSavedSession(cfgPath: string): string {
	if (cfgPath === "") return "";
	let m: Record<string, unknown>;
	try {
		m = loadConfigMap(cfgPath);
	} catch {
		return "";
	}
	const s = m[SESSION_KEY];
	return typeof s === "string" ? s : "";
}

/** saveSession persists the default conversation id, preserving every other config key. */
export function saveSession(cfgPath: string, sessionId: string): Error | undefined {
	if (cfgPath === "") return undefined;
	let m: Record<string, unknown>;
	try {
		m = loadConfigMap(cfgPath);
	} catch (err) {
		return err instanceof Error ? err : new Error(String(err));
	}
	m[SESSION_KEY] = sessionId;
	try {
		saveConfigMap(cfgPath, m);
	} catch (err) {
		return err instanceof Error ? err : new Error(String(err));
	}
	return undefined;
}

/** clearSession removes the persisted default conversation key. */
export function clearSession(cfgPath: string): Error | undefined {
	if (cfgPath === "") return undefined;
	let m: Record<string, unknown>;
	try {
		m = loadConfigMap(cfgPath);
	} catch (err) {
		return err instanceof Error ? err : new Error(String(err));
	}
	if (!(SESSION_KEY in m)) return undefined;
	delete m[SESSION_KEY];
	try {
		saveConfigMap(cfgPath, m);
	} catch (err) {
		return err instanceof Error ? err : new Error(String(err));
	}
	return undefined;
}

export interface ResolvedSession {
	sessionId: string;
	/** True when the id came from the config (eligible for stale-session recovery). */
	trusted: boolean;
	/** Deletes the ephemeral session server-side at the end of the run. */
	cleanup: (() => Promise<void>) | undefined;
}

/**
 * resolveDefaultSession returns the session a command should run in.
 * Without --persist nothing is reused or saved: a fresh session is created
 * and the returned cleanup deletes it when the run ends. With --persist the
 * saved default conversation (if any) is resumed; the first persisted run
 * creates and saves one.
 */
export async function resolveDefaultSession(
	client: DeepSeekClient,
	cfgPath: string,
	persist: boolean,
): Promise<ResolvedSession> {
	if (!persist) {
		const sid = await client.createChatSession();
		return {
			sessionId: sid,
			trusted: false,
			cleanup: async () => {
				try {
					await client.deleteSessions([sid]);
				} catch (err) {
					stderrNote(`warning: failed to delete session: ${err instanceof Error ? err.message : err}\n`);
				}
			},
		};
	}
	const saved = loadSavedSession(cfgPath);
	if (saved !== "") return { sessionId: saved, trusted: true, cleanup: undefined };
	const sid = await client.createChatSession();
	const err = saveSession(cfgPath, sid);
	if (err) throw err;
	return { sessionId: sid, trusted: false, cleanup: undefined };
}

/**
 * persistConversation saves the advanced conversation position
 * (session:message) after a successful turn, but only for explicitly
 * persisted runs — ephemeral runs leave nothing behind.
 */
export function persistConversation(cfgPath: string, persist: boolean, convId: string): void {
	if (!persist || cfgPath === "" || convId === "") return;
	const err = saveSession(cfgPath, convId);
	if (err) stderrNote(`warning: could not save conversation position: ${err.message}\n`);
}

/**
 * advanceConversation returns the conversation id to persist after a turn in
 * session that produced msgID: the bare session part with the message tail
 * replaced.
 */
export function advanceConversation(session: string, msgId: number): string {
	const { sessionId } = splitConversation(session);
	return conversationID(sessionId, null, msgId);
}

/**
 * recoverStaleSession runs run once. If it fails and the session was a
 * trusted persisted one (loaded from the config), a fresh session is
 * created, saved as the new default, and run re-executed once with it —
 * covering the case where the saved session no longer exists server-side.
 * Returns the id the successful run used.
 */
export async function recoverStaleSession(
	client: DeepSeekClient,
	cfgPath: string,
	sessionId: string,
	trusted: boolean,
	run: (sid: string) => Promise<void>,
): Promise<{ used: string; err?: Error }> {
	let firstErr: Error | undefined;
	try {
		await run(sessionId);
		return { used: sessionId };
	} catch (err) {
		firstErr = err instanceof Error ? err : new Error(String(err));
	}
	if (!trusted) return { used: sessionId, err: firstErr };
	const sid = await client.createChatSession().catch(() => null);
	if (sid === null) return { used: sessionId, err: firstErr };
	if (!saveSession(cfgPath, sid)) {
		try {
			await run(sid);
			stderrNote("note: the persisted conversation no longer exists server-side; started a fresh one\n");
			return { used: sid };
		} catch {
			// fall through: report the original failure
		}
	}
	return { used: sessionId, err: firstErr };
}
