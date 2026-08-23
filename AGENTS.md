# Agent Instructions

## Release workflow

This project is installable via `go install github.com/dat267/dscli@latest`. Go's
module proxy permanently freezes the source of every published tag, and the
module cache caches that source forever. A broken or incomplete tag cannot be
silently fixed — it requires a new version.

Therefore, before tagging a commit and pushing that tag, you MUST verify:

1. `git status` is clean (no uncommitted or unstaged changes).
2. `go build ./...` and `go vet ./...` pass.
3. `go test -race -count=1 ./...` passes.
4. The README (especially the usage/help block and examples) matches the output
   of the freshly built binary.
5. The version bump is committed and the tag points at the correct commit.

`golangci-lint` is an optional local quality check and is not part of CI.

Never tag or push a version you have not verified per the checklist above.

## PoW wasm

`internal/deepseek/sha3_wasm_bg.wasm` is DeepSeek's proprietary PoW module,
vendored from the sums001/Deepseek-API repository (sha256
b3fca8cc072c1defbd60c02266a8e48bd307a1804aaff4314900aea720e72f7d). If you
replace it, update the checksum comment in `internal/deepseek/pow.go` and the
golden challenge test in `internal/deepseek/pow_test.go` — the golden vector
pins the exact solver behaviour (0x06 domain, rounds 1..23 Keccak), so a
regression in the wasm or in the call convention fails loudly.