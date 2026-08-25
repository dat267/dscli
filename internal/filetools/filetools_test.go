package filetools

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsText(t *testing.T) {
	if !IsText([]byte("plain text\nline two")) {
		t.Error("plain text should be text")
	}
	if IsText([]byte("bin\x00ary")) {
		t.Error("NUL byte should mark binary")
	}
	// Large text with no NUL is text (only the probe is inspected).
	if !IsText([]byte(strings.Repeat("a", 100_000))) {
		t.Error("large no-NUL blob should be text")
	}
}

func TestDetectFormat(t *testing.T) {
	cases := map[string]string{
		"a.lrc": "lrc", "b.srt": "srt", "c.vtt": "vtt",
		"d.ass": "ass", "e.ssa": "ass", "f.ttml": "ttml",
		"g.md": "markdown", "h.markdown": "markdown", "i.txt": "text", "j": "text",
	}
	for path, want := range cases {
		if got := DetectFormat(path, nil); got != want {
			t.Errorf("DetectFormat(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestProtectedAndVerify(t *testing.T) {
	orig := "[00:01.00]Line one\n[00:02.50]Line two\n[01:00.00]Line three"
	good := "[00:01.00]첫 줄\n[00:02.50]둘째 줄\n[01:00.00]셋째 줄"
	bad := "[00:01.00]첫 줄\n[00:03.00]둘째 줄\n[01:00.00]셋째 줄" // middle timestamp changed
	if err := VerifyProtected("lrc", orig, good); err != nil {
		t.Errorf("good lrc should verify: %v", err)
	}
	if err := VerifyProtected("lrc", orig, bad); err == nil {
		t.Error("changed timestamp must fail verification")
	}
	// Plain text has no protected lines and always verifies.
	if err := VerifyProtected("text", "hello", "bonjour"); err != nil {
		t.Errorf("text should always verify: %v", err)
	}
}

func TestProtectedLinesAdditionalFormats(t *testing.T) {
	srt := "1\n00:00:01,000 --> 00:00:02,000\nHello\n"
	vtt := "WEBVTT\n\n00:01.000 --> 00:02.000\nalign:start\nHi\n"
	ass := "[Script Info]\nTitle: T\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,Hello\n"
	ttml := `<tt><body><p begin="0s">Hi</p></body></tt>`

	if got := ProtectedLines("srt", []byte(srt)); len(got) != 1 || !strings.Contains(got[0], "-->") {
		t.Errorf("srt protected = %v", got)
	}
	v := ProtectedLines("vtt", []byte(vtt))
	if len(v) != 2 || v[0] != "WEBVTT" {
		t.Errorf("vtt protected = %v", v)
	}
	a := ProtectedLines("ass", []byte(ass))
	if len(a) != 4 {
		t.Errorf("ass protected = %v", a)
	}
	if !strings.Contains(a[3], "Dialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,") {
		t.Errorf("ass dialogue prefix not kept: %q", a[3])
	}
	if got := ProtectedLines("ttml", []byte(ttml)); len(got) != 6 {
		t.Errorf("ttml protected = %v", got)
	}
}

func TestReadEpub(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "book.epub")
	if err := writeTestEPUB(f); err != nil {
		t.Fatal(err)
	}
	text, err := ReadEpub(f)
	if err != nil {
		t.Fatalf("ReadEpub: %v", err)
	}
	for _, want := range []string{"Chapter one content", "Chapter two content"} {
		if !strings.Contains(text, want) {
			t.Errorf("EPUB text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "<p>") || strings.Contains(text, "<html") {
		t.Errorf("markup not stripped:\n%s", text)
	}
}

// writeTestEPUB builds a minimal EPUB: container.xml → OPF → two XHTML
// chapters.
func writeTestEPUB(path string) error {
	ch1 := "<html><body><h1>One</h1><p>Chapter one content &amp; stuff</p></body></html>"
	ch2 := "<html><body><p>Chapter two content</p></body></html>"
	files := map[string]string{
		"META-INF/container.xml": `<?xml version="1.0"?><container><rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`,
		"OEBPS/content.opf": `<?xml version="1.0"?><package><manifest>
			<item id="c1" href="ch1.xhtml" media-type="application/xhtml+xml"/>
			<item id="c2" href="ch2.xhtml" media-type="application/xhtml+xml"/>
			</manifest><spine>
			<itemref idref="c1"/><itemref idref="c2"/>
			</spine></package>`,
		"OEBPS/ch1.xhtml": ch1,
		"OEBPS/ch2.xhtml": ch2,
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(content)); err != nil {
			return err
		}
	}
	return zw.Close()
}
