package deepseek

// Minimal Keccak-f[1600] used only to generate golden PoW challenge test
// vectors offline. DeepSeekHashV1 uses a non-standard variant: SHA3-style
// 0x06 domain padding with only 23 permutation rounds (rounds 1..23,
// skipping round 0). This mirrors aiodeepseek's reverse-engineered C++
// solver, which targets the same wasm module.

func keccakRoundConstants() [24]uint64 {
	return [24]uint64{
		0x0000000000000001, 0x0000000000008082, 0x800000000000808a, 0x8000000080008000,
		0x000000000000808b, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
		0x000000000000008a, 0x0000000000000088, 0x0000000080008009, 0x000000008000000a,
		0x000000008000808b, 0x800000000000008b, 0x8000000000008089, 0x8000000000008003,
		0x8000000000008002, 0x8000000000000080, 0x000000000000800a, 0x800000008000000a,
		0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
	}
}

// rhoOffsets returns r[x][y] in x+5*y lane order.
func rhoOffsets() [25]uint64 {
	return [25]uint64{
		0, 1, 62, 28, 27, // y=0
		36, 44, 6, 55, 20, // y=1
		3, 10, 43, 25, 39, // y=2
		41, 45, 15, 21, 8, // y=3
		18, 2, 61, 56, 14, // y=4
	}
}

func rotl64(x uint64, n uint64) uint64 {
	if n == 0 {
		return x
	}
	return x<<n | x>>(64-n)
}

// keccakF runs `rounds` rounds of Keccak-f[1600] starting at firstRound.
func keccakF(state *[25]uint64, firstRound, rounds int) {
	rc := keccakRoundConstants()
	rho := rhoOffsets()
	var C, D, B [25]uint64
	for r := firstRound; r < firstRound+rounds; r++ {
		// Theta
		for x := 0; x < 5; x++ {
			C[x] = state[x] ^ state[x+5] ^ state[x+10] ^ state[x+15] ^ state[x+20]
		}
		for x := 0; x < 5; x++ {
			D[x] = C[(x+4)%5] ^ rotl64(C[(x+1)%5], 1)
		}
		for x := 0; x < 5; x++ {
			for y := 0; y < 5; y++ {
				state[x+5*y] ^= D[x]
			}
		}
		// Rho + Pi: B[y][2x+3y] = rotl(A[x][y], rho[x][y])
		for x := 0; x < 5; x++ {
			for y := 0; y < 5; y++ {
				B[y+5*((2*x+3*y)%5)] = rotl64(state[x+5*y], rho[x+5*y])
			}
		}
		// Chi
		for x := 0; x < 5; x++ {
			for y := 0; y < 5; y++ {
				state[x+5*y] = B[x+5*y] ^ (^B[(x+1)%5+5*y] & B[(x+2)%5+5*y])
			}
		}
		// Iota
		state[0] ^= rc[r]
	}
}

// keccakHash hashes data with the given domain byte and round window,
// producing a 32-byte digest (Keccak-256 rate, 136-byte blocks).
func keccakHash(data []byte, domain byte, firstRound, rounds int) [32]byte {
	const rate = 136 // 1600 - 2*256 bits
	var state [25]uint64
	var block [rate]byte
	copy(block[:], data)
	block[len(data)] = domain
	block[rate-1] |= 0x80
	for i := 0; i < rate; i += 8 {
		var lane uint64
		for j := 0; j < 8; j++ {
			lane |= uint64(block[i+j]) << (8 * j)
		}
		state[i/8] ^= lane
	}
	keccakF(&state, firstRound, rounds)
	var out [32]byte
	for i := 0; i < 4; i++ {
		lane := state[i]
		for j := 0; j < 8; j++ {
			out[8*i+j] = byte(lane >> (8 * j))
		}
	}
	return out
}

// keccak256v24 is standard legacy Keccak-256 (0x01 domain, 24 rounds), used
// to sanity-check the permutation against known vectors.
func keccak256v24(data []byte) [32]byte { return keccakHash(data, 0x01, 0, 24) }

// deepseekHashV1 is DeepSeek's variant: 0x06 domain, rounds 1..23.
func deepseekHashV1(data []byte) [32]byte { return keccakHash(data, 0x06, 1, 23) }