/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
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
 * This source code is licensed under the AGPL-3.0-only license found in the
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

// Version is the only wire format version this package understands.
const Version uint8 = 1

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
	// FlagGzip marks the snapshot blob as gzip-compressed.
	FlagGzip uint8 = 1 << 0
	// FlagKDelta marks a KEYFRAME payload as delta-coded against the
	// prior keyframe (ADR-011), rather than a Full encoding.
	FlagKDelta uint8 = 1 << 1
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
	Reserved  uint8

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
