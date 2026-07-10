/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import "fmt"

// MaxDecompressedBytes bounds decompression of untrusted compressed bytes (a
// SnapshotBlob, or a KEYFRAME Payload) so a crafted small input can't
// exhaust memory ("zip bomb"), regardless of which Codec produced it.
// Exported so callers decompressing a KEYFRAME Payload themselves (e.g.
// internal/core's decode path) can bound it the same way EncodeSnapshot/
// DecodeSnapshot do.
const MaxDecompressedBytes = 64 << 20

// SnapshotEntry is one instance-level ring-buffer sample (ADR-012, ADR-008:
// instance labels ride the ring buffer rather than the sketch).
type SnapshotEntry struct {
	ID    uint64
	TSMs  uint64
	Value float32
}

// EncodeSnapshot serializes entries as count u32 + repeated{id u64,
// ts_ms u64, value f32} (docs/SPEC.md), then compresses the result with
// the Compressor registered for codec (Register; ADR-006 §Addendum).
// Returns ErrUnregisteredCodec if codec has none registered.
func EncodeSnapshot(entries []SnapshotEntry, codec Codec) ([]byte, error) {
	buf := appendU32(nil, uint32(len(entries)))
	for _, e := range entries {
		buf = appendU64(buf, e.ID)
		buf = appendU64(buf, e.TSMs)
		buf = appendF32(buf, e.Value)
	}
	return CompressPayload(codec, buf)
}

// DecodeSnapshot decodes a blob produced by EncodeSnapshot. codec must
// match the Codec the frame carrying blob was marked with (Frame.Codec).
func DecodeSnapshot(blob []byte, codec Codec) ([]SnapshotEntry, error) {
	raw, err := DecompressPayload(codec, blob, MaxDecompressedBytes)
	if err != nil {
		return nil, err
	}

	r := &reader{b: raw}
	count, err := r.u32()
	if err != nil {
		return nil, err
	}
	entries := make([]SnapshotEntry, 0, min(int(count), r.remaining()/20+1))
	for i := uint32(0); i < count; i++ {
		id, err := r.u64()
		if err != nil {
			return nil, err
		}
		ts, err := r.u64()
		if err != nil {
			return nil, err
		}
		v, err := r.f32()
		if err != nil {
			return nil, err
		}
		entries = append(entries, SnapshotEntry{ID: id, TSMs: ts, Value: v})
	}
	if r.remaining() != 0 {
		return nil, fmt.Errorf("%w: snapshot has %d trailing bytes", ErrInvalidFrame, r.remaining())
	}
	return entries, nil
}
