/**
 * Pure rendering parts of the interactive chat TUI — the working-indicator
 * rule and the status line. Kept free of terminal dependencies so the
 * behaviour is unit-testable (the Go TUI's TestTUISpinner contract).
 */

/** pi's braille pulse (the working indicator's frames). */
export const SPINNER_FRAMES = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"] as const;

export const ANSI_DIM = "\x1b[2m";
export const ANSI_MUTED = "\x1b[38;2;133;131;146m"; // Squid grey
export const ANSI_ACCENT = "\x1b[38;2;18;199;143m"; // Guac mint
export const ANSI_RESET = "\x1b[0m";

export interface RuleColors {
	dim(s: string): string;
	accent(s: string): string;
	muted(s: string): string;
}

/**
 * ruleText: while a turn runs the separator rule doubles as the working
 * indicator — pi-style "── ⠋ Working ───" — so streaming never changes the
 * pane layout; idle views show the plain dim rule.
 */
export function ruleText(busy: boolean, frame: number, width: number, colors?: RuleColors): string {
	if (width <= 0) return "";
	const w = (s: string) => (colors ? colors.dim(s) : s);
	if (!busy) return w("─".repeat(width));
	const f = SPINNER_FRAMES[frame % SPINNER_FRAMES.length]!;
	const head = `── ${f} Working `;
	const fill = Math.max(0, width - head.length);
	if (colors) {
		return (
			colors.dim("──") +
			" " +
			colors.accent(f) +
			" " +
			colors.muted("Working") +
			" " +
			colors.dim("─".repeat(fill))
		);
	}
	return head + "─".repeat(fill);
}

export interface StatusParams {
	model: string;
	thinking: boolean;
	search: boolean;
	/** "ephemeral" | "persisted" | "continuing" */
	mode: string;
	turn: number;
	/** The conversation id; shortened to 8 chars + ellipsis in display. */
	conversation: string;
}

/** onoff renders a boolean as "on"/"off". */
export function onoff(v: boolean): string {
	return v ? "on" : "off";
}

/** statusLine: the modes sit before turn/session so narrow terminals truncate the tail, not the state. */
export function statusLine(p: StatusParams): string {
	let conv = "";
	if (p.conversation !== "") {
		const sess = p.conversation.split(":")[0] ?? "";
		conv = sess.length > 8 ? sess.slice(0, 8) + "…" : sess;
	}
	const base =
		`DeepSeek · model ${p.model} · thinking ${onoff(p.thinking)} · search ${onoff(p.search)}` +
		` · ${p.mode} · turn ${p.turn}`;
	return conv !== "" ? `${base} · ${conv}` : base;
}

/** The slash commands the TUI understands. */
export const TUI_COMMANDS = [
	"/exit",
	"/quit",
	"/new",
	"/help",
	"/model",
	"/thinking",
	"/search",
	"/clear",
	"/resume",
	"/session",
	"/sessions",
] as const;
