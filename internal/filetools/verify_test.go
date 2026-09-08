package filetools

import (
	"strings"
	"testing"
)

// TestVerifyCompleteLineFormats: markdown and subtitle formats must translate
// every content line — a dropped or merged line fails, while blank-line
// changes are tolerated (models add/drop separators).
func TestVerifyCompleteLineFormats(t *testing.T) {
	cases := []struct {
		format string
		orig   string
		bad    string // missing one content line
	}{
		{"md", "# Title\n\nFirst paragraph.\n\nSecond paragraph.\n", "# Title\n\nFirst paragraph.\n"},
		{"srt", "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n2\n00:00:03,000 --> 00:00:04,000\nBye\n",
			"1\n00:00:01,000 --> 00:00:02,000\n\n2\n00:00:03,000 --> 00:00:04,000\nBye\n"},
		{"vtt", "WEBVTT\n\n00:01.000 --> 00:02.000\nHello\n\n00:03.000 --> 00:04.000\nBye\n",
			"WEBVTT\n\n00:01.000 --> 00:02.000\nHello"},
		{"lrc", "[00:01.00]First line\n[00:02.00]Second line\n", "[00:01.00]First line\n"},
		{"ass", "[Script Info]\nTitle: x\n\n[Events]\nDialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,Hello\nDialogue: 0,0:00:03.00,0:00:04.00,Default,,0,0,0,,Bye\n",
			"[Script Info]\nTitle: x\n\n[Events]\nDialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,Hello\n"},
	}
	for _, c := range cases {
		if err := VerifyComplete(c.format, c.orig, c.orig); err != nil {
			t.Errorf("%s: identical text should pass: %v", c.format, err)
		}
		// Blank lines may come and go; content lines may not.
		blanked := strings.ReplaceAll(c.orig, "\n\n", "\n\n\n")
		if err := VerifyComplete(c.format, c.orig, blanked); err != nil {
			t.Errorf("%s: blank-line changes should pass: %v", c.format, err)
		}
		if err := VerifyComplete(c.format, c.orig, c.bad); err == nil {
			t.Errorf("%s: dropped content line should fail", c.format)
		}
	}
}

// TestVerifyCompleteRatioFormats: for reflowing formats (txt, epub) a line
// count is meaningless — instead the output length must stay within a broad
// band of the source, so a vanished paragraph fails but legitimate
// cross-lingual shrink/grow (EN→ZH ≈ 0.3×) passes.
func TestVerifyCompleteRatioFormats(t *testing.T) {
	orig := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 40) // ~1.9KB prose
	if err := VerifyComplete("txt", orig, orig); err != nil {
		t.Errorf("identical text should pass: %v", err)
	}
	zh := strings.Repeat("敏捷的棕色狐狸跳过了懒狗。", 45) // ≈0.3× byte length
	if err := VerifyComplete("txt", orig, zh); err != nil {
		t.Errorf("legitimate cross-lingual shrink should pass: %v", err)
	}
	// The band only catches gross omissions: a 50% loss is inside it (a
	// legitimate EN→ZH shrink sits at ≈0.3×), losing 90% is not.
	if err := VerifyComplete("txt", orig, orig[:len(orig)*50/100]); err != nil {
		t.Errorf("50%% loss is within the band for cross-lingual shrink: %v", err)
	}
	if err := VerifyComplete("txt", orig, orig[:len(orig)*10/100]); err == nil {
		t.Error("90%% loss should fail")
	}
	if err := VerifyComplete("txt", orig, orig+orig+orig); err == nil {
		t.Error("tripled content should fail")
	}
	// Tiny texts are noisy; skip the ratio check there.
	if err := VerifyComplete("txt", "short", ""); err != nil {
		t.Errorf("tiny text should skip the ratio check: %v", err)
	}
}

// TestVerifyCompleteTTML: the tag-sequence protection already detects dropped
// elements, so the completeness check adds nothing — it must not reject
// legitimate reflow.
func TestVerifyCompleteTTML(t *testing.T) {
	orig := "<tt>\n  <body>\n    <p>Hello</p>\n  </body>\n</tt>"
	if err := VerifyComplete("ttml", orig, "<tt><body><p>Hallo</p></body></tt>"); err != nil {
		t.Errorf("ttml reflow should pass: %v", err)
	}
}
