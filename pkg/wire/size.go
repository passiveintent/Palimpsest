/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

// DictBlockRawBytes returns the number of uncompressed bytes the dict_delta
// entries in dds would occupy on the wire: the same quantity EncodedSize
// counts toward a frame's total. Provided as a standalone helper for
// shadow-mode instrumentation (ADR-017 pilot week) and the P16 gate's
// compression-measurement variant, so callers can compare CompressDictBlock
// output against the baseline without re-counting the entire frame.
func DictBlockRawBytes(dds []DictDelta) int {
	n := 0
	for _, dd := range dds {
		n += 8 + 1 + 4 + 2 + len(dd.Name) // id, flags, init_value, name_len, name
	}
	return n
}

// EncodedSize returns the exact number of bytes Marshal(f) would produce,
// computed from field lengths alone: no allocation, no CRC computation, no
// validation. Byte-accounting metrics that need every frame's wire size
// (e.g. the ADR-008 dict_root Merkle/resync evidence gate's full-dict-
// keyframe-bytes-vs-total-wire-bytes measurement) can call this on the hot
// path without paying Marshal's allocation + CRC32C cost per frame.
//
// The field order and sizes here mirror Marshal (codec.go) exactly; a
// change to the wire layout must update both, and TestEncodedSizeMatchesMarshal
// (size_test.go) fails if they ever drift apart.
func EncodedSize(f *Frame) int {
	n := 4 + // magic
		1 + 1 + 1 + 1 + // version, frame_type, flags, bits
		8 + 8 + 8 + 4 + // emitter_id, shard_id, epoch, seq
		2 + 4 + 1 + 1 + 1 + 1 + // view_id, m, d, predictor, key_version, codec
		4 + 4 + 8 + // energy, quant_scale, dict_root
		4 // dict_delta_count

	for _, dd := range f.DictDeltas {
		n += 8 + 1 + 4 + 2 + len(dd.Name) // id, flags, init_value, name_len, name
	}

	n += 4 + len(f.SnapshotBlob) // snapshot_blob_len, snapshot_blob
	n += 4 + len(f.Payload)      // payload_len, payload
	n += 4                       // crc32c

	return n
}
