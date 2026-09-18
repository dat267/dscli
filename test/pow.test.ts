import { strict as assert } from "node:assert";
import { test } from "node:test";
import { challengePrefix, numberToString, powHeader, type Challenge } from "../src/core/deepseek/pow.js";

/**
 * A challenge DeepSeek's server could have issued. Its value was computed
 * offline as deepseekHashV1("450f343f44a1e9e6_1752033600_999") (the
 * reverse-engineered 0x06-domain, 23-round variant), so a correct wasm_solve
 * invocation MUST return answer 999. This pins the whole PoW plumbing — call
 * convention, memory layout, status/answer decoding — without a live session.
 */
function goldenChallenge(): Challenge {
	return {
		algorithm: "DeepSeekHashV1",
		challenge: "9099d8ee62c210152bb06cf47a6071e24785ab2e6c413d4cab9bbb9f849f5a58",
		salt: "450f343f44a1e9e6",
		signature: "signed-request",
		target_path: "/api/v0/chat/completion",
		difficulty: 2000,
		expire_at: 1752033600,
	};
}

test("challenge prefix", () => {
	const ch = goldenChallenge();
	assert.equal(challengePrefix(ch), "450f343f44a1e9e6_1752033600_");
});

test("number as string", () => {
	const cases: Record<string, string> = {
		"1752033600": "1752033600",
		"3": "3",
		"3.5": "3.5",
		"1e3": "1000",
	};
	for (const [inRaw, want] of Object.entries(cases)) {
		assert.equal(numberToString(inRaw), want, `numberToString(${inRaw})`);
	}
	// Parsed JSON numbers already carry the shortest form.
	assert.equal(numberToString(1752033600), "1752033600");
	assert.equal(numberToString(3.5), "3.5");
});

test("solve gold challenge produces the exact header", () => {
	const ch = goldenChallenge();
	const h1 = powHeader(ch);
	assert.equal(typeof h1, "string");
	// Solving is deterministic: the same challenge gives the same header.
	assert.equal(powHeader(ch), h1);
	const raw = Buffer.from(h1, "base64").toString("utf8");
	assert.equal(
		raw,
		'{"algorithm":"DeepSeekHashV1","challenge":"9099d8ee62c210152bb06cf47a6071e24785ab2e6c413d4cab9bbb9f849f5a58","salt":"450f343f44a1e9e6","answer":999,"signature":"signed-request","target_path":"/api/v0/chat/completion"}',
	);
});
