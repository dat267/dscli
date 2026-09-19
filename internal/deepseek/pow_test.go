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

// TestSolveRealChallengeVectors replays challenges the live site issued
// (captured in a HAR) together with the answers the web client submitted in
// its x-ds-pow-response headers. Unlike the offline golden above, these come
// from production: solving each challenge with the same salt/expire_at/
// difficulty must reproduce the recorded answer, pinning the prefix format,
// the wasm call convention and the JSON number handling against the current
// server.
func TestSolveRealChallengeVectors(t *testing.T) {
	vectors := []struct {
		challenge, salt, signature string
		expireAt                   string
		difficulty                 float64
		want                       int64
	}{
		{"16d6d539f41b758899d4687af4fbad52ec302236c4e9dcc55ba69e29554782ec", "8a09c5e0e0bb56df19d8", "f2676e1d6b72726da74dbd936c1f64e160ca03268ddec6a5b4c54944d1ae80a5", "1789842927388", 144000, 22298},
		{"dcf7b173f8b8222947ac5e2f68e80bfc0596999021a6ee046efebf5ce205f17e", "0621a2ae1ce04161307d", "26ea1014ea2d5b854731a26c9d08a45e9d54330768b2e4bb9c51e83003ddf2d2", "1789842931799", 144000, 18230},
		{"bbe1ad9efb4ae8aa69479c21960da5703d0b1c33e1e77a8528b380c673c4d482", "5dcb2c73b1aa513a7168", "ee81445879502e9ed54e607c35d599433fe9c95aa21c4e837c83216808358e8a", "1789842935323", 144000, 82103},
		{"a013f8c0ddc7fbb222c4a4278c6eb219e05417a35968872ae727d2c567f35f8a", "3ed981bd7b91076d8022", "70fdf4ccef914f5518e12b8933d187df4241c23b655b805774627129471502bd", "1789842995871", 144000, 52573},
	}
	for i, v := range vectors {
		ch := Challenge{
			Algorithm:  "DeepSeekHashV1",
			Challenge:  v.challenge,
			Salt:       v.salt,
			Signature:  v.signature,
			TargetPath: CompletionPath,
			Difficulty: v.difficulty,
			ExpireAt:   json.Number(v.expireAt),
		}
		header, err := PowHeader(context.Background(), ch)
		if err != nil {
			t.Fatalf("vector %d: PowHeader: %v", i, err)
		}
		raw, err := base64.StdEncoding.DecodeString(header)
		if err != nil {
			t.Fatalf("vector %d: decode header: %v", i, err)
		}
		var got powPayload
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("vector %d: unmarshal header: %v", i, err)
		}
		if got.Answer != v.want {
			t.Errorf("vector %d (%s…): answer = %d, want %d", i, v.challenge[:8], got.Answer, v.want)
		}
		if got.TargetPath != CompletionPath {
			t.Errorf("vector %d: target_path = %q", i, got.TargetPath)
		}
	}
}
