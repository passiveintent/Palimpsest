/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import "math"

// This file is the single shared assembly path for a KEYFRAME/RESIDUAL
// frame's payload-bearing fields, consumed by every encoder implementation
// (cmd/plsim/encoder.go, otel/processor/csresidual/processor.go, and
// internal/core's test encoder). Before this existed, all three carried
// their own hand-copied version of this logic -- and the golden-keyframe
// tombstone-drop bug (docs/adr/ADR-017-merkle-decision.md's "Discovered
// defect", closed by docs/adr/ADR-008-ephemeral-cardinality.md's "Ownership
// & Eviction" amendment) shipped identically in all three because there was
// no single place to fix it once. A caller-specific concern (panic vs. log
// vs. degrade-and-continue on error) is left to each caller: these
// functions only ever return an error, never panic or log.

// MinQuantScale floors an adaptive quantization scale so an all-zero
// (perfectly quiet) window never produces an invalid (zero) Quantize/
// EncodeKeyframe scale.
const MinQuantScale float32 = 1e-6

// AdaptiveScale picks the smallest RESIDUAL quantization scale that keeps
// y's largest-magnitude value from clamping against Quantize's int8/int16
// range: scale = max|y| / representable-limit, floored at MinQuantScale.
func AdaptiveScale(y []float64, bits int) float32 {
	var maxAbs float64
	for _, v := range y {
		if a := math.Abs(v); a > maxAbs {
			maxAbs = a
		}
	}
	limit := 127.0
	if bits == 16 {
		limit = 32767.0
	}
	return scaleFor(maxAbs, limit)
}

// AdaptiveKDeltaScale picks the smallest KDELTA quantization scale that
// keeps the largest keyframe-to-keyframe change (cur vs. prev; a missing
// prev entry, i.e. a birth, deltas against 0) from clamping.
func AdaptiveKDeltaScale(cur, prev map[uint64]float32) float32 {
	var maxAbs float64
	for id, v := range cur {
		d := math.Abs(float64(v - prev[id]))
		if d > maxAbs {
			maxAbs = d
		}
	}
	return scaleFor(maxAbs, 32767.0)
}

func scaleFor(maxAbs, steps float64) float32 {
	if maxAbs <= 0 {
		return MinQuantScale
	}
	s := float32(maxAbs / steps)
	if s <= 0 {
		return MinQuantScale
	}
	return s
}

// KeyframeBuildInput bundles everything one flush window needs to assemble
// a KEYFRAME frame's fields, gathered from that window's Tracker/Accumulator
// state by the caller.
type KeyframeBuildInput struct {
	// Golden selects the Full encoding (self-describing, decodable without
	// prior state) over KDELTA (delta-coded against PrevKeyframeValues).
	Golden bool
	// Codec compresses the encoded keyframe payload (ADR-006 §Addendum).
	Codec Codec

	// IDs is the active-ID set this keyframe is built over (e.g.
	// Tracker.ActiveIDs()), sorted ascending.
	IDs []uint64
	// Values holds every id in IDs' current value (e.g.
	// Tracker.CurrentValues()).
	Values             map[uint64]float64
	PrevKeyframeValues map[uint64]float32

	// DictDeltas is this window's births ++ tombstones, used verbatim when
	// Golden is false.
	DictDeltas []DictDelta
	// FullDict is Tracker.FullDict(): every currently-active series as an
	// add-only dict_delta. Used (Golden only) as the base of DictDeltas.
	FullDict []DictDelta
	// Tombstones is this same window's Tracker.Expire() output. ADR-008
	// amendment: a golden keyframe must append these explicitly alongside
	// FullDict's adds -- FullDict alone silently omits any ID that expired
	// in this exact window, which a decoder can never distinguish from
	// "still alive, just not itemized this frame."
	Tombstones []DictDelta
}

// KeyframeBuildResult is the resulting KEYFRAME frame's payload-bearing
// fields, ready to assign onto a wire.Frame. Values32 is returned for the
// caller's own bookkeeping (next window's PrevKeyframeValues, and the
// open-loop predictor's LoadKeyframe per ADR-003).
type KeyframeBuildResult struct {
	Payload    []byte
	Flags      uint8
	QuantScale float32
	DictRoot   uint64
	DictDeltas []DictDelta
	Values32   map[uint64]float32
}

// BuildKeyframe assembles a KEYFRAME frame's fields from in. It is the
// shared implementation of the pattern every encoder needs: convert
// float64 values to float32, pick an adaptive KDELTA scale, encode and
// compress the payload, compute the dict_root, and -- the ADR-008
// amendment's fix -- assemble DictDeltas as FullDict()'s adds ++ this
// window's own tombstones on a golden keyframe, never FullDict() alone.
func BuildKeyframe(in KeyframeBuildInput) (KeyframeBuildResult, error) {
	values32 := make(map[uint64]float32, len(in.Values))
	for id, v := range in.Values {
		values32[id] = float32(v)
	}

	scale := AdaptiveKDeltaScale(values32, in.PrevKeyframeValues)
	payload, flags, err := EncodeKeyframe(in.IDs, values32, in.PrevKeyframeValues, in.Golden, scale)
	if err != nil {
		return KeyframeBuildResult{}, err
	}
	payload, err = CompressPayload(in.Codec, payload)
	if err != nil {
		return KeyframeBuildResult{}, err
	}

	res := KeyframeBuildResult{
		Payload:    payload,
		Flags:      flags,
		QuantScale: scale,
		DictRoot:   ComputeDictRoot(in.IDs),
		Values32:   values32,
	}
	if in.Golden {
		res.DictDeltas = append(append([]DictDelta(nil), in.FullDict...), in.Tombstones...)
	} else {
		res.DictDeltas = in.DictDeltas
	}
	return res, nil
}

// ResidualBuildInput bundles what one flush window needs to assemble a
// RESIDUAL frame's fields.
type ResidualBuildInput struct {
	Y          []float64
	Bits       uint8
	DictDeltas []DictDelta
	// IDs is the active-ID set the dict_root is computed over (e.g.
	// Tracker.ActiveIDs(), post-Expire).
	IDs []uint64
}

// ResidualBuildResult is the resulting RESIDUAL frame's payload-bearing
// fields, ready to assign onto a wire.Frame.
type ResidualBuildResult struct {
	Payload    []byte
	QuantScale float32
	DictRoot   uint64
	DictDeltas []DictDelta
}

// BuildResidual assembles a RESIDUAL frame's fields from in: an adaptive
// scale, the quantized payload, and the dict_root.
func BuildResidual(in ResidualBuildInput) (ResidualBuildResult, error) {
	scale := AdaptiveScale(in.Y, int(in.Bits))
	payload, err := Quantize(in.Y, in.Bits, scale)
	if err != nil {
		return ResidualBuildResult{}, err
	}
	return ResidualBuildResult{
		Payload:    payload,
		QuantScale: scale,
		DictRoot:   ComputeDictRoot(in.IDs),
		DictDeltas: in.DictDeltas,
	}, nil
}
