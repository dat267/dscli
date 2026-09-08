// Package filetools holds the format- and content-handling helpers shared by
// the translate engine: text/EPUB detection and extraction, and byte-for-byte
// verification of subtitle/lyric structure after a translation.
package filetools

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultMaxReadBytes caps how much extracted EPUB text (or any single
// content read) is kept. Large enough for real books while bounding memory.
const DefaultMaxReadBytes = 512 * 1024

// MaxReadBytes is the runtime read ceiling. It is initialised to
// DefaultMaxReadBytes.
var MaxReadBytes = DefaultMaxReadBytes

// binaryProbeSize is how many leading bytes are inspected to classify a file
// as text vs binary (a NUL byte anywhere in the probe means binary).
const binaryProbeSize = 8000

// IsText reports whether data looks like text: no NUL byte in the probe.
func IsText(data []byte) bool {
	probe := data
	if len(probe) > binaryProbeSize {
		probe = probe[:binaryProbeSize]
	}
	return !bytes.Contains(probe, []byte{0})
}

// ReadEpub extracts the chapter text of an EPUB (ZIP of XHTML) in spine
// order, stripped of markup, capped at MaxReadBytes of extracted text.
func ReadEpub(path string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	chapters, err := epubChapters(&zr.Reader)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, ch := range chapters {
		text, err := stripHTML(ch)
		if err != nil {
			return "", err
		}
		if out.Len()+len(text) > MaxReadBytes {
			out.WriteString(text[:MaxReadBytes-out.Len()])
			break
		}
		out.WriteString(text)
	}
	return out.String(), nil
}

// findZip returns the archive entry with the given name (case-insensitive).
func findZip(zr *zip.Reader, name string) (*zip.File, bool) {
	for _, f := range zr.File {
		if strings.EqualFold(f.Name, name) {
			return f, true
		}
	}
	return nil, false
}

// epubOPFPath reads META-INF/container.xml and returns the OPF path
// (relative to the archive root) that describes the book.
func epubOPFPath(zr *zip.Reader) (string, error) {
	container, ok := findZip(zr, "META-INF/container.xml")
	if !ok {
		return "", fmt.Errorf("missing META-INF/container.xml")
	}
	rc, err := container.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	var ctr struct {
		Rootfiles struct {
			Rootfile []struct {
				FullPath string `xml:"full-path,attr"`
			} `xml:"rootfile"`
		} `xml:"rootfiles"`
	}
	if err := xml.NewDecoder(rc).Decode(&ctr); err != nil {
		return "", fmt.Errorf("container.xml: %w", err)
	}
	if len(ctr.Rootfiles.Rootfile) == 0 {
		return "", fmt.Errorf("container.xml has no rootfile")
	}
	return ctr.Rootfiles.Rootfile[0].FullPath, nil
}

// epubChapters returns the XHTML files of an EPUB in spine order, reading
// META-INF/container.xml → OPF → manifest/spine.
func epubChapters(zr *zip.Reader) ([]string, error) {
	opfPath, err := epubOPFPath(zr)
	if err != nil {
		return nil, err
	}
	opfFile, ok := findZip(zr, opfPath)
	if !ok {
		return nil, fmt.Errorf("OPF %q not found", opfPath)
	}
	rc, err := opfFile.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var opf struct {
		Manifest struct {
			Items []struct {
				ID   string `xml:"id,attr"`
				Href string `xml:"href,attr"`
			} `xml:"item"`
		} `xml:"manifest"`
		Spine struct {
			Itemrefs []struct {
				IDRef string `xml:"idref,attr"`
			} `xml:"itemref"`
		} `xml:"spine"`
	}
	if err := xml.NewDecoder(rc).Decode(&opf); err != nil {
		return nil, fmt.Errorf("OPF: %w", err)
	}
	hrefs := make(map[string]string, len(opf.Manifest.Items))
	for _, it := range opf.Manifest.Items {
		hrefs[it.ID] = it.Href
	}
	base := filepath.Dir(opfFile.Name)
	var chapters []string
	for _, ref := range opf.Spine.Itemrefs {
		href, ok := hrefs[ref.IDRef]
		if !ok {
			continue
		}
		name := filepath.ToSlash(filepath.Join(base, href))
		f, ok := findZip(zr, name)
		if !ok {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(rc, int64(MaxReadBytes+1)))
		rc.Close()
		if err != nil {
			return nil, err
		}
		chapters = append(chapters, string(data))
	}
	return chapters, nil
}

// stripHTML removes markup and decodes common entities, returning visible
// text. Deterministic and deliberately simple — formatting is irrelevant to
// the model.
func stripHTML(s string) (string, error) {
	var r strings.Builder
	r.Grow(len(s))
	inTag := false
	inScript := false
	i := 0
	for i < len(s) {
		switch {
		case inTag:
			if s[i] == '>' {
				inTag = false
			}
		case !inTag && i+7 <= len(s) && strings.EqualFold(s[i:i+6], "<script"):
			inTag = true
			inScript = true
		case inScript && strings.HasPrefix(strings.ToLower(s[i:]), "</script"):
			inScript = false
			inTag = true
		case s[i] == '<':
			inTag = true
		case s[i] == '&':
			end := strings.IndexByte(s[i:], ';')
			if end >= 0 && end <= 8 {
				switch s[i+1 : i+end] {
				case "amp":
					r.WriteByte('&')
				case "lt":
					r.WriteByte('<')
				case "gt":
					r.WriteByte('>')
				case "quot":
					r.WriteByte('"')
				case "#39", "apos":
					r.WriteByte('\'')
				case "nbsp":
					r.WriteByte(' ')
				default:
					r.WriteString(s[i : i+end+1])
				}
				i += end + 1
				continue
			}
			r.WriteByte('&')
		default:
			r.WriteByte(s[i])
		}
		i++
	}
	return r.String(), nil
}

// DetectFormat returns the translation format for a file path/extension:
// "lrc" | "srt" | "vtt" | "ass" | "ttml" | "markdown", else "text".
func DetectFormat(path string, content []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".lrc":
		return "lrc"
	case ".srt":
		return "srt"
	case ".vtt":
		return "vtt"
	case ".ass", ".ssa":
		return "ass"
	case ".ttml":
		return "ttml"
	case ".md", ".markdown":
		return "markdown"
	}
	return "text"
}

// ProtectedLines returns the structural tokens of a format that MUST survive
// a translation byte-for-byte: for LRC every [mm:ss(.xx)] timestamp in order
// (including on lyric lines); for VTT the timing lines and the WEBVTT header.
// Empty for plain text.
func ProtectedLines(format string, content []byte) []string {
	switch format {
	case "lrc":
		return lrcTimecodeRE.FindAllString(string(content), -1)
	case "srt":
		var out []string
		for _, line := range strings.Split(string(content), "\n") {
			if trim := strings.TrimSpace(line); strings.Contains(trim, "-->") {
				out = append(out, trim)
			}
		}
		return out
	case "vtt":
		var out []string
		for _, line := range strings.Split(string(content), "\n") {
			trim := strings.TrimSpace(line)
			if strings.Contains(trim, "-->") || trim == "WEBVTT" || strings.HasPrefix(trim, "NOTE") {
				out = append(out, trim)
			}
		}
		return out
	case "ass":
		var out []string
		diag := regexp.MustCompile(`(?i)^(?:dialogue|comment):`)
		for _, line := range strings.Split(string(content), "\n") {
			trim := strings.TrimSpace(line)
			if trim == "" {
				continue
			}
			if diag.MatchString(trim) {
				out = append(out, assDialoguePrefix(trim))
			} else {
				// Everything outside Dialogue/Comment lines (script info,
				// style headers, Format:, Style:) is structure — never
				// translated, must survive byte-for-byte.
				out = append(out, trim)
			}
		}
		return out
	case "ttml":
		// The ordered sequence of XML tags (with attributes) is the
		// structure; only text content between tags may change.
		return xmlTagRE.FindAllString(string(content), -1)
	}
	return nil
}

// assDialoguePrefix returns the Dialogue/Comment line prefix through the 9th
// comma — Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect —
// which must stay untouched; only the trailing text field is translated.
func assDialoguePrefix(line string) string {
	n := 0
	for i := 0; i < len(line); i++ {
		if line[i] == ',' {
			n++
			if n == 9 {
				return line[:i+1]
			}
		}
	}
	return line
}

// xmlTagRE matches an XML tag including attributes (used for TTML structure).
var xmlTagRE = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)

// lrcTimecodeRE matches an LRC timestamp tag like [01:23.45] or [1:02].
var lrcTimecodeRE = regexp.MustCompile(`\[\d{1,2}:\d{2}(?:\.\d+)?\]`)

// VerifyProtected checks that the protected lines of a translation are
// exactly the protected lines of the original.
func VerifyProtected(format string, original, translated string) error {
	orig := ProtectedLines(format, []byte(original))
	trans := ProtectedLines(format, []byte(translated))
	if len(orig) != len(trans) {
		return fmt.Errorf("protected line count changed (%d → %d); timestamps/headers must stay identical", len(orig), len(trans))
	}
	for i := range orig {
		if orig[i] != trans[i] {
			return fmt.Errorf("protected line %d changed; timestamps/headers must stay identical", i+1)
		}
	}
	return nil
}
