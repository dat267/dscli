package deepseek

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
)

// goldenChallenge is a challenge DeepSeek's server could have issued. Its
// value was computed offline as deepseekHashV1("450f343f44a1e9e6_1752033600_999")
// (the reverse-engineered 0x06-domain, 23-round variant), so a correct
// wasm_solve invocation MUST return answer 999. This pins the whole PoW
// plumbing — shadow stack, call convention, memory layout, status/answer
// decoding — without needing a live session.
func goldenChallenge() Challenge {
	return Challenge{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  "9099d8ee62c210152bb06cf47a6071e24785ab2e6c413d4cab9bbb9f849f5a58",
		Salt:       "450f343f44a1e9e6",
		Signature:  "signed-request",
		TargetPath: "/api/v0/chat/completion",
		Difficulty: 2000,
		ExpireAt:   json.Number("1752033600"),
	}
}

func TestChallengePrefix(t *testing.T) {
	ch := goldenChallenge()
	if got, want := ch.Prefix(), "450f343f44a1e9e6_1752033600_"; got != want {
		t.Errorf("Prefix() = %q, want %q", got, want)
	}
}

func TestNumberAsString(t *testing.T) {
	for in, want := range map[string]string{
		"1752033600": "1752033600",
		"3":          "3",
		"3.5":        "3.5",
		"1e3":        "1000",
	} {
		if got := numberAsString(json.Number(in)); got != want {
			t.Errorf("numberAsString(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSolveGoldChallenge solves a known-good challenge and checks the exact
// header produced: base64 of the canonical JSON payload with answer 999.
func TestSolveGoldChallenge(t *testing.T) {
	ctx := context.Background()
	ch := goldenChallenge()

	h1, err := PowHeader(ctx, ch)
	if err != nil {
		t.Fatalf("PowHeader: %v", err)
	}
	h2, err := PowHeader(ctx, ch)
	if err != nil {
		t.Fatalf("PowHeader (2nd run): %v", err)
	}
	if h1 != h2 {
		t.Errorf("solve not deterministic: %q vs %q", h1, h2)
	}

	raw, err := base64.StdEncoding.DecodeString(h1)
	if err != nil {
		t.Fatalf("header is not valid base64: %v", err)
	}
	wantJSON := fmt.Sprintf(
		`{"algorithm":"DeepSeekHashV1","challenge":%q,"salt":%q,"answer":999,"signature":%q,"target_path":%q}`,
		ch.Challenge, ch.Salt, ch.Signature, ch.TargetPath,
	)
	if string(raw) != wantJSON {
		t.Errorf("header payload mismatch:\n got %s\nwant %s", raw, wantJSON)
	}
}

// TestSolveHonorsInputs checks the wasm answer changes when the challenge or
// the expire_at (which feeds the prefix) changes.
func TestSolveHonorsInputs(t *testing.T) {
	ctx := context.Background()
	a := goldenChallenge()

	b := a
	b.Challenge = "9099d8ee62c210152bb06cf47a6071e24785ab2e6c413d4cab9bbb9f849f5a99"
	bSalt := b
	// Challenge differing only in the last byte must give a different answer.
	_, err := PowHeader(ctx, bSalt)
	if err == nil {
		t.Fatal("expected no answer for an unsolvable challenge, got one")
	}

	// A different expire_at changes the prefix, so the same challenge digest
	// no longer matches nonce 999: solving should fail rather than return 999.
	c := a
	c.ExpireAt = json.Number("1752033601")
	_, err = PowHeader(ctx, c)
	if err == nil {
		t.Fatal("expected no answer when expire_at shifts the prefix, got one")
	}

	// Sanity: the sibling digest (nonce 1000 under the same prefix) solves to
	// 1000.
	d := a
	d.Challenge = "fa3bc704b7f9f01808d4f114523608c9bcaff2747eedb938d39ce0af7b8e367c"
	if err := checkAnswer(ctx, d, 1000); err != nil {
		t.Error(err)
	}
}

// TestSolveRequiresDifficultyBounds checks that a valid challenge whose nonce
// exceeds the difficulty bound is not found.
func TestSolveRequiresDifficultyBounds(t *testing.T) {
	ctx := context.Background()
	ch := goldenChallenge()
	ch.Difficulty = 998 // nonce 999 is out of range
	if err := checkAnswer(ctx, ch, 999); err == nil {
		t.Fatal("expected no answer when nonce exceeds difficulty, got one")
	}
}

// checkAnswer asserts the solver returns the expected nonce for a challenge.
func checkAnswer(ctx context.Context, ch Challenge, want int64) error {
	h, err := PowHeader(ctx, ch)
	if err != nil {
		return fmt.Errorf("PowHeader: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(h)
	if err != nil {
		return fmt.Errorf("bad base64: %w", err)
	}
	var payload struct {
		Answer int64 `json:"answer"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("bad payload JSON: %w", err)
	}
	if payload.Answer != want {
		return fmt.Errorf("answer = %d, want %d", payload.Answer, want)
	}
	return nil
}

// TestPowPayloadFieldOrder pins the JSON field order the website's client
// emits, in case the server is picky about byte-identical headers.
func TestPowPayloadFieldOrder(t *testing.T) {
	raw, err := json.Marshal(powPayload{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  "c",
		Salt:       "s",
		Answer:     42,
		Signature:  "sig",
		TargetPath: "/api/v0/chat/completion",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"algorithm":"DeepSeekHashV1","challenge":"c","salt":"s","answer":42,"signature":"sig","target_path":"/api/v0/chat/completion"}`
	if string(raw) != want {
		t.Errorf("payload JSON differs:\n got %s\nwant %s", raw, want)
	}
}
