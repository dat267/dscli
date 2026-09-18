import { strict as assert } from "node:assert";
import { test } from "node:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { zipSync } from "fflate";
import {
	assDialoguePrefix,
	detectFormat,
	isText,
	protectedLines,
	stripHTML,
	verifyProtected,
	readEpub,
} from "../src/core/filetools/index.js";

test("isText: NUL bytes in the probe mean binary", () => {
	assert.equal(isText(Buffer.from("hello")), true);
	assert.equal(isText(Buffer.concat([Buffer.from("hello"), Buffer.from([0])])), false);
	// NUL beyond the probe window does not matter.
	assert.equal(isText(Buffer.concat([Buffer.alloc(9000, 0x61), Buffer.from([0])])), true);
});

test("detectFormat by extension", () => {
	assert.equal(detectFormat("a.lrc", new Uint8Array()), "lrc");
	assert.equal(detectFormat("a.SRT", new Uint8Array()), "srt");
	assert.equal(detectFormat("a.vtt", new Uint8Array()), "vtt");
	assert.equal(detectFormat("a.ass", new Uint8Array()), "ass");
	assert.equal(detectFormat("a.ssa", new Uint8Array()), "ass");
	assert.equal(detectFormat("a.ttml", new Uint8Array()), "ttml");
	assert.equal(detectFormat("a.md", new Uint8Array()), "markdown");
	assert.equal(detectFormat("a.markdown", new Uint8Array()), "markdown");
	assert.equal(detectFormat("a.txt", new Uint8Array()), "text");
	assert.equal(detectFormat("noext", new Uint8Array()), "text");
});

test("protected lines: srt timing lines", () => {
	const got = protectedLines("srt", "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n2\n00:00:03,000 --> 00:00:04,000\nBye\n");
	assert.deepEqual(got, ["00:00:01,000 --> 00:00:02,000", "00:00:03,000 --> 00:00:04,000"]);
	assert.deepEqual(protectedLines("text", "anything"), []);
});

test("protected lines: vtt includes the WEBVTT header and NOTEs", () => {
	const got = protectedLines("vtt", "WEBVTT\n\nNOTE a comment\n\n00:01.000 --> 00:02.000\nHello\n");
	assert.deepEqual(got, ["WEBVTT", "NOTE a comment", "00:01.000 --> 00:02.000"]);
});

test("protected lines: lrc timecodes in order", () => {
	const got = protectedLines("lrc", "[00:12.00]First line\n[01:02]Second\n[02:03.456]Third\n");
	assert.deepEqual(got, ["[00:12.00]", "[01:02]", "[02:03.456]"]);
});

test("protected lines: ass dialogue prefix and raw structure", () => {
	const ass = "[Script Info]\nTitle: x\n\n[Events]\nFormat: Layer, Start\nDialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,Hello\n";
	const got = protectedLines("ass", ass);
	assert.deepEqual(got, ["[Script Info]", "Title: x", "[Events]", "Format: Layer, Start", "Dialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,"]);
	assert.equal(
		assDialoguePrefix("Dialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,Hello, world"),
		"Dialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,",
	);
});

test("protected lines: ttml tag sequence", () => {
	const got = protectedLines("ttml", '<tt><body><p>Hi</p><p p2="1">Yo</p></body></tt>');
	assert.deepEqual(got, ["<tt>", "<body>", "<p>", "</p>", '<p p2="1">', "</p>", "</body>", "</tt>"]);
});

test("verifyProtected: identical passes, changes fail", () => {
	const srt = "1\n00:00:01,000 --> 00:00:02,000\nHello\n";
	assert.equal(verifyProtected("srt", srt, "1\n00:00:01,000 --> 00:00:02,000\nHola\n"), undefined);
	const err = verifyProtected("srt", srt, "1\n00:00:01,500 --> 00:00:02,000\nHola\n");
	assert.ok(err instanceof Error);
	assert.match(err.message, /protected line 1 changed/);
	const count = verifyProtected("srt", srt, "1\n00:00:01,000 --> 00:00:02,000\nHola\n\n2\n00:00:03,000 --> 00:00:04,000\nBye\n");
	assert.ok(count instanceof Error);
	assert.match(count!.message, /protected line count changed \(1 → 2\)/);
});

test("stripHTML: tags, scripts and entities", () => {
	assert.equal(stripHTML("<p>Hello &amp; <b>world</b></p>"), "Hello & world");
	assert.equal(stripHTML("<script>bad()</script>ok"), "ok");
	assert.equal(stripHTML("a &lt; b &gt; c &quot;d&quot; &#39;e&apos; f&nbsp;g"), 'a < b > c "d" \'e\' f g');
	assert.equal(stripHTML("4 &lt; 5 &unknown; x"), "4 < 5 &unknown; x");
	assert.equal(stripHTML("loose & amp"), "loose & amp");
});

test("readEpub: spine order, markup stripping, cap", () => {
	const container = `<?xml version="1.0"?><container><rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`;
	const opf = `<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf"><manifest>
<item id="ch1" href="ch1.xhtml" media-type="application/xhtml+xml"/>
<item id="ch2" href="ch2.xhtml" media-type="application/xhtml+xml"/>
<item id="cover" href="cover.xhtml" media-type="application/xhtml+xml"/>
</manifest><spine><itemref idref="ch1"/><itemref idref="ch2"/></spine></package>`;
	const ch1 = "<html><body><p>First &amp; chapter</p></body></html>";
	const ch2 = "<html><body><p>Second chapter</p><!-- note --><script>no()</script></body></html>";
	const zip = zipSync({
		"META-INF/container.xml": Buffer.from(container),
		"OEBPS/content.opf": Buffer.from(opf),
		"OEBPS/ch1.xhtml": Buffer.from(ch1),
		"OEBPS/ch2.xhtml": Buffer.from(ch2),
		"OEBPS/cover.xhtml": Buffer.from("<p>cover</p>"),
	});
	const dir = mkdtempSync(join(tmpdir(), "dscli-"));
	const path = join(dir, "book.epub");
	writeFileSync(path, Buffer.from(zip));
	assert.equal(readEpub(path), "First & chapterSecond chapter"); // spine order, cover excluded
});

test("readEpub: missing container fails loudly", () => {
	const zip = zipSync({ "a.txt": Buffer.from("x") });
	const dir = mkdtempSync(join(tmpdir(), "dscli-"));
	const path = join(dir, "bad.epub");
	writeFileSync(path, Buffer.from(zip));
	assert.throws(() => readEpub(path), /missing META-INF\/container\.xml/);
});
