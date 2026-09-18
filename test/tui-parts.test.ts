import { strict as assert } from "node:assert";
import { test } from "node:test";
import {
	onoff,
	ruleText,
	statusLine,
	SPINNER_FRAMES,
	ANSI_ACCENT,
	ANSI_DIM,
	ANSI_MUTED,
	ANSI_RESET,
} from "../src/modes/interactive/parts.js";

const colors = {
	dim: (s: string) => ANSI_DIM + s + ANSI_RESET,
	accent: (s: string) => ANSI_ACCENT + s + ANSI_RESET,
	muted: (s: string) => ANSI_MUTED + s + ANSI_RESET,
};

test("spinner frames are pi's braille pulse", () => {
	assert.equal(SPINNER_FRAMES[0], "⠋");
	assert.equal(SPINNER_FRAMES.length, 10);
});

test("idle rule is the plain dim rule; busy rule embeds the working indicator", () => {
	const idle = ruleText(false, 0, 40);
	assert.equal(idle, "─".repeat(40));
	const busy = ruleText(true, 0, 40);
	assert.equal(busy, "── ⠋ Working " + "─".repeat(27));
	// The frame advances modulo the table.
	assert.ok(ruleText(true, 11, 40).includes("⠙"));
});

test("colored rule keeps the accent frame and muted label", () => {
	const v = ruleText(true, 0, 40, colors);
	assert.ok(v.includes(ANSI_ACCENT + "⠋" + ANSI_RESET), v);
	assert.ok(v.includes(ANSI_MUTED + "Working" + ANSI_RESET), v);
	// Visible width still fills the full rule (ANSI codes are zero-width).
	const visible = v.replace(/\x1b\[[0-9;]*m/g, "");
	assert.equal(visible.length, 40);
});

test("idle rule has no working indicator", () => {
	const idle = ruleText(false, 3, 40, colors);
	assert.ok(!idle.includes("Working"));
	assert.ok(!idle.includes(ANSI_ACCENT));
});

test("status line shows modes before turn/session", () => {
	const s = statusLine({
		model: "default",
		thinking: true,
		search: false,
		mode: "ephemeral",
		turn: 2,
		conversation: "910c6ac2-cca9:4",
	});
	assert.equal(s, "DeepSeek · model default · thinking on · search off · ephemeral · turn 2 · 910c6ac2…");
	// The modes come before turn/session so truncation never hides them.
	assert.ok(s.indexOf("thinking") < s.indexOf("turn 2"));
});

test("status line without a conversation omits the tail", () => {
	const s = statusLine({ model: "expert", thinking: false, search: true, mode: "persisted", turn: 0, conversation: "" });
	assert.equal(s, "DeepSeek · model expert · thinking off · search on · persisted · turn 0");
});

test("onoff", () => {
	assert.equal(onoff(true), "on");
	assert.equal(onoff(false), "off");
});
