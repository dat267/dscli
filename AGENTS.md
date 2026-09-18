# Agent Instructions

## Release workflow

This project installs straight from GitHub, bypassing npm registry publishing:

    npm install -g github:dat267/dscli

There are no published tags to freeze (npm installs resolve the default
branch's HEAD). If you publish to the npm registry later, remember that a
published version is immutable — a broken release requires a new version,
never a force-push.

Before pushing to `main`, verify:

1. `git status` is clean (no uncommitted or unstaged changes).
2. `npm run build` passes (tsc + wasm asset copy).
3. `npm test` passes (`node --import tsx --test test/*.test.ts`).
4. The README (especially the help block and examples) matches the output of
   `node --import tsx src/cli.ts --help` — enforced by `test/readme.test.ts`.

## PoW wasm

`src/core/deepseek/sha3_wasm_bg.wasm` is DeepSeek's proprietary PoW module,
vendored from the sums001/Deepseek-API repository (sha256
b3fca8cc072c1defbd60c02266a8e48bd307a1804aaff4314900aea720e72f7d). If you
replace it, the golden challenge test in `test/pow.test.ts` pins the exact
solver behaviour (0x06 domain, rounds 1..23 Keccak, answer 999 and the exact
base64 header), so a regression in the wasm or in the call convention fails
loudly. `npm run build` copies the wasm into `dist/` — do not edit the copy.
