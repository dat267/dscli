/**
 * Pure constants of the interactive chat TUI, kept free of terminal
 * dependencies so the behaviour is unit-testable.
 */

/** pi's braille pulse (the working indicator's frames). */
export const SPINNER_FRAMES = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"] as const;

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
