import { strict as assert } from "node:assert";
import { test } from "node:test";
import { join } from "node:path";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import {
	advanceConversation,
	conversationID,
	effectiveModel,
	loadSavedSession,
	persistConversation,
	saveSession,
	splitConversation,
	clearSession,
} from "../src/core/session.js";
import { appendTranscript, loadTranscript, transcriptPath, transcriptsEnabled } from "../src/core/transcript.js";
import { DeepSeekClient } from "../src/core/deepseek/client.js";
import { setConfigValue, loadConfigMap, saveConfigMap } from "../src/config.js";
import { parseConfigValue } from "../src/cli/commands/config.js";

const tmp = () => join(mkdtempSync(join(tmpdir(), "dscli-")), "dscli.json");

test("splitConversation parses session and parent message id", () => {
	assert.deepEqual(splitConversation(""), { sessionId: "", parentId: null });
	assert.deepEqual(splitConversation("sess-1"), { sessionId: "sess-1", parentId: null });
	assert.deepEqual(splitConversation("sess-1:2"), { sessionId: "sess-1", parentId: 2 });
	assert.deepEqual(splitConversation("sess-1:abc"), { sessionId: "sess-1", parentId: null });
	assert.deepEqual(splitConversation(":2"), { sessionId: "", parentId: null });
});

test("conversationID renders the id for the next turn", () => {
	assert.equal(conversationID("sess-1", null, 3), "sess-1:3");
	assert.equal(conversationID("sess-1", 2, 0), "sess-1:2");
	assert.equal(conversationID("sess-1", null, 0), "sess-1");
	assert.equal(advanceConversation("sess-1:2", 7), "sess-1:7");
});

test("effectiveModel maps empty to default", () => {
	assert.equal(effectiveModel(""), "default");
	assert.equal(effectiveModel("expert"), "expert");
});

test("ephemeral runs save nothing: default session functions", () => {
	const cfg = tmp();
	assert.equal(loadSavedSession(cfg), "");
	assert.equal(saveSession(cfg, "sess-1:2"), undefined);
	assert.equal(loadSavedSession(cfg), "sess-1:2");
	assert.ok(saveSession(cfg, "sess-1:3") === undefined);
	persistConversation(cfg, false, "sess-1:9");
	assert.equal(loadSavedSession(cfg), "sess-1:3", "persistConversation is a no-op without --persist");
	persistConversation(cfg, true, "sess-1:9");
	assert.equal(loadSavedSession(cfg), "sess-1:9");
	assert.equal(clearSession(cfg), undefined);
	assert.equal(loadSavedSession(cfg), "");
	assert.equal(clearSession(cfg), undefined); // idempotent
});

test("transcriptsEnabled: only persisted runs with a config dir", () => {
	assert.equal(transcriptsEnabled("/x/cfg.json", false, false), false); // ephemeral by default
	assert.equal(transcriptsEnabled("", true, false), false); // no data dir
	assert.equal(transcriptsEnabled("/x/cfg.json", true, false), true); // explicit --persist
	assert.equal(transcriptsEnabled("/x/cfg.json", true, true), false); // --persist --no-transcript
});

test("transcript path, append and load", () => {
	const cfg = tmp();
	assert.equal(transcriptPath(cfg, ""), "");
	// The exact path: transcripts/<session>.jsonl next to the config file.
	const p = transcriptPath(cfg, "sess-9:17");
	assert.ok(p.endsWith(join("transcripts", "sess-9.jsonl")), p);
	assert.equal(loadTranscript(cfg, "sess-9"), null);
	appendTranscript(cfg, "sess-9", "user", "hi");
	appendTranscript(cfg, "sess-9", "assistant", "Hello back");
	const entries = loadTranscript(cfg, "sess-9")!;
	assert.equal(entries.length, 2);
	assert.equal(entries[0]!.role, "user");
	assert.equal(entries[0]!.text, "hi");
	assert.equal(entries[1]!.text, "Hello back");
	assert.ok(entries[0]!.time !== "");
	// Corrupted lines are skipped, not fatal.
	const file = readFileSync(p, "utf8");
	writeFileSync(p, file + "not json\n");
	assert.equal(loadTranscript(cfg, "sess-9")!.length, 2);
});

test("parseConfigValue: booleans and lossless numbers only", () => {
	assert.equal(parseConfigValue("true"), true);
	assert.equal(parseConfigValue("false"), false);
	assert.equal(parseConfigValue("5"), 5);
	assert.equal(parseConfigValue("0.5"), 0.5);
	assert.equal(parseConfigValue("00123"), "00123"); // not lossless
	assert.equal(parseConfigValue("hello"), "hello");
});

test("config get/set with dot notation preserves other keys", () => {
	const cfg = tmp();
	saveConfigMap(cfg, { token: "t", session: "sess-1" });
	const m = loadConfigMap(cfg);
	setConfigValue(m, "core.timeout", 5);
	assert.equal(m["token"], "t");
	saveConfigMap(cfg, m);
	assert.deepEqual(loadConfigMap(cfg)["core"], { timeout: 5 });
	setConfigValue(m, "core", undefined); // deleting an intermediate object
});
