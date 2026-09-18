/**
 * Chunk-sizing strategies for the translate engine.
 *
 * Two strategies cover the modes: adaptiveSizer probes a small chunk,
 * learns the output/input byte ratio, and sizes the remaining chunks to fill
 * the model's per-reply output budget (Instant translation); fixedSizer
 * holds one size for the whole run and halves it once on truncation
 * (DeepThink mode, whose ~300K-token replies make the probe unnecessary);
 * summarizeSizer starts at the cap because a summary is far shorter than
 * its input. Truncation handlers return false when the chunk size has hit
 * the floor and the run must give up.
 *
 * Port of internal/translate/sizer.go.
 */

export interface ChunkSizer {
	size(): number;
	success(chunkBytes: number, textBytes: number): void;
	/** Returns false when the floor is reached and the run must give up. */
	truncated(partialBytes: number): boolean;
}

export const MIN_CHUNK_BYTES = 1024;
export const INITIAL_CHUNK_BYTES = 8 * 1024;
export const DEFAULT_CAP_BYTES = 36 * 1024;
export const THINKING_CHUNK_BYTES = 256 * 1024;
export const OUTPUT_CAP_MARGIN = 0.85;
export const BYTES_PER_TOKEN = 4;
export const THINKING_CAP_BYTES = 300_000 * BYTES_PER_TOKEN; // ~1.2 MiB
export const DEFAULT_CHUNK_BYTES = 1 << 20; // 1 MiB

/** idealChunk: size a chunk so its expected output fills the per-reply output budget, bounded by [minChunkBytes, maxChunk]. */
export function idealChunk(capBytes: number, ratio: number, maxChunk: number): number {
	if (ratio <= 0) return maxChunk;
	let n = Math.trunc((capBytes * OUTPUT_CAP_MARGIN) / ratio);
	if (n < MIN_CHUNK_BYTES) n = MIN_CHUNK_BYTES;
	if (n > maxChunk) n = maxChunk;
	return n;
}

/** shrinkChunk: halve, but skip straight to the ratio-based estimate when smaller. Never below minChunkBytes. */
export function shrinkChunk(current: number, capBytes: number, ratio: number): number {
	let est = Math.trunc((capBytes * OUTPUT_CAP_MARGIN) / 4); // assume verbose until learned
	if (ratio > 0) est = Math.trunc((capBytes * OUTPUT_CAP_MARGIN) / ratio);
	let n = Math.trunc(current / 2);
	if (est < n) n = est;
	if (n < MIN_CHUNK_BYTES) n = MIN_CHUNK_BYTES;
	return n;
}

/** adaptiveSizer: probe, learn, grow to fill the output budget, shrink on truncation. */
export class AdaptiveSizer implements ChunkSizer {
	private chunkBytes: number;
	private readonly maxChunk: number;
	private capBytes: number;
	private ratio = 0;
	private growOK: boolean;

	constructor(maxChunk: number, start?: number) {
		let n = start ?? INITIAL_CHUNK_BYTES;
		if (n > maxChunk) n = maxChunk;
		this.chunkBytes = n;
		this.maxChunk = maxChunk;
		this.capBytes = DEFAULT_CAP_BYTES;
		this.growOK = true;
	}

	size(): number {
		return this.chunkBytes;
	}

	success(chunkBytes: number, textBytes: number): void {
		if (this.ratio === 0 && chunkBytes > 0 && textBytes > 0) {
			// The first complete chunk is the lesson; later ones are noisier and
			// can be inflated by markdown-heavy replies.
			this.ratio = textBytes / chunkBytes;
		}
		if (this.ratio > 0) {
			const n = idealChunk(this.capBytes, this.ratio, this.maxChunk);
			if (this.growOK || n < this.chunkBytes) this.chunkBytes = n;
		}
	}

	truncated(partialBytes: number): boolean {
		// Learn the real output cap from the cut-off reply and re-split this
		// offset with a smaller chunk. Growing stops at the first truncation.
		this.growOK = false;
		if (partialBytes > this.capBytes) this.capBytes = partialBytes;
		if (this.chunkBytes <= MIN_CHUNK_BYTES) return false;
		this.chunkBytes = shrinkChunk(this.chunkBytes, this.capBytes, this.ratio);
		return true;
	}
}

/** fixedSizer: one chunk size for the whole run, halved on truncation. */
export class FixedSizer implements ChunkSizer {
	private chunkBytes: number;

	constructor(maxChunk: number) {
		let n = THINKING_CHUNK_BYTES;
		if (n > maxChunk) n = maxChunk;
		this.chunkBytes = n;
	}

	size(): number {
		return this.chunkBytes;
	}

	success(): void {}

	truncated(): boolean {
		if (this.chunkBytes <= MIN_CHUNK_BYTES) return false;
		this.chunkBytes = Math.trunc(this.chunkBytes / 2);
		return true;
	}
}

/** newSizer picks the strategy for a run. Summarize starts at the cap (its output is short, so the input cap never binds). */
export function newSizer(task: string, thinking: boolean, maxChunk: number): ChunkSizer {
	if (task === "summarize") return new AdaptiveSizer(maxChunk, maxChunk);
	if (thinking) return new FixedSizer(maxChunk);
	return new AdaptiveSizer(maxChunk);
}
