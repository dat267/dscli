/**
 * `dscli config`: init/path/set/unset. Port of cmd/config.go — the config is
 * a JSON object at the resolved path; set parses booleans and lossless
 * numbers, both with dot-notation for nested keys.
 */
import process from "node:process";
import { configExists, loadConfigMap, saveConfigMap, setConfigValue } from "../../config.js";
import { stderrNote } from "../../ui/notes.js";

export interface ConfigOptions {
	cfgPath: string;
	/** set: the key and value; unset: the key. */
	key?: string;
	value?: string;
	overwrite?: boolean;
}

export async function configCommand(sub: string | undefined, opts: ConfigOptions): Promise<void> {
	switch (sub) {
		case undefined:
			throw new Error("config requires a subcommand (init, path, set, unset)");
		case "init":
			return configInit(opts);
		case "path":
			return configPath(opts);
		case "set":
			return configSet(opts);
		case "unset":
			return configUnset(opts);
		default:
			throw new Error(`unknown config subcommand ${sub}`);
	}
}

function configInit(opts: ConfigOptions): void {
	const p = opts.cfgPath;
	if (configExists(p) && !opts.overwrite) {
		throw new Error(`configuration file already exists at ${p}`);
	}
	saveConfigMap(p, {});
	process.stdout.write(`Configuration file created at ${p}\n`);
}

function configPath(opts: ConfigOptions): void {
	const p = opts.cfgPath;
	if (!configExists(p)) process.stdout.write(`${p} (does not exist)\n`);
	else process.stdout.write(p + "\n");
}

/** parseConfigValue: booleans, then lossless numbers; everything else stays a string. */
export function parseConfigValue(raw: string): string | boolean | number {
	if (raw === "true") return true;
	if (raw === "false") return false;
	const n = Number(raw);
	if (
		Number.isFinite(n) &&
		!Number.isNaN(n) &&
		raw.trim() !== "" &&
		String(n) === raw
	) {
		return n; // Only lossless conversions; "00123" or "1e999" stay strings
	}
	return raw;
}

function configSet(opts: ConfigOptions): void {
	if (opts.key === undefined || opts.value === undefined) {
		throw new Error("config set requires a key and a value");
	}
	validateConfigKey(opts.key);
	const cfg = loadConfigMap(opts.cfgPath);
	const val = parseConfigValue(opts.value);
	setConfigValue(cfg, opts.key, val);
	try {
		saveConfigMap(opts.cfgPath, cfg);
	} catch (err) {
		throw new Error(err instanceof Error ? err.message : String(err));
	}
	process.stdout.write(`Set "${opts.key}" = ${JSON.stringify(val)}\n`);
}

function configUnset(opts: ConfigOptions): void {
	if (opts.key === undefined) {
		throw new Error("config unset requires a key");
	}
	validateConfigKey(opts.key);
	const cfg = loadConfigMap(opts.cfgPath);
	setConfigValue(cfg, opts.key, undefined);
	try {
		saveConfigMap(opts.cfgPath, cfg);
	} catch (err) {
		throw new Error(err instanceof Error ? err.message : String(err));
	}
	process.stdout.write(`Unset "${opts.key}"\n`);
}

/** validateConfigKey rejects empty parts and whitespace-only keys. */
export function validateConfigKey(key: string): void {
	if (key.trim() === "" || key.split(".").some((p) => p === "")) {
		throw new Error("configuration key cannot be empty");
	}
}

/** Warn (softly) when the active config silently comes from a planted ./dscli.json. */
export function warnOnConfigOverride(active: string, autoResolved: boolean): void {
	if (!autoResolved || active !== "dscli.json" || !configExists(active)) return;
	stderrNote(`warning: using config file "${active}" from the current directory\n`);
}
