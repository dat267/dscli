/**
 * Application identity, config path resolution and the config store.
 *
 * Mirrors the Go version: the config is a JSON object at a resolved path
 * (~/.config/dscli/dscli.json, a ./dscli.json override, or DSCLI_CONFIG_FILE),
 * written with 0600 permissions. Structured like pi's src/config.ts (single
 * source for APP_NAME and package-relative assets).
 */
import { chmodSync, existsSync, mkdirSync, readFileSync, renameSync, statSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { homedir } from "node:os";
import { fileURLToPath } from "node:url";

export const APP_NAME = "dscli";
export const APP_DESCRIPTION = "DeepSeek chat from your terminal";
export const VERSION = "0.1.0";

export const CONFIG_FILE_ENV = "DSCLI_CONFIG_FILE";
const CONFIG_FLAG = "--config-file";

/** The config key holding the persisted default conversation (session[:message]). */
export const SESSION_KEY = "session";
export const TRANSCRIPTS_DIR = "transcripts";

/**
 * Find an explicit --config-file flag without a full parse, so the config
 * path is known before any command runs (same trick as the Go runtime).
 */
export function findConfigFileFlag(argv: readonly string[]): string | undefined {
	for (let i = 0; i < argv.length; i++) {
		const arg = argv[i]!;
		if (arg === CONFIG_FLAG) return argv[i + 1]; // possibly undefined: a value-less flag
		if (arg.startsWith(CONFIG_FLAG + "=")) return arg.slice(CONFIG_FLAG.length + 1);
	}
	return undefined;
}

/**
 * Resolve the active config file path: $DSCLI_CONFIG_FILE, then a ./dscli.json
 * planted in the current directory, then the user config dir.
 */
export function resolveConfigPath(explicit?: string): string {
	if (explicit) return explicit;
	const env = process.env[CONFIG_FILE_ENV];
	if (env) return env;
	const local = `${APP_NAME}.json`;
	if (existsSync(local)) return local;
	return join(homedir(), ".config", APP_NAME, `${APP_NAME}.json`);
}

/** True when the config path was auto-selected from a planted ./dscli.json. */
export function configOverrideWarning(active: string, autoResolved: boolean): string {
	if (!autoResolved || active !== `${APP_NAME}.json` || !existsSync(active)) return "";
	return `warning: using config file "${active}" from the current directory\n`;
}

/** Config is a plain JSON object; values are read with dot-notation keys. */
export type ConfigMap = Record<string, unknown>;

/** Read the config file (missing file = empty config), preserving key order. */
export function loadConfigMap(path: string): ConfigMap {
	let raw: string;
	try {
		raw = readFileSync(path, "utf8");
	} catch {
		return {};
	}
	try {
		const parsed = JSON.parse(raw);
		if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) return parsed;
		return {};
	} catch (err) {
		throw new Error(`failed to parse configuration file ${path}: ${err instanceof Error ? err.message : err}`);
	}
}

/** Write the config with 0600 permissions, creating the directory on demand. */
export function saveConfigMap(path: string, cfg: ConfigMap): void {
	mkdirSync(dirname(path), { recursive: true });
	const tmp = `${path}.tmp`;
	writeFileSync(tmp, JSON.stringify(cfg, null, 2) + "\n", { mode: 0o600 });
	chmodSync(tmp, 0o600);
	renameSync(tmp, path);
}

/** Read a dot-notation key from the config map. */
export function getConfigValue(cfg: ConfigMap, key: string): unknown {
	const parts = key.split(".");
	let cur: unknown = cfg;
	for (const part of parts) {
		if (cur && typeof cur === "object" && !Array.isArray(cur) && part in (cur as ConfigMap)) {
			cur = (cur as ConfigMap)[part];
		} else {
			return undefined;
		}
	}
	return cur;
}

/** Set (or delete, when value is undefined) a dot-notation key. */
export function setConfigValue(cfg: ConfigMap, key: string, value: unknown): void {
	const parts = key.split(".");
	let cur: ConfigMap = cfg;
	for (let i = 0; i < parts.length - 1; i++) {
		const part = parts[i]!;
		const next = cur[part];
		if (next && typeof next === "object" && !Array.isArray(next)) {
			cur = next as ConfigMap;
		} else {
			const made: ConfigMap = {};
			cur[part] = made;
			cur = made;
		}
	}
	const last = parts[parts.length - 1]!;
	if (value === undefined) delete cur[last];
	else cur[last] = value;
}

/** Report whether the config file exists (with its permissions intact). */
export function configExists(path: string): boolean {
	try {
		return statSync(path).isFile();
	} catch {
		return false;
	}
}

/** Directory holding the config file (and the transcripts/ folder beside it). */
export function configDir(path: string): string {
	return dirname(path);
}

/** Path of the package, for vendored assets like the PoW wasm. */
export function getPackageDir(): string {
	// src/config.ts -> package root (dist/config.js -> dist's parent).
	return dirname(dirname(fileURLToPath(import.meta.url)));
}
