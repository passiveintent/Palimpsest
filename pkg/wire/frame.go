/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package wire implements the Palimpsest frame wire format: encoding and
// decoding of KEYFRAME, RESIDUAL, and FALLBACK frames as defined in
// docs/SPEC.md.
//
// See ADR-006 (wire format), ADR-008 (dict deltas / dict root), ADR-011
// (KDELTA delta-coded keyframes), ADR-012 (snapshot blobs), and ADR-013
// (emitter_id, window-indexed seq).
/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import "errors"

// ErrInvalidFrame indicates a frame (or a field within it) violates a
// wire-format invariant: bad magic/version, an invalid tombstone, an
// oversized field, or a malformed sub-payload.
var ErrInvalidFrame = errors.New("wire: invalid frame")

// ErrShortBuffer indicates the input buffer ended before a required field
// could be read.
var ErrShortBuffer = errors.New("wire: short buffer")

// ErrCRCMismatch indicates the trailing CRC32C did not match the computed
// checksum of the preceding bytes.
var ErrCRCMismatch = errors.New("wire: crc32c mismatch")

// Magic identifies a Palimpsest wire frame ("PLMP").
var Magic = [4]byte{'P', 'L', 'M', 'P'}

// Version is the wire format version this package writes (docs/SPEC.md v2).
const Version uint8 = 2

// VersionMin is the oldest wire format version this package can decode.
// Versions 1 and 2 are both valid; versions above Version return ErrVersion.
const VersionMin uint8 = 1

// ErrVersion indicates a wire frame whose version byte exceeds Version.
// It is a distinct sentinel from ErrInvalidFrame so callers can distinguish
// "future format we don't understand" from "malformed frame".
var ErrVersion = errors.New("wire: unsupported version")

// Frame types (docs/SPEC.md "Frame Layout").
const (
	FrameTypeKeyframe uint8 = 1
	FrameTypeResidual uint8 = 2
	FrameTypeFallback uint8 = 3
)

// PredictorID identifies the baseline predictor that produced a frame's
// residuals (Frame.Predictor), so a decoder can select a wire-compatible
// implementation (ADR-003).
type PredictorID uint8

// PredictorHold is the v0 HOLD predictor: the baseline is held constant at
// the value last set by a keyframe or dict_delta birth, and only changes
// when the encoder/decoder next agree on one (ADR-003's open-loop coding).
const PredictorHold PredictorID = 0

// Frame-level flag bits.
const (
	// FlagGzip marks the snapshot blob as gzip-compressed (v1 frames only).
	// Deprecated in v2 in favour of the Codec field; kept for v1 decode.
	FlagGzip uint8 = 1 << 0
	// FlagKDelta marks a KEYFRAME payload as delta-coded against the
	// prior keyframe (ADR-011), rather than a Full encoding.
	FlagKDelta uint8 = 1 << 1
)

// Codec identifies the compression scheme applied to a frame's compressible
// bytes (Frame.SnapshotBlob always; Frame.Payload too when FrameType is
// FrameTypeKeyframe — docs/SPEC.md, ADR-006 §Addendum). It is a distinct
// type from the wire's raw uint8 so Register/CompressPayload/
// DecompressPayload (compress.go) can't be called with an arbitrary byte
// that was never meant to be a codec id.
type Codec uint8

// Codec values for Frame.Codec (v2 and later; docs/SPEC.md).
const (
	// CodecNone means no compression is applied.
	CodecNone Codec = 0
	// CodecGzip means gzip compression (compress/gzip, stdlib).
	// Mirrors the deprecated FlagGzip for v1 decode compatibility.
	CodecGzip Codec = 1
	// CodecZstd is zstd compression. pkg/wire has no zstd implementation
	// of its own (ADR-007: pkg/* stays stdlib+xxhash) — a Compressor for
	// it must be Register-ed by a caller outside pkg/* (e.g. the
	// zstdcodec leaf package) before any frame carrying this codec can be
	// encoded or decoded.
	CodecZstd Codec = 2
)

// DictDelta flag bits (ADR-008).
const (
	// DictFlagTombstone marks a dict_delta as evicting an ID. Tombstones
	// MUST carry a zero InitValue and an empty Name.
	DictFlagTombstone uint8 = 1 << 0
)

// Frame is a single Palimpsest wire frame (ADR-006, ADR-008, ADR-011,
// ADR-012, ADR-013). Field order matches the on-wire layout exactly;
// see codec.go for the byte-level encoding.
type Frame struct {
	Magic     [4]byte
	Version   uint8
	FrameType uint8
	Flags     uint8
	Bits      uint8

	EmitterID uint64 // ADR-013: random per agent process; identity is per-frame

	ShardID uint64
	Epoch   uint64
	Seq     uint32 // window index within epoch; never re-derived from wall time

	ViewID    uint16 // declared view tag (ADR-010); 0 for single-view
	M         uint32
	D         uint8
	Predictor uint8
	// KeyVersion identifies the HKDF tenant_key generation used for seed
	// derivation (ADR-012 §Addendum). Introduced in wire v2; v1 frames
	// imply KeyVersion=0 on decode.
	KeyVersion uint8
	// Codec identifies the compression applied to SnapshotBlob and, for
	// FrameTypeKeyframe, Payload too (v2+; ADR-006 §Addendum). Replaces
	// flags.bit0 (FlagGzip) from v1; v1 frames derive Codec from FlagGzip
	// on decode. See compress.go for the registry that maps this value to
	// a Compressor.
	Codec Codec

	Energy     float32
	QuantScale float32
	DictRoot   uint64

	DictDeltas []DictDelta

	SnapshotBlob []byte // optional, ADR-012

	Payload []byte

	CRC32C uint32
}

// DictDelta records a dictionary birth/update/tombstone (ADR-008).
type DictDelta struct {
	ID        uint64
	Flags     uint8 // bit0=TOMBSTONE, bit1=reserved
	InitValue float32
	Name      []byte
}

// IsTombstone reports whether d marks a dictionary eviction.
func (d DictDelta) IsTombstone() bool {
	return d.Flags&DictFlagTombstone != 0
}
