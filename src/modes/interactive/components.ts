/**
 * The chat TUI's message and chrome components, mirroring pi's
 * modes/interactive/components: user messages in a background box,
 * a DynamicBorder that embeds the working indicator, and a two-sided
 * footer.
 */
import { Box, Markdown, truncateToWidth, visibleWidth } from "@earendil-works/pi-tui";
import type { Component } from "@earendil-works/pi-tui";
import { getMarkdownTheme, theme } from "./theme.js";
import { SPINNER_FRAMES } from "./parts.js";

/**
 * UserMessageComponent: the typed prompt in a padded background box
 * (pi's userMessageBg), rendered as markdown in the message text colour.
 */
export class UserMessageComponent implements Component {
	private readonly box: Box;

	constructor(text: string) {
		this.box = new Box(1, 1, (content) => theme.bg("userMsgBg", content));
		this.box.addChild(new Markdown(text, 0, 0, getMarkdownTheme(), {
			color: (content: string) => theme.fg("userMessageText", content),
		} as never));
	}

	render(width: number): string[] {
		return this.box.render(width);
	}

	invalidate(): void {
		this.box.invalidate();
	}
}

/** AssistantMessageComponent: markdown in the default foreground. */
export class AssistantMessageComponent implements Component {
	private readonly md: Markdown;

	constructor(text: string) {
		this.md = new Markdown(text, 0, 0, getMarkdownTheme());
	}

	setText(text: string): void {
		this.md.setText(text);
	}

	render(width: number): string[] {
		return this.md.render(width);
	}

	invalidate(): void {
		this.md.invalidate();
	}
}

/** NoteComponent: dimmed system text (slash feedback, hints). */
export class NoteComponent implements Component {
	private readonly lines: string[];

	constructor(text: string) {
		this.lines = text.split("\n").map((l) => theme.fg("muted", l));
	}

	render(width: number): string[] {
		return this.lines.map((l) => truncateToWidth(l, width, ""));
	}

	invalidate(): void {}
}

/**
 * DynamicBorder: the rule between the chat pane and the editor. While a
 * turn runs it embeds the working indicator — pi-style
 * "─ ⠋ Working ───" with the braille loader in accent — so streaming never
 * changes the pane layout; idle it is the plain border-coloured rule.
 */
export class WorkingBorder implements Component {
	private busy = false;
	private frame = 0;

	setBusy(busy: boolean): void {
		this.busy = busy;
	}

	tick(): void {
		this.frame = (this.frame + 1) % SPINNER_FRAMES.length;
	}

	render(width: number): string[] {
		if (width <= 0) return [""];
		if (!this.busy) return [theme.fg("border", "─".repeat(width))];
		const spinner = SPINNER_FRAMES[this.frame]!;
		const prefix = `─ ${spinner} Working `;
		const fill = Math.max(0, width - visibleWidth(prefix));
		return [
			theme.fg("border", "─ ") +
				theme.fg("accent", spinner) +
				theme.fg("muted", " Working ") +
				theme.fg("border", "─".repeat(fill)),
		];
	}

	invalidate(): void {}
}

export interface FooterParams {
	model: string;
	thinking: boolean;
	search: boolean;
	mode: string;
	turns: number;
	conversation: string;
	cwd: string;
}

/** formatCwd: replace the home directory with ~. */
export function formatCwd(cwd: string, home: string | undefined): string {
	if (!home) return cwd;
	if (cwd === home) return "~";
	if (cwd.startsWith(home + "/")) return "~" + cwd.slice(home.length);
	return cwd;
}

/**
 * FooterComponent: pi's two-sided footer — left side the run stats, right
 * side the model (right-aligned, truncated when the terminal is narrow).
 * DeepSeek's web API exposes no token accounting, so the left side carries
 * the durable session facts instead.
 */
export class FooterComponent implements Component {
	private params: FooterParams;

	constructor(params: FooterParams) {
		this.params = params;
	}

	setParams(params: FooterParams): void {
		this.params = params;
	}

	render(width: number): string[] {
		let conv = this.params.conversation.split(":")[0] ?? "";
		if (conv.length > 8) conv = conv.slice(0, 8) + "…";
		const left = [
			this.params.cwd,
			`${this.params.turns} turns`,
			`${this.params.mode}${conv !== "" ? ` · ${conv}` : ""}`,
		].join(" · ");
		const right = [
			this.params.model,
			`thinking ${this.params.thinking ? "on" : "off"}`,
			`search ${this.params.search ? "on" : "off"}`,
		].join(" · ");

		let leftW = visibleWidth(left);
		if (leftW > width) {
			const t = truncateToWidth(left, width, "...");
			return [t];
		}
		const minPadding = 2;
		const rightW = visibleWidth(right);
		if (leftW + minPadding + rightW > width) {
			const available = width - leftW - minPadding;
			if (available > 0) {
				const truncatedRight = truncateToWidth(right, available, "");
				const pad = " ".repeat(Math.max(0, width - leftW - visibleWidth(truncatedRight)));
				return [theme.fg("muted", left) + pad + theme.fg("muted", truncatedRight)];
			}
			return [theme.fg("muted", left)];
		}
		const padding = " ".repeat(width - leftW - rightW);
		return [theme.fg("muted", left) + padding + theme.fg("muted", right)];
	}

	invalidate(): void {}
}
