/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package zstdcodec

import (
	"fmt"
	"testing"

	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// goldenKeyframePayload builds an n-series Full (golden) KEYFRAME payload —
// docs/PERF.md's standard N=50,000 recovery scale, matching a realistic
// plsim-scale fleet — for the docs/PERF.md "zstd vs gzip vs none" codec
// comparison (ADR-006 §Addendum acceptance criterion).
func goldenKeyframePayload(n int) []byte {
	ids := make([]uint64, n)
	values := make(map[uint64]float32, n)
	for i := 0; i < n; i++ {
		id := uint64(i + 1)
		ids[i] = id
		// A realistic steady-state fleet: mostly-similar values clustered
		// around a per-series baseline, not all-identical (all-identical
		// bytes would flatter gzip/zstd's ratio than real telemetry) and
		// not uniform random (real metrics aren't white noise either).
		values[id] = float32(100 + (i%97)-48)
	}
	payload, _, err := wire.EncodeKeyframe(ids, values, nil, true /* golden */, 0)
	if err != nil {
		panic(err)
	}
	return payload
}

const benchN = 50000

// TestCodecSizeComparison reports docs/PERF.md's "bytes" half of the zstd
// vs gzip vs none comparison: not a benchmark (byte size is deterministic,
// not something -bench's timing loop adds value to), matching this repo's
// existing "measured directly, not benchmarked" convention for keyframe
// byte counts (docs/PERF.md "Keyframe bytes/series").
func TestCodecSizeComparison(t *testing.T) {
	if err := Register(); err != nil {
		t.Fatalf("Register: %v", err)
	}
	raw := goldenKeyframePayload(benchN)
	t.Logf("raw golden keyframe payload (N=%d): %d bytes (%.2f B/series)", benchN, len(raw), float64(len(raw))/benchN)

	for _, codec := range []wire.Codec{wire.CodecNone, wire.CodecGzip, wire.CodecZstd} {
		compressed, err := wire.CompressPayload(codec, raw)
		if err != nil {
			t.Fatalf("CompressPayload(codec=%d): %v", codec, err)
		}
		decompressed, err := wire.DecompressPayload(codec, compressed, wire.MaxDecompressedBytes)
		if err != nil {
			t.Fatalf("DecompressPayload(codec=%d): %v", codec, err)
		}
		if len(decompressed) != len(raw) {
			t.Fatalf("codec=%d: round trip length = %d, want %d", codec, len(decompressed), len(raw))
		}
		t.Logf("codec=%d: %d bytes (%.2f B/series, %.1f%% of raw)", codec, len(compressed), float64(len(compressed))/benchN, 100*float64(len(compressed))/float64(len(raw)))
	}
}

func BenchmarkCompress(b *testing.B) {
	if err := Register(); err != nil {
		b.Fatalf("Register: %v", err)
	}
	raw := goldenKeyframePayload(benchN)
	for _, codec := range []wire.Codec{wire.CodecNone, wire.CodecGzip, wire.CodecZstd} {
		b.Run(fmt.Sprintf("codec=%d", codec), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for i := 0; i < b.N; i++ {
				if _, err := wire.CompressPayload(codec, raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDecompress(b *testing.B) {
	if err := Register(); err != nil {
		b.Fatalf("Register: %v", err)
	}
	raw := goldenKeyframePayload(benchN)
	for _, codec := range []wire.Codec{wire.CodecNone, wire.CodecGzip, wire.CodecZstd} {
		compressed, err := wire.CompressPayload(codec, raw)
		if err != nil {
			b.Fatalf("CompressPayload(codec=%d): %v", codec, err)
		}
		b.Run(fmt.Sprintf("codec=%d", codec), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for i := 0; i < b.N; i++ {
				if _, err := wire.DecompressPayload(codec, compressed, wire.MaxDecompressedBytes); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
