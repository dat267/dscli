/**
 * The TUI theme — pi's dark palette (themes/dark.json), with fg/bg helpers
 * shaped like pi's theme object.
 */
import chalk from "chalk";

export const PALETTE = {
	cyan: "#00d7ff",
	blue: "#5f87ff",
	green: "#b5bd68",
	red: "#cc6666",
	yellow: "#ffff00",
	text: "#d4d4d4",
	gray: "#808080",
	dimGray: "#666666",
	darkGray: "#505050",
	accent: "#8abeb7",
	selectedBg: "#3a3a4a",
	userMsgBg: "#343541",
} as const;

export const theme = {
	fg(name: keyof typeof COLORS, s: string): string {
		return chalk.hex(COLORS[name])(s);
	},
	bg(name: "userMsgBg", s: string): string {
		return chalk.bgHex(PALETTE[name])(s);
	},
	bold(s: string): string {
		return chalk.bold(s);
	},
};

const COLORS = {
	accent: PALETTE.accent,
	border: PALETTE.blue,
	borderAccent: PALETTE.cyan,
	success: PALETTE.green,
	error: PALETTE.red,
	warning: PALETTE.yellow,
	muted: PALETTE.gray,
	dim: PALETTE.dimGray,
	text: PALETTE.text,
	userMessageText: PALETTE.text,
} as const;

export type FgName = keyof typeof COLORS;

/** The Markdown theme for pi-tui's Markdown component, from the same palette. */
export function getMarkdownTheme() {
	return {
		heading: (t: string) => chalk.bold.hex(PALETTE.accent)(t),
		link: (t: string) => chalk.hex(PALETTE.cyan)(t),
		linkUrl: (t: string) => chalk.dim(t),
		code: (t: string) => chalk.hex(PALETTE.green)(t),
		codeBlock: (t: string) => t,
		codeBlockBorder: (t: string) => chalk.hex(PALETTE.darkGray)(t),
		quote: (t: string) => t,
		quoteBorder: (t: string) => chalk.hex(PALETTE.blue)(t),
		hr: (t: string) => chalk.hex(PALETTE.darkGray)(t),
		listBullet: (t: string) => chalk.hex(PALETTE.accent)(t),
		bold: (t: string) => chalk.bold(t),
		italic: (t: string) => chalk.italic(t),
		strikethrough: (t: string) => chalk.strikethrough(t),
		underline: (t: string) => chalk.underline(t),
	};
}
