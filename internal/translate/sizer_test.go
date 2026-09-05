package translate

import "testing"

func TestAdaptiveSizerProbesThenGrows(t *testing.T) {
	s := newAdaptiveSizer(DefaultChunkBytes)
	if s.size() != initialChunkBytes {
		t.Errorf("probe chunk = %d, want %d", s.size(), initialChunkBytes)
	}
	// A tiny reply teaches a near-zero output/input ratio: grow to the max.
	s.success(1000, 2)
	if s.size() != DefaultChunkBytes {
		t.Errorf("after learning, chunk = %d, want max %d", s.size(), DefaultChunkBytes)
	}
}

func TestAdaptiveSizerClampsProbeToMax(t *testing.T) {
	s := newAdaptiveSizer(1024)
	if s.size() != 1024 {
		t.Errorf("probe chunk = %d, want clamped to maxChunk 1024", s.size())
	}
}

func TestAdaptiveSizerShrinksOnTruncation(t *testing.T) {
	s := newAdaptiveSizer(DefaultChunkBytes)
	s.success(1000, 800) // verbose ratio: grows to ~38 KiB
	before := s.size()
	if !s.truncated(40*1024) {
		t.Fatal("truncation above the floor must be retryable")
	}
	if s.size() >= before {
		t.Errorf("size did not shrink: %d -> %d", before, s.size())
	}
}

func TestAdaptiveSizerGivesUpAtFloor(t *testing.T) {
	s := newAdaptiveSizer(DefaultChunkBytes)
	s.chunkBytes = minChunkBytes
	if s.truncated(4096) {
		t.Error("truncation at the minimum chunk size must give up (return false)")
	}
}

func TestAdaptiveSizerNeverGrowsAfterTruncation(t *testing.T) {
	s := newAdaptiveSizer(DefaultChunkBytes)
	s.truncated(40 * 1024)
	shrunk := s.size()
	s.success(1000, 2) // a tiny ratio would want the max again
	if s.size() > shrunk {
		t.Errorf("size grew after a truncation: %d -> %d", shrunk, s.size())
	}
	// But shrinking further is still allowed.
	if !s.truncated(50*1024) || s.size() >= shrunk {
		t.Errorf("truncation must still shrink: %d -> %d", shrunk, s.size())
	}
}

func TestAdaptiveSizerLearnsRatioOnce(t *testing.T) {
	s := newAdaptiveSizer(DefaultChunkBytes)
	s.success(1000, 100) // ratio 0.1
	first := s.size()
	s.success(5000, 5000) // ratio 1.0 must not overwrite the first lesson
	if s.size() != first {
		t.Errorf("ratio re-learned: %d -> %d", first, s.size())
	}
}

func TestFixedSizer(t *testing.T) {
	s := newFixedSizer(10 * thinkingChunkBytes)
	if s.size() != thinkingChunkBytes {
		t.Errorf("fixed chunk = %d, want %d", s.size(), thinkingChunkBytes)
	}
	s.success(1, 1) // no-op: size never grows
	if s.size() != thinkingChunkBytes {
		t.Errorf("fixed chunk changed on success: %d", s.size())
	}
	if !s.truncated(0) {
		t.Error("truncation above the floor must be retryable")
	}
	if s.size() != thinkingChunkBytes/2 {
		t.Errorf("chunk = %d, want halved %d", s.size(), thinkingChunkBytes/2)
	}
}

func TestFixedSizerClampsAndGivesUp(t *testing.T) {
	s := newFixedSizer(minChunkBytes)
	if s.size() != minChunkBytes {
		t.Errorf("fixed chunk = %d, want clamped to maxChunk %d", s.size(), minChunkBytes)
	}
	if s.truncated(0) {
		t.Error("truncation at the minimum chunk size must give up (return false)")
	}
}
