// Package deepseek speaks chat.deepseek.com's internal web API: it creates a
// chat session, solves DeepSeek's DeepSeekHashV1 proof-of-work challenge (by
// running DeepSeek's own WebAssembly module inside the wazero sandbox) and
// streams the completion response.
package deepseek

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// powWasm is DeepSeek's sha3_wasm_bg.wasm (wasm-bindgen build of the
// DeepSeekHashV1 PoW solver, ~26 KB). It is the exact module the web app at
// chat.deepseek.com loads from its CDN; fetched from the sums001/Deepseek-API
// repository, sha256 b3fca8cc072c1defbd60c02266a8e48bd307a1804aaff4314900aea720e72f7d.
//
// The module is self-contained (no imports). Exports used here:
//
//	wasm_solve(retptr, challenge_ptr, challenge_len, prefix_ptr, prefix_len, difficulty f64) -> ()
//
// with a 16-byte return slot on the wasm-bindgen shadow stack: an i32 status
// flag at retptr+0 (0 = no answer found) and an f64 answer at retptr+8.
// __wbindgen_export_0 is malloc(size, align) and
// __wbindgen_add_to_stack_pointer(delta) moves the shadow stack pointer.
//
//go:embed sha3_wasm_bg.wasm
var powWasm []byte

// SolveTimeout bounds a single PoW solve. DeepSeek's challenges are tuned to
// be solvable in well under a minute on a typical machine.
const SolveTimeout = 2 * time.Minute

// Challenge is the `data.biz_data.challenge` object returned by
// POST /api/v0/chat/create_pow_challenge. expire_at is kept as a raw JSON
// number so the solving prefix exactly matches what the JavaScript client
// builds (an integer stays an integer).
type Challenge struct {
	Algorithm  string      `json:"algorithm"`
	Challenge  string      `json:"challenge"`
	Salt       string      `json:"salt"`
	Signature  string      `json:"signature"`
	TargetPath string      `json:"target_path"`
	Difficulty float64     `json:"difficulty"`
	ExpireAt   json.Number `json:"expire_at"`
}

// Prefix returns the solving prefix f"{salt}_{expire_at}_".
func (c Challenge) Prefix() string {
	return fmt.Sprintf("%s_%s_", c.Salt, numberAsString(c.ExpireAt))
}

// numberAsString formats a JSON number the way Python's repr does: integers
// stay integers ("1750000000"), non-integers use shortest float formatting.
func numberAsString(n json.Number) string {
	s := string(n)
	if !strings.ContainsAny(s, ".eE") {
		return s
	}
	f, err := n.Float64()
	if err != nil {
		return s
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// powSolver wraps the wazero instantiation of DeepSeek's wasm PoW module.
// Calls are serialised with a mutex because the module's shadow-stack pointer
// is a single global.
type powSolver struct {
	mu     sync.Mutex
	mod    api.Module
	mem    api.Memory
	solve  api.Function
	malloc api.Function
	stack  api.Function
}

// newPowSolver compiles and instantiates the embedded wasm module.
func newPowSolver(ctx context.Context) (*powSolver, error) {
	r := wazero.NewRuntime(ctx)
	compiled, err := r.CompileModule(ctx, powWasm)
	if err != nil {
		_ = r.Close(ctx)
		return nil, fmt.Errorf("compile pow wasm: %w", err)
	}
	mod, err := r.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		_ = r.Close(ctx)
		return nil, fmt.Errorf("instantiate pow wasm: %w", err)
	}
	s := &powSolver{mod: mod, mem: mod.Memory()}
	if s.solve = mod.ExportedFunction("wasm_solve"); s.solve == nil {
		s.close()
		return nil, fmt.Errorf("pow wasm missing export wasm_solve")
	}
	if s.malloc = mod.ExportedFunction("__wbindgen_export_0"); s.malloc == nil {
		s.close()
		return nil, fmt.Errorf("pow wasm missing export __wbindgen_export_0")
	}
	if s.stack = mod.ExportedFunction("__wbindgen_add_to_stack_pointer"); s.stack == nil {
		s.close()
		return nil, fmt.Errorf("pow wasm missing export __wbindgen_add_to_stack_pointer")
	}
	return s, nil
}

func (s *powSolver) close() {
	_ = s.mod.Close(context.Background())
}

// writeString mallocs a UTF-8 copy of text in wasm memory and returns (ptr, len).
func (s *powSolver) writeString(ctx context.Context, text string) (uint32, uint32, error) {
	data := []byte(text)
	out, err := s.malloc.Call(ctx, uint64(len(data)), 1) // align 1, like the JS wrapper
	if err != nil {
		return 0, 0, err
	}
	ptr := uint32(out[0])
	if ok := s.mem.Write(ptr, data); !ok {
		return 0, 0, fmt.Errorf("wasm memory out of bounds at %d (len %d)", ptr, len(data))
	}
	return ptr, uint32(len(data)), nil
}

// Solve runs DeepSeek's wasm solver for challenge/prefix/difficulty and
// returns the integer answer, or an error if no answer was found (expired or
// invalid challenge).
func (s *powSolver) Solve(ctx context.Context, challenge, prefix string, difficulty float64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, SolveTimeout)
	defer cancel()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Reserve a 16-byte return slot on the shadow stack: i32 status at +0,
	// f64 answer at +8. The negative delta must go through a variable so the
	// runtime conversion to uint64 (two's complement) is valid.
	delta := int64(-16)
	stackOut, err := s.stack.Call(ctx, uint64(delta))
	if err != nil {
		return 0, fmt.Errorf("reserve return slot: %w", err)
	}
	retptr := uint32(stackOut[0])

	cPtr, cLen, err := s.writeString(ctx, challenge)
	if err != nil {
		return 0, err
	}
	pPtr, pLen, err := s.writeString(ctx, prefix)
	if err != nil {
		return 0, err
	}

	_, err = s.solve.Call(ctx,
		uint64(retptr),
		uint64(cPtr), uint64(cLen),
		uint64(pPtr), uint64(pLen),
		math.Float64bits(difficulty),
	)
	if err != nil {
		return 0, fmt.Errorf("wasm_solve: %w", err)
	}

	status, _ := s.mem.ReadUint32Le(retptr)
	var answer float64
	if status != 0 {
		answer, _ = s.mem.ReadFloat64Le(retptr + 8)
	}
	if _, err := s.stack.Call(ctx, uint64(16)); err != nil {
		return 0, fmt.Errorf("restore stack pointer: %w", err)
	}

	if status == 0 {
		return 0, fmt.Errorf("pow solver returned no answer (challenge expired?)")
	}
	return int64(answer), nil
}

// powPayload is the JSON document base64-encoded into the x-ds-pow-response
// header. Field order matches the website's client.
type powPayload struct {
	Algorithm  string `json:"algorithm"`
	Challenge  string `json:"challenge"`
	Salt       string `json:"salt"`
	Answer     int64  `json:"answer"`
	Signature  string `json:"signature"`
	TargetPath string `json:"target_path"`
}

// PowHeader solves the challenge and returns the base64 x-ds-pow-response
// header value.
func PowHeader(ctx context.Context, ch Challenge) (string, error) {
	solver, err := newPowSolver(ctx)
	if err != nil {
		return "", err
	}
	defer solver.close()

	answer, err := solver.Solve(ctx, ch.Challenge, ch.Prefix(), ch.Difficulty)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(powPayload{
		Algorithm:  ch.Algorithm,
		Challenge:  ch.Challenge,
		Salt:       ch.Salt,
		Answer:     answer,
		Signature:  ch.Signature,
		TargetPath: ch.TargetPath,
	})
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}