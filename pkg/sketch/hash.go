package sketch

import "github.com/cespare/xxhash/v2"

// phiMix decorrelates the second hash from the first; the same odd 64-bit
// golden-ratio constant used throughout the Palimpsest hashing scheme
// (docs/SPEC.md "Hashing & Phi").
const phiMix uint64 = 0x9E3779B97F4A7C15

// SeriesID returns the persistent logical-series identity: xxh64(name,
// seed=0). This is the value carried as DictDelta.ID and used as the
// dict_root input (ADR-008); it does not depend on the ephemeral per-epoch
// seed, so it is stable across epochs and shared by every emitter that
// reports the same logical series name.
func SeriesID(name []byte) uint64 {
	return xxhash.Sum64(name)
}

// Buckets derives the d hash-implicit Phi columns for name under the given
// ephemeral seed (docs/SPEC.md "Hashing & Phi", ADR-002):
//
//	h1 = xxh64(name, seed)
//	h2 = xxh64(name, seed ^ 0x9E3779B97F4A7C15) | 1
//	bucket[i] = (h1 + i*h2) mod m
//	sign[i]   = +1 if bit (i+1) of h1 is set, else -1
//
// The measurement matrix itself is never materialized: idx/sign are
// recomputed (or cached by the caller) from name alone, so cardinality is
// bounded by the number of *distinct logical series* rather than requiring
// a persisted column assignment.
//
// The returned sign values are the +1/-1 factor only; callers apply the
// 1/sqrt(d) normalization (docs/SPEC.md: "Phi entry value = sign[i] /
// sqrt(d)").
func Buckets(name []byte, seed uint64, m, d int) (idx []uint32, sign []int8) {
	h1 := hashSeeded(name, seed)
	h2 := hashSeeded(name, seed^phiMix) | 1

	idx = make([]uint32, d)
	sign = make([]int8, d)
	mu64 := uint64(m)
	for i := 0; i < d; i++ {
		idx[i] = uint32((h1 + uint64(i)*h2) % mu64)
		if (h1>>uint(i+1))&1 == 1 {
			sign[i] = 1
		} else {
			sign[i] = -1
		}
	}
	return idx, sign
}

// hashSeeded computes xxh64(name, seed).
func hashSeeded(name []byte, seed uint64) uint64 {
	d := xxhash.NewWithSeed(seed)
	d.Write(name)
	return d.Sum64()
}
