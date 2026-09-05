package translate

// chunkSizer decides the input chunk size as the engine walks the source
// text. Two strategies cover the modes: adaptiveSizer probes a small chunk,
// learns the output/input byte ratio, and sizes the remaining chunks to fill
// the model's per-reply output budget (Instant mode); fixedSizer holds one
// size for the whole run and halves it once on truncation (DeepThink mode,
// whose ~300K-token replies make the probe unnecessary). Truncation handlers
// return false when the chunk size has hit the floor and the run must give
// up.
type chunkSizer interface {
	size() int
	success(chunkBytes, textBytes int)
	truncated(partialBytes int) bool
}

// adaptiveSizer is the Instant-mode strategy: probe, learn, grow to fill the
// output budget, shrink on truncation.
type adaptiveSizer struct {
	chunkBytes int
	maxChunk   int
	capBytes   int
	ratio      float64
	growOK     bool
}

func newAdaptiveSizer(maxChunk int) *adaptiveSizer {
	n := initialChunkBytes
	if n > maxChunk {
		n = maxChunk
	}
	return &adaptiveSizer{
		chunkBytes: n,
		maxChunk:   maxChunk,
		capBytes:   defaultCapBytes,
		growOK:     true,
	}
}

func (s *adaptiveSizer) size() int { return s.chunkBytes }

func (s *adaptiveSizer) success(chunkBytes, textBytes int) {
	if s.ratio == 0 && chunkBytes > 0 && textBytes > 0 {
		// The first complete chunk is the lesson; later ones are noisier and
		// can be inflated by markdown-heavy replies.
		s.ratio = float64(textBytes) / float64(chunkBytes)
	}
	if s.ratio > 0 {
		n := idealChunk(s.capBytes, s.ratio, s.maxChunk)
		if s.growOK || n < s.chunkBytes {
			s.chunkBytes = n
		}
	}
}

func (s *adaptiveSizer) truncated(partialBytes int) bool {
	// Learn the real output cap from the cut-off reply and re-split this
	// offset with a smaller chunk. Growing stops at the first truncation.
	s.growOK = false
	if partialBytes > s.capBytes {
		s.capBytes = partialBytes
	}
	if s.chunkBytes <= minChunkBytes {
		return false
	}
	s.chunkBytes = shrinkChunk(s.chunkBytes, s.capBytes, s.ratio)
	return true
}

// fixedSizer is the DeepThink-mode strategy: one chunk size for the whole
// run, halved once if a reply is still cut off.
type fixedSizer struct {
	chunkBytes int
}

func newFixedSizer(maxChunk int) *fixedSizer {
	n := thinkingChunkBytes
	if n > maxChunk {
		n = maxChunk
	}
	return &fixedSizer{chunkBytes: n}
}

func (s *fixedSizer) size() int { return s.chunkBytes }

func (s *fixedSizer) success(chunkBytes, textBytes int) {}

func (s *fixedSizer) truncated(partialBytes int) bool {
	if s.chunkBytes <= minChunkBytes {
		return false
	}
	s.chunkBytes /= 2
	return true
}
