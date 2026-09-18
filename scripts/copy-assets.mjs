// Copies non-TypeScript assets (the vendored PoW wasm) into dist/.
import { copyFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const files = [
	["src/core/deepseek/sha3_wasm_bg.wasm", "dist/core/deepseek/sha3_wasm_bg.wasm"],
];
for (const [src, dst] of files) {
	mkdirSync(dirname(join(root, dst)), { recursive: true });
	copyFileSync(join(root, src), join(root, dst));
	console.log(`copied ${src} -> ${dst}`);
}
