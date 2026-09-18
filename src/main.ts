/**
 * Main entry point: dispatches parsed CLI arguments to commands, pi-style
 * (cli/setup.ts sets process identity, main() translates args into command
 * options). Missing commands (chat TUI, translate family) land in later
 * rounds; asking for them gives a clear "not yet ported" error.
 */
import process from "node:process";
import { findConfigFileFlag, resolveConfigPath, configOverrideWarning, VERSION, loadConfigMap } from "./config.js";
import { parseArgs, printHelp } from "./cli/args.js";
import { askCommand } from "./cli/commands/ask.js";
import { configCommand, warnOnConfigOverride } from "./cli/commands/config.js";
import { loginCommand, versionCommand } from "./cli/commands/login.js";
import { sessionCommand } from "./cli/commands/session.js";
import { fileCommand, parseDurationMs as parseFileDuration } from "./cli/commands/files.js";
import { stderrNote } from "./ui/notes.js";

/** setupCli mirrors pi's cli/setup.js: process identity and env markers. */
export function setupCli(): void {
	process.title = "dscli";
}

/** parseDuration accepts Go-style durations ("15m", "90s", "1h30m", "0"). */
export function parseDuration(s: string): number {
	if (s === "" || s === "0") return 0;
	let total = 0;
	let rest = s;
	const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/y;
	while (rest !== "") {
		const m = re.exec(rest);
		if (!m) throw new Error(`invalid duration ${s}`);
		const n = Number(m[1]!);
		switch (m[2]) {
			case "ms": total += n; break;
			case "s": total += n * 1000; break;
			case "m": total += n * 60_000; break;
			case "h": total += n * 3_600_000; break;
			case "us":
			case "µs": total += n / 1000; break;
			case "ns": total += n / 1e6; break;
		}
		rest = rest.slice(m[0].length);
	}
	return total;
}

/** Config-resolved credentials: flags first, then the config file, then env (env is applied in parseArgs). */
function resolveCredentials(flags: Record<string, unknown>, cfgPath: string): { token: string; cookie: string; userAgent: string } {
	const cfg = loadConfigMap(cfgPath);
	const pick = (flag: string, key: string): string => {
		const f = flags[flag];
		if (typeof f === "string" && f !== "") return f;
		const c = cfg[key];
		if (typeof c === "string" && c !== "") return c;
		return typeof f === "string" ? f : "";
	};
	return {
		token: pick("token", "token"),
		cookie: pick("cookie", "cookie"),
		userAgent: pick("user-agent", "user-agent"),
	};
}

export async function main(argv: readonly string[]): Promise<number> {
	const explicit = findConfigFileFlag(argv);
	const cfgPath = resolveConfigPath(explicit);
	warnOnConfigOverride(cfgPath, explicit === undefined);

	const parsed = parseArgs(argv);
	if (parsed.errors.length > 0) {
		for (const err of parsed.errors) stderrNote(`error: ${err}\n`);
		printHelp(process.stderr);
		return 1;
	}
	if (parsed.version) {
		versionCommand();
		return 0;
	}
	if (parsed.help || !parsed.command) {
		printHelp();
		return 0;
	}

	const flags = parsed.flags;
	const creds = resolveCredentials(flags, cfgPath);

	try {
		switch (parsed.command) {
			case "version":
				versionCommand();
				return 0;
			case "login":
				loginCommand();
				return 0;
			case "config":
				await configCommand(parsed.sub, {
					cfgPath,
					key: parsed.positionals[0],
					value: parsed.positionals[1],
					overwrite: flags["overwrite"] === true,
				});
				return 0;
			case "session":
				await sessionCommand(parsed.sub, {
					cfgPath,
					session: parsed.positionals[0],
					delete: flags["delete"] === true,
					...creds,
				});
				return 0;
			case "ask":
				await askCommand({
					cfgPath,
					prompt: parsed.positionals,
					model: String(flags["model"] ?? ""),
					thinking: flags["thinking"] === true,
					search: flags["search"] === true,
					persist: flags["persist"] === true,
					noTranscript: flags["no-transcript"] === true,
					jsonOut: flags["json-out"] === true,
					timeoutMs: parseDuration(String(flags["timeout"] ?? "0")),
					...creds,
				});
				return 0;
			case "translate":
				await fileCommand("translate", {
					cfgPath,
					files: parsed.positionals,
					from: String(flags["from"] ?? "auto"),
					to: String(flags["to"] ?? "English"),
					output: String(flags["output"] ?? ""),
					force: flags["force"] === true,
					inPlace: false,
					chunkBytes: Number(flags["chunk-bytes"] ?? 0),
					instructions: String(flags["instructions"] ?? ""),
					glossary: String(flags["glossary"] ?? ""),
					timeoutMs: parseFileDuration(String(flags["timeout"] ?? "0")),
					model: String(flags["model"] ?? ""),
					thinking: flags["thinking"] === true,
					parallel: flags["parallel"] === true,
					persist: flags["persist"] === true,
					...creds,
				});
				return 0;
			case "improve-writing":
				await fileCommand("improve", {
					cfgPath,
					files: parsed.positionals,
					from: "",
					to: "",
					output: "",
					force: true,
					inPlace: true,
					chunkBytes: Number(flags["chunk-bytes"] ?? 0),
					instructions: String(flags["instructions"] ?? ""),
					glossary: String(flags["glossary"] ?? ""),
					timeoutMs: parseFileDuration(String(flags["timeout"] ?? "0")),
					model: String(flags["model"] ?? ""),
					thinking: flags["thinking"] === true,
					parallel: flags["parallel"] === true,
					persist: flags["persist"] === true,
					...creds,
				});
				return 0;
			case "summarize":
				await fileCommand("summarize", {
					cfgPath,
					files: parsed.positionals,
					from: "",
					to: "",
					output: String(flags["output"] ?? ""),
					force: flags["force"] === true,
					inPlace: false,
					chunkBytes: Number(flags["chunk-bytes"] ?? 0),
					instructions: String(flags["instructions"] ?? ""),
					glossary: "",
					timeoutMs: parseFileDuration(String(flags["timeout"] ?? "0")),
					model: String(flags["model"] ?? ""),
					thinking: flags["thinking"] === true,
					parallel: flags["parallel"] === true,
					persist: flags["persist"] === true,
					...creds,
				});
				return 0;
			case "chat":
				stderrNote(`'chat' is not yet ported to the Node.js rewrite (in progress)\n`);
				return 2;
			default:
				stderrNote(`unknown command ${parsed.command}\n`);
				printHelp(process.stderr);
				return 1;
		}
	} catch (err) {
		stderrNote(`error: ${err instanceof Error ? err.message : String(err)}\n`);
		return 1;
	}
}
