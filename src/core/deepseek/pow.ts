/**
 * DeepSeek's DeepSeekHashV1 proof-of-work, solved by running DeepSeek's own
 * WebAssembly module (the exact sha3_wasm_bg.wasm the web app loads from its
 * CDN; vendored from the sums001/Deepseek-API repository, sha256
 * b3fca8cc072c1defbd60c02266a8e48bd307a1804aaff4314900aea720e72f7d).
 *
 * The module is self-contained (no imports). Exports used:
 *
 *   wasm_solve(retptr, challenge_ptr, challenge_len, prefix_ptr, prefix_len, difficulty f64) -> ()
 *
 * with a 16-byte return slot on the wasm-bindgen shadow stack: an i32 status
 * flag at retptr+0 (0 = no answer found) and an f64 answer at retptr+8.
 * __wbindgen_export_0 is malloc(size, align) and
 * __wbindgen_add_to_stack_pointer(delta) moves the shadow stack pointer.
 *
 * The port mirrors internal/deepseek/pow.go: same module, same call
 * convention. Node executes wasm synchronously, so no mutex is needed
 * (the Go version serialised access to the single shadow-stack pointer).
 */
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { getPackageDir } from "../../config.js";

const powWasm = readFileSync(
	join(getPackageDir(), "src", "core", "deepseek", "sha3_wasm_bg.wasm"),
);

interface PowExports extends WebAssembly.Exports {
	memory: WebAssembly.Memory;
	wasm_solve: (retptr: number, cPtr: number, cLen: number, pPtr: number, pLen: number, difficulty: number) => void;
	__wbindgen_export_0: (size: number, align: number) => number;
	__wbindgen_add_to_stack_pointer: (delta: number) => number;
}

let instance: WebAssembly.Instance | undefined;

/** Instantiate (once) and return the module's exports. */
function solver(): PowExports {
	if (!instance) {
		const mod = new WebAssembly.Module(powWasm);
		instance = new WebAssembly.Instance(mod, {});
	}
	return instance.exports as PowExports;
}

/** The `data.biz_data.challenge` object from POST /api/v0/chat/create_pow_challenge. */
export interface Challenge {
	algorithm: string;
	challenge: string;
	salt: string;
	signature: string;
	target_path: string;
	difficulty: number;
	/** Raw expire_at: parsed JSON numbers lose their literal formatting, so the caller may pass the original string. */
	expire_at: number | string;
}

/**
 * numberToString formats expire_at the way the website's client builds the
 * prefix: an integer stays an integer, non-integers use shortest formatting.
 * JSON.parse in JS already produces the shortest form for round-trip values
 * (1e3 -> 1000, 3.0 -> 3), matching the Go numberAsString behaviour.
 */
export function numberToString(n: number | string): string {
	if (typeof n === "string") {
		if (!/[.eE]/.test(n)) return n;
		const f = Number(n);
		return Number.isFinite(f) ? String(f) : n;
	}
	return String(n);
}

/** The solving prefix f"{salt}_{expire_at}_". */
export function challengePrefix(ch: Challenge): string {
	return `${ch.salt}_${numberToString(ch.expire_at)}_`;
}

/** malloc a UTF-8 copy of text in wasm memory; returns [ptr, length]. */
function writeString(ex: PowExports, text: string): [ptr: number, len: number] {
	const data = Buffer.from(text, "utf8");
	const ptr = ex.__wbindgen_export_0(data.length, 1); // align 1, like the JS wrapper
	new Uint8Array(ex.memory.buffer, ptr, data.length).set(data);
	return [ptr, data.length];
}

/**
 * Run DeepSeek's wasm solver for challenge/prefix/difficulty and return the
 * integer answer; throws when no answer was found (expired or invalid
 * challenge).
 */
export function solvePow(challenge: string, prefix: string, difficulty: number): number {
	const ex = solver();
	// Reserve a 16-byte return slot on the shadow stack: i32 status at +0,
	// f64 answer at +8.
	const retptr = ex.__wbindgen_add_to_stack_pointer(-16);
	try {
		const [cPtr, cLen] = writeString(ex, challenge);
		const [pPtr, pLen] = writeString(ex, prefix);
		ex.wasm_solve(retptr, cPtr, cLen, pPtr, pLen, difficulty);
		// wasm_solve may have grown memory; read through a fresh view.
		const view = new DataView(ex.memory.buffer);
		const status = view.getUint32(retptr, true);
		if (status === 0) {
			throw new Error("pow solver returned no answer (challenge expired?)");
		}
		return Math.trunc(view.getFloat64(retptr + 8, true));
	} finally {
		ex.__wbindgen_add_to_stack_pointer(16);
	}
}

/**
 * Solve the challenge and return the base64 x-ds-pow-response header value.
 * Field order in the payload matches the website's client.
 */
export function powHeader(ch: Challenge): string {
	const answer = solvePow(ch.challenge, challengePrefix(ch), ch.difficulty);
	const payload = {
		algorithm: ch.algorithm,
		challenge: ch.challenge,
		salt: ch.salt,
		answer,
		signature: ch.signature,
		target_path: ch.target_path,
	};
	return Buffer.from(JSON.stringify(payload), "utf8").toString("base64");
}
