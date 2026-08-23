package cmd

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// translatedPairRE matches a translation file name following the name-coding
// convention: <base>.translated[.<lang>].<ext>.
var translatedPairRE = regexp.MustCompile(`^(.*)\.translated(?:\.([a-z0-9]{1,8}))?\.([^.]+)$`)

// maxPairsEntries bounds the scan for huge directories.
const maxPairsEntries = 20000

// PairsCmd implements `dscli pairs`: lists original/translation file pairs
// as tab-separated lines (base, lang, path) so a content-serving tool can
// group the two (or more) forms of the same base name. lang is "-" for the
// original.
type PairsCmd struct {
	Dir string `arg:"" optional:"" help:"Directory to scan (default: .)"`
}

func (c *PairsCmd) Run(app *App, ctx context.Context) error {
	dir := c.Dir
	if dir == "" {
		dir = "."
	}
	type line struct{ base, lang, path string }
	var lines []line
	emittedOriginals := map[string]bool{}
	count := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if count >= maxPairsEntries {
			return fs.SkipAll
		}
		if d.IsDir() {
			return nil
		}
		count++
		m := translatedPairRE.FindStringSubmatch(d.Name())
		if m == nil {
			return nil // not a translation
		}
		base, lang, ext := m[1], m[2], m[3]

		// The original (same base, no suffix) closes the pair when present.
		orig := filepath.Join(filepath.Dir(path), base+"."+ext)
		if _, oerr := os.Stat(orig); oerr == nil && !emittedOriginals[orig] {
			emittedOriginals[orig] = true
			lines = append(lines, line{base, "-", orig})
		}
		lines = append(lines, line{base, lang, path})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].base != lines[j].base {
			return lines[i].base < lines[j].base
		}
		return lines[i].lang < lines[j].lang
	})
	for _, l := range lines {
		fmt.Printf("%s\t%s\t%s\n", l.base, l.lang, l.path)
	}
	return nil
}
