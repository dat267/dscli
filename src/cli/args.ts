/**
 * CLI argument parsing and help display — hand-rolled, pi-style (no
 * commander/yargs): a declarative command table, a single parse loop, and a
 * help printer generated from the table.
 */
import { APP_DESCRIPTION, APP_NAME, VERSION } from "../config.js";

export type FlagType = "string" | "bool" | "number";

export interface FlagDef {
	name: string;
	short?: string;
	type: FlagType;
	/** Applied when the flag is absent AND its env var is unset. */
	default?: unknown;
	/** Environment variable consulted before the default (e.g. DS_TOKEN). */
	env?: string;
	help: string;
}

export interface CommandDef {
	name: string;
	help: string;
	flags?: FlagDef[];
	/** Positional arguments; multiple = variadic. */
	positional?: { name: string; multiple: boolean; help: string };
	/** Subcommands (session, config): the bare group also runs. */
	subcommands?: CommandDef[];
	/** The group command's own bare behaviour. */
	bare?: boolean;
}

// Shared flag groups.

const authFlags: FlagDef[] = [
	{ name: "token", type: "string", env: "DS_TOKEN", help: "DeepSeek user token (localStorage.userToken). Alternatively: config set token" },
	{ name: "cookie", type: "string", env: "DS_COOKIE", help: "DeepSeek ds_session_id cookie value. Alternatively: config set cookie" },
	{ name: "user-agent", type: "string", env: "DS_USER_AGENT", help: "Browser user-agent; some deployments reject non-browser UAs" },
];

const persistFlag: FlagDef = {
	name: "persist",
	type: "bool",
	help: "Persist and reuse the default session across runs, and save transcripts (default: ephemeral — the session is deleted when the run ends)",
};

const noTranscriptFlag: FlagDef = {
	name: "no-transcript",
	type: "bool",
	help: "Do not save session texts (transcripts) next to the config file",
};

const modelFlag: FlagDef = { name: "model", short: "m", type: "string", help: "Model: default (Instant) or expert" };

const thinkingFlag: FlagDef = {
	name: "thinking",
	short: "t",
	type: "bool",
	help: "Enable DeepThink reasoning",
};

const searchFlag: FlagDef = { name: "search", short: "s", type: "bool", help: "Enable web search" };

const chunkBytesFlag: FlagDef = {
	name: "chunk-bytes",
	type: "number",
	default: 0,
	help: "Upper bound on chunk size in bytes; 0 uses a 1 MiB cap. Chunks are sized adaptively to fit the model's output limit, so this is a maximum, not a fixed size",
};

const instructionsFlag = (what: string): FlagDef => ({
	name: "instructions",
	type: "string",
	help: `File with custom ${what} instructions for this run`,
});

const glossaryFlag: FlagDef = {
	name: "glossary",
	type: "string",
	help: "File with a project-specific name/term glossary (appended to every chunk prompt)",
};

const parallelFlag = (what: string): FlagDef => ({
	name: "parallel",
	short: "p",
	type: "bool",
	help: `${what} multiple files concurrently (each in its own session). No shared context between files — terminology may drift`,
});

const timeoutFlag = (what: string): FlagDef => ({
	name: "timeout",
	type: "string",
	default: "15m",
	help: `Overall budget ${what} (0 = no limit)`,
});

// The command table. Mirrors the Go CLI's structure exactly.

export const COMMANDS: CommandDef[] = [
	{
		name: "chat",
		help: "Chat with DeepSeek (omit the prompt for an interactive session)",
		positional: { name: "prompt", multiple: true, help: "Question to ask; omit to start an interactive session" },
		flags: [
			{ name: "conversation", short: "c", type: "string", help: "Continue an existing conversation (id printed at the end of each reply)" },
			{ ...modelFlag, help: "Model for a new thread: default (Instant) or expert. Cannot be combined with --conversation — a thread's model is fixed when it is created" },
			thinkingFlag,
			searchFlag,
			{ name: "json-out", type: "bool", help: 'Emit NDJSON: one {"delta":...} line per chunk, then a final {"done":true,"conversation_id":...} line' },
			timeoutFlag("for one question"),
			...authFlags,
			persistFlag,
			noTranscriptFlag,
			{ name: "workdir", type: "string", default: ".", help: "Working directory for /file loads" },
		],
	},
	{
		name: "ask",
		help: "Ask the model once and print the answer (input from args or stdin)",
		positional: { name: "prompt", multiple: true, help: "Input to send (omit to read from stdin; use -- before a prompt that starts with -)" },
		flags: [
			modelFlag,
			thinkingFlag,
			searchFlag,
			persistFlag,
			noTranscriptFlag,
			{ name: "json-out", type: "bool", help: 'Emit NDJSON: one {"delta":...} line per chunk, then a {"sources":[...]} line when search returned citations' },
			timeoutFlag(""),
			...authFlags,
		],
	},
	{
		name: "translate",
		help: "Translate a file (txt, md, lrc, srt, vtt, ass, ttml, epub) via the model",
		positional: { name: "file", multiple: true, help: "File(s) to translate (txt, md, lrc, srt, vtt, ass, ttml, epub); omit to read from stdin" },
		flags: [
			{ name: "from", type: "string", default: "auto", help: "Source language (defaults to auto-detect)" },
			{ name: "to", type: "string", default: "English", help: "Target language" },
			{ name: "output", short: "o", type: "string", help: "Output path (default: <input>.translated.<ext>, .txt for epub)" },
			{ name: "force", short: "f", type: "bool", help: "Overwrite the output file if it exists" },
			chunkBytesFlag,
			instructionsFlag("translation"),
			glossaryFlag,
			timeoutFlag("per file"),
			...authFlags,
			modelFlag,
			{ ...thinkingFlag, help: "Enable DeepThink reasoning for each chunk (the reasoning model allows longer replies, so chunks are sized bigger and fewer are needed)" },
			parallelFlag("Translate"),
			persistFlag,
		],
	},
	{
		name: "improve-writing",
		help: "Improve the writing of a file in place (txt, md, lrc, srt, vtt, ass, ttml) via the model",
		positional: { name: "file", multiple: true, help: "File(s) to improve (txt, md, lrc, srt, vtt, ass, ttml); omit to read from stdin" },
		flags: [
			{ name: "in-place", short: "i", type: "bool", help: "Rewrite each file in place with the improved text. Required: improve-writing replaces the original instead of writing a separate output file" },
			chunkBytesFlag,
			instructionsFlag("improvement"),
			glossaryFlag,
			timeoutFlag("per file"),
			...authFlags,
			modelFlag,
			{ ...thinkingFlag, help: "Enable DeepThink reasoning for each chunk (the reasoning model allows longer replies, so chunks are sized bigger and fewer are needed)" },
			parallelFlag("Improve"),
			persistFlag,
		],
	},
	{
		name: "summarize",
		help: "Summarize a file (txt, md, lrc, srt, vtt, ass, ttml, epub) via the model",
		positional: { name: "file", multiple: true, help: "File(s) to summarize (txt, md, lrc, srt, vtt, ass, ttml, epub); omit to read from stdin" },
		flags: [
			{ name: "output", short: "o", type: "string", help: "Output path (default: print to stdout)" },
			{ name: "force", short: "f", type: "bool", help: "Overwrite the output file if it exists" },
			chunkBytesFlag,
			instructionsFlag("summarization"),
			timeoutFlag("per file"),
			...authFlags,
			modelFlag,
			{ ...thinkingFlag, help: "Enable DeepThink reasoning for each chunk (the reasoning model allows longer replies, so chunks are sized bigger and fewer are needed)" },
			parallelFlag("Summarize"),
			persistFlag,
		],
	},
	{
		name: "session",
		help: "Inspect, forget or delete the persisted default session",
		bare: true,
		subcommands: [
			{ name: "list", help: "List sessions with saved texts" },
			{ name: "select", help: "Select a session to resume as the default", positional: { name: "id", multiple: false, help: "Session id (see `session list`)" } },
			{
				name: "transcript",
				help: "Print or delete the saved session texts (transcript) for a session",
				positional: { name: "id", multiple: false, help: "Session id (see `session list`)" },
				flags: [{ name: "delete", type: "bool", help: "Delete the transcript file instead of printing it" }],
			},
			{ name: "delete", help: "Delete the persisted default session server-side and forget it" },
			{ name: "forget", help: "Forget the persisted default session (the thread is kept server-side)" },
		],
	},
	{ name: "login", help: "Show how to capture your DeepSeek login (token + cookie)" },
	{ name: "version", help: "Show version" },
	{
		name: "config",
		help: "Manage application configuration",
		bare: false,
		subcommands: [
			{ name: "init", help: "Generate a default configuration file", flags: [{ name: "overwrite", type: "bool", help: "Overwrite existing configuration file" }] },
			{ name: "path", help: "Show configuration file path" },
			{ name: "set", help: "Set a config value", positional: { name: "key value", multiple: true, help: "Configuration key (dot-notation for nested, e.g. core.timeout) and value" } },
			{ name: "unset", help: "Unset a config value", positional: { name: "key", multiple: false, help: "Configuration key to unset" } },
		],
	},
];

export interface ParsedArgs {
	/** The command name (first positional). */
	command?: string;
	/** The subcommand name for group commands (session/config). */
	sub?: string;
	/** Flag values by long name (dashes kept), with env/default applied. */
	flags: Record<string, unknown>;
	/** Positional arguments after the (sub)command name. */
	positionals: string[];
	/** Explicit --config-file, when present. */
	configFile?: string;
	help?: boolean;
	version?: boolean;
	/** Unknown flags/commands encountered (surfaced as usage errors). */
	errors: string[];
}

function findCommand(name: string): CommandDef | undefined {
	return COMMANDS.find((c) => c.name === name);
}

function flagNames(f: FlagDef): string[] {
	const out = [`--${f.name}`];
	if (f.short) out.push(`-${f.short}`);
	return out;
}

/**
 * parseArgs walks argv the pi way: one loop, --flag value / --flag=value /
 * -f value / -fvalue, "--" ends flags, and the first bare token selects the
 * command (the next one a subcommand for group commands). Env vars and
 * defaults are applied for the selected command's flags only.
 */
export function parseArgs(argv: readonly string[]): ParsedArgs {
	const result: ParsedArgs = { flags: {}, positionals: [], errors: [] };

	// Collect bare positionals (command tokens) so the command is known
	// before flags are resolved — flags may precede or follow it.
	const bareTokens: string[] = [];
	let noMoreFlags = false;
	for (let i = 0; i < argv.length; i++) {
		const arg = argv[i]!;
		if (noMoreFlags) { bareTokens.push(arg); continue; }
		if (arg === "--") { noMoreFlags = true; continue; }
		if (arg === "--help" || arg === "-h") { result.help = true; continue; }
		if (arg === "--version" || arg === "-v") { result.version = true; continue; }
		if (isFlagish(arg)) continue; // flags are resolved in the second pass
		bareTokens.push(arg);
	}
	// Resolve the command and subcommand from the leading bare tokens.
	let command: string | undefined;
	let sub: string | undefined;
	let skip = 0;
	if (bareTokens.length > 0 && findCommand(bareTokens[0]!)) {
		command = bareTokens[0]!;
		skip = 1;
		const cmd = findCommand(command)!;
		if (cmd.subcommands && bareTokens.length > 1 && cmd.subcommands.some((sc) => sc.name === bareTokens[1])) {
			sub = bareTokens[1]!;
			skip = 2;
		}
	}

	// The active flag set: the subcommand's flags extend the group's.
	const cmd = command ? findCommand(command) : undefined;
	const active: FlagDef[] = [];
	if (cmd?.subcommands && sub) {
		const subDef = cmd.subcommands.find((sc) => sc.name === sub)!;
		active.push(...(cmd.flags ?? []), ...(subDef.flags ?? []));
	} else if (cmd) {
		active.push(...(cmd.flags ?? []));
	}
	// --config-file is global.
	active.push({ name: "config-file", type: "string", help: "Config file path" });
	const byName = new Map<string, FlagDef>();
	for (const f of active) {
		byName.set(`--${f.name}`, f);
		if (f.short) byName.set(`-${f.short}`, f);
	}

	// Second pass: resolve flags against the active table.
	const flags: Record<string, unknown> = {};
	const seen = new Set<string>();
	const positionals: string[] = [];
	noMoreFlags = false;
	for (let i = 0; i < argv.length; i++) {
		const arg = argv[i]!;
		if (noMoreFlags) { positionals.push(arg); continue; }
		if (arg === "--") { noMoreFlags = true; continue; }
		if (arg === "--help" || arg === "-h" || arg === "--version" || arg === "-v") continue;
		if (arg.startsWith("--")) {
			const body = arg.slice(2);
			const eq = body.indexOf("=");
			const name = eq >= 0 ? body.slice(0, eq) : body;
			const def = byName.get(`--${name}`);
			if (!def) {
				result.errors.push(`unknown flag --${name}`);
				continue;
			}
			if (def.type === "bool") {
				flags[def.name] = eq >= 0 ? body.slice(eq + 1) !== "false" : true;
			} else {
				let value: string;
				if (eq >= 0) value = body.slice(eq + 1);
				else if (i + 1 < argv.length && !isFlagish(argv[i + 1]!) && argv[i + 1] !== "--") {
					value = argv[i + 1]!;
					i++;
				} else {
					result.errors.push(`--${name} requires a value`);
					continue;
				}
				flags[def.name] = def.type === "number" ? Number(value) : value;
			}
			seen.add(def.name);
			continue;
		}
		if (isFlagish(arg)) {
			const short = arg.slice(1, 2);
			const rest = arg.slice(2);
			const def = byName.get(`-${short}`);
			if (!def) {
				result.errors.push(`unknown flag -${short}`);
				continue;
			}
			if (def.type === "bool") {
				if (rest === "") {
					flags[def.name] = true;
				} else {
					// -abc: packed short bools.
					for (const ch of arg.slice(1)) {
						const d2 = byName.get(`-${ch}`);
						if (!d2 || d2.type !== "bool") {
							result.errors.push(d2 ? `-${ch} requires a value` : `unknown flag -${ch}`);
							break;
						}
						flags[d2.name] = true;
						seen.add(d2.name);
					}
				}
			} else {
				let value: string;
				if (rest !== "") value = rest;
				else if (i + 1 < argv.length && !isFlagish(argv[i + 1]!) && argv[i + 1] !== "--") {
					value = argv[i + 1]!;
					i++;
				} else {
					result.errors.push(`-${short} requires a value`);
					continue;
				}
				flags[def.name] = def.type === "number" ? Number(value) : value;
			}
			seen.add(def.name);
			continue;
		}
		positionals.push(arg);
	}

	// Drop the command/subcommand tokens consumed above.
	result.command = command;
	result.sub = sub;
	result.positionals = positionals.slice(skip);

	// Env vars and defaults for flags not given on the command line.
	for (const f of active) {
		if (seen.has(f.name)) continue;
		if (f.env && process.env[f.env] !== undefined) flags[f.name] = process.env[f.env];
		else if (f.default !== undefined) flags[f.name] = f.default;
		else flags[f.name] = f.type === "bool" ? false : "";
	}

	result.flags = flags;
	const cfg = flags["config-file"];
	if (typeof cfg === "string" && cfg !== "") result.configFile = cfg;

	if (!command && result.errors.length === 0 && positionals.length > 0) {
		result.errors.push(`unknown command ${positionals[0]}`);
	}
	return result;
}

function isFlagish(s: string): boolean {
	return s.startsWith("-") && s !== "-";
}

/** printHelp renders the compact top-level help from the command table. */
export function printHelp(out: NodeJS.WritableStream = process.stdout): void {
	const w = (s = "") => out.write(s + "\n");
	w(`Usage: ${APP_NAME} [--config-file FILE] <command> [flags]`);
	w();
	w(APP_DESCRIPTION);
	w();
	w("Commands:");
	const width = Math.max(...COMMANDS.map((c) => c.name.length + 2));
	for (const c of COMMANDS) {
		w(`  ${c.name.padEnd(width)}${c.help}`);
	}
	for (const c of COMMANDS) {
		if (!c.subcommands) continue;
		w();
		w(`  ${c.name} <command>`);
		const sw = Math.max(...c.subcommands.map((s) => s.name.length + 2));
		for (const s of c.subcommands) {
			w(`    ${s.name.padEnd(sw)}${s.help}`);
		}
	}
	w();
	w(`Use "${APP_NAME} <command> --help" for command flags.`);
	w(`${APP_NAME} version ${VERSION}`);
}
