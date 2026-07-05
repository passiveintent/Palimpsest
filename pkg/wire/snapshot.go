/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

// maxSnapshotDecompressedBytes bounds gzip decompression of an untrusted
// snapshot blob so a crafted small input can't exhaust memory.
const maxSnapshotDecompressedBytes = 64 << 20

// SnapshotEntry is one instance-level ring-buffer sample (ADR-012, ADR-008:
// instance labels ride the ring buffer rather than the sketch).
type SnapshotEntry struct {
	ID    uint64
	TSMs  uint64
	Value float32
}

// EncodeSnapshot serializes entries as count u32 + repeated{id u64,
// ts_ms u64, value f32} (docs/SPEC.md), gzip-compressing the result when
// gzipCompress is true (Frame.Flags bit0, FlagGzip).
func EncodeSnapshot(entries []SnapshotEntry, gzipCompress bool) ([]byte, error) {
	buf := appendU32(nil, uint32(len(entries)))
	for _, e := range entries {
		buf = appendU64(buf, e.ID)
		buf = appendU64(buf, e.TSMs)
		buf = appendF32(buf, e.Value)
	}
	if !gzipCompress {
		return buf, nil
	}
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	if _, err := gw.Write(buf); err != nil {
		return nil, fmt.Errorf("wire: gzip snapshot: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("wire: gzip snapshot: %w", err)
	}
	return out.Bytes(), nil
}

// DecodeSnapshot decodes a blob produced by EncodeSnapshot. gzipped must
// match the FlagGzip bit the frame was marked with.
func DecodeSnapshot(blob []byte, gzipped bool) ([]SnapshotEntry, error) {
	raw := blob
	if gzipped {
		gr, err := gzip.NewReader(bytes.NewReader(blob))
		if err != nil {
			return nil, fmt.Errorf("%w: gunzip snapshot: %v", ErrInvalidFrame, err)
		}
		defer gr.Close()
		decoded, err := io.ReadAll(io.LimitReader(gr, maxSnapshotDecompressedBytes+1))
		if err != nil {
			return nil, fmt.Errorf("%w: gunzip snapshot: %v", ErrInvalidFrame, err)
		}
		if len(decoded) > maxSnapshotDecompressedBytes {
			return nil, fmt.Errorf("%w: snapshot decompresses to more than %d bytes", ErrInvalidFrame, maxSnapshotDecompressedBytes)
		}
		raw = decoded
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
