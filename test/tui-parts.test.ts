import { strict as assert } from "node:assert";
import chalk from "chalk";
chalk.level = 3; // force truecolor even in the non-TTY test runner
import { test } from "node:test";
import { FooterComponent, NoteComponent, UserMessageComponent, WorkingBorder } from "../src/modes/interactive/components.js";
import { SPINNER_FRAMES } from "../src/modes/interactive/parts.js";
import { PALETTE, theme } from "../src/modes/interactive/theme.js";

const strip = (s: string) => s.replace(/\x1b\[[0-9;]*m/g, "");

test("spinner frames are pi's braille pulse", () => {
	assert.equal(SPINNER_FRAMES[0], "⠋");
	assert.equal(SPINNER_FRAMES.length, 10);
});

test("WorkingBorder: idle is the plain border-coloured rule", () => {
	const b = new WorkingBorder();
	const lines = b.render(40);
	assert.equal(lines.length, 1);
	assert.equal(strip(lines[0]!), "─".repeat(40));
	assert.ok(lines[0]!.includes("\x1b[38;2;95;135;255m"), "border colour (pi's blue)");
});

test("WorkingBorder: busy embeds the working indicator, no layout change", () => {
	const b = new WorkingBorder();
	b.setBusy(true);
	const lines = b.render(40);
	assert.equal(lines.length, 1); // still one row
	const visible = strip(lines[0]!);
	assert.equal(visible.length, 40);
	assert.ok(visible.includes("⠋"), "braille frame");
	assert.ok(visible.includes("Working"));
	// The frame is in pi's accent colour, the label muted.
	assert.ok(lines[0]!.includes("\x1b[38;2;138;190;183m⠋"), "accent spinner");
	// The frame advances on tick.
	b.tick();
	assert.ok(strip(b.render(40)[0]!).includes(SPINNER_FRAMES[1]!));
	// Setting busy=false stops the indicator.
	b.setBusy(false);
	assert.ok(!b.render(40)[0]!.includes("Working"));
});

test("UserMessageComponent renders the prompt in a padded background box", () => {
	const c = new UserMessageComponent("Hello\nworld");
	const lines = c.render(30);
	// pi's Box: blank padded rows around the content, all under the bg colour.
	assert.equal(lines.length, 4); // top pad, 2 content rows, bottom pad
	assert.ok(lines[0]!.includes("\x1b[48;2;52;53;65m"), "userMsgBg #343541");
	assert.equal(strip(lines[0]!), " ".repeat(30));
	assert.ok(strip(lines[1]!).includes("Hello"));
	assert.ok(strip(lines[2]!).includes("world"));
});

test("NoteComponent dims and truncates system text", () => {
	const c = new NoteComponent("a hint\nsecond line");
	const lines = c.render(80);
	assert.equal(lines.length, 2);
	assert.ok(lines[0]!.includes("\x1b[38;2;128;128;128m"), "muted grey");
	assert.equal(strip(c.render(5)[0]!), strip(lines[0]!).slice(0, 5));
});

test("FooterComponent right-aligns the model side", () => {
	const f = new FooterComponent({
		model: "default",
		thinking: false,
		search: false,
		mode: "ephemeral",
		turns: 2,
		conversation: "910c6ac2-cca9:4",
		cwd: "~/repos/dscli",
	});
	const line = strip(f.render(120)[0]!);
	assert.ok(line.startsWith("~/repos/dscli · 2 turns · ephemeral · 910c6ac2…"));
	assert.ok(line.endsWith("default · thinking off · search off"));
	// Right-aligned: padding between the two sides.
	assert.equal(line.length, 120);
});

test("FooterComponent truncates the left side on a narrow terminal", () => {
	const f = new FooterComponent({
		model: "default",
		thinking: true,
		search: true,
		mode: "persisted",
		turns: 1,
		conversation: "sess-1",
		cwd: "~/somewhere",
	});
	// pi's footer: when both sides cannot fit, the right side is dropped
	// entirely rather than truncated mid-state.
	const line = strip(f.render(40)[0]!);
	assert.ok(line.length <= 40, line);
	assert.ok(line.startsWith("~/somewhere"), line);
	assert.ok(!line.includes("thinking on"), line);
});

test("theme palette matches pi's dark theme", () => {
	assert.equal(PALETTE.accent, "#8abeb7");
	assert.equal(PALETTE.userMsgBg, "#343541");
	assert.equal(theme.fg("error", "x"), "\x1b[38;2;204;102;102mx\x1b[39m");
});
