import { strict as assert } from "node:assert";
import { test } from "node:test";
import { parseArgs } from "../src/cli/args.js";

test("bare command discovery and flags after it", () => {
	const p = parseArgs(["ask", "--persist", "hello", "world"]);
	assert.equal(p.command, "ask");
	assert.deepEqual(p.positionals, ["hello", "world"]);
	assert.equal(p.flags["persist"], true);
});

test("flags before the command", () => {
	const p = parseArgs(["--persist", "ask", "hi"]);
	assert.equal(p.command, "ask");
	assert.equal(p.flags["persist"], true);
	assert.deepEqual(p.positionals, ["hi"]);
});

test("short flags with separate and inline values", () => {
	const p = parseArgs(["ask", "-m", "expert", "-t", "hello"]);
	assert.equal(p.flags["model"], "expert");
	assert.equal(p.flags["thinking"], true);
	const q = parseArgs(["ask", "-mexpert", "hello"]);
	assert.equal(q.flags["model"], "expert");
});

test("long flag with = value", () => {
	const p = parseArgs(["translate", "--to=French", "a.txt"]);
	assert.equal(p.flags["to"], "French");
	assert.deepEqual(p.positionals, ["a.txt"]);
});

test("defaults and env fallbacks", () => {
	const p = parseArgs(["translate", "a.txt"]);
	assert.equal(p.flags["from"], "auto");
	assert.equal(p.flags["to"], "English");
	assert.equal(p.flags["chunk-bytes"], 0);
	assert.equal(p.flags["persist"], false);
	assert.equal(p.flags["token"], "");
});

test("env var fills a flag", () => {
	const prev = process.env["DS_TOKEN"];
	process.env["DS_TOKEN"] = "from-env";
	try {
		const p = parseArgs(["ask", "hi"]);
		assert.equal(p.flags["token"], "from-env");
		const q = parseArgs(["ask", "--token", "explicit", "hi"]);
		assert.equal(q.flags["token"], "explicit"); // command line wins
	} finally {
		if (prev === undefined) delete process.env["DS_TOKEN"];
		else process.env["DS_TOKEN"] = prev;
	}
});

test("-- ends flags: a prompt that starts with -", () => {
	const p = parseArgs(["ask", "--", "-v", "flag-ish"]);
	assert.equal(p.help, undefined);
	assert.deepEqual(p.positionals, ["-v", "flag-ish"]);
});

test("group subcommands and their positionals", () => {
	const p = parseArgs(["session", "select", "sess-9:4"]);
	assert.equal(p.command, "session");
	assert.equal(p.sub, "select");
	assert.deepEqual(p.positionals, ["sess-9:4"]);
});

test("config subcommand with two positionals", () => {
	const p = parseArgs(["config", "set", "token", "abc"]);
	assert.equal(p.command, "config");
	assert.equal(p.sub, "set");
	assert.deepEqual(p.positionals, ["token", "abc"]);
});

test("group bare (session) has no sub", () => {
	const p = parseArgs(["session"]);
	assert.equal(p.command, "session");
	assert.equal(p.sub, undefined);
});

test("global --config-file is recognized anywhere", () => {
	const p = parseArgs(["--config-file", "/tmp/x.json", "ask", "hi"]);
	assert.equal(p.configFile, "/tmp/x.json");
	const q = parseArgs(["ask", "--config-file=/tmp/y.json", "hi"]);
	assert.equal(q.configFile, "/tmp/y.json");
});

test("unknown flags and commands are reported", () => {
	assert.ok(parseArgs(["ask", "--nope"]).errors.length > 0);
	assert.ok(parseArgs(["frobnicate"]).errors.some((e) => e.includes("frobnicate")));
});

test("help and version flags", () => {
	assert.equal(parseArgs(["--help"]).help, true);
	assert.equal(parseArgs(["ask", "-h"]).help, true);
	assert.equal(parseArgs(["--version"]).version, true);
});

test("timeout duration is a string flag", () => {
	const p = parseArgs(["ask", "--timeout", "5m", "hi"]);
	assert.equal(p.flags["timeout"], "5m");
});

test("bool flag with =false", () => {
	const p = parseArgs(["ask", "--persist=false", "hi"]);
	assert.equal(p.flags["persist"], false);
});
