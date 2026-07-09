/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// EncodeKeyframe encodes the payload for a KEYFRAME frame (docs/SPEC.md,
// ADR-011).
//
// ids must be sorted ascending and list every dictionary ID active in
// this (emitter, epoch) after this frame's DictDeltas are applied; values
// must have an entry for every id.
//
// If golden is true, the Full encoding is used: count u32 + repeated{id
// u64, value f32}. Golden keyframes are self-describing and let a
// decoder bootstrap without prior state (ADR-013's "new streams open
// with golden keyframe + full dict").
//
// If golden is false, the KDELTA encoding is used: prev is the value map
// decoded from the immediately preceding keyframe for this stream. IDs
// whose value differs from prev (or that are absent from prev, i.e.
// births) are marked "changed"; each changed value is delta-coded
// against prev (0 for births) using scale as the quantization step,
// matching the RESIDUAL quantization in quant.go.
//
// The changed-bitmap and per-delta encoding are LEB128-style varints:
// this package's specific choice for the otherwise bit-level-unspecified
// "changed_bitmap (varint)" wording in docs/SPEC.md. See
// encodeChangedBitmap for the exact scheme.
//
// Returns the payload and the Frame.Flags bit to OR in (FlagKDelta when
// delta-coded, 0 for Full).
func EncodeKeyframe(ids []uint64, values map[uint64]float32, prev map[uint64]float32, golden bool, scale float32) ([]byte, uint8, error) {
	if !sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }) {
		return nil, 0, fmt.Errorf("%w: keyframe ids must be sorted ascending", ErrInvalidFrame)
	}

	if golden {
		buf := appendU32(nil, uint32(len(ids)))
		for _, id := range ids {
			v, ok := values[id]
			if !ok {
				return nil, 0, fmt.Errorf("%w: keyframe id %d missing value", ErrInvalidFrame, id)
			}
			buf = appendU64(buf, id)
			buf = appendF32(buf, v)
		}
		return buf, 0, nil
	}

	if scale <= 0 || math.IsNaN(float64(scale)) || math.IsInf(float64(scale), 0) {
		return nil, 0, fmt.Errorf("%w: invalid quant_scale %v for KDELTA", ErrInvalidFrame, scale)
	}

	changed := make([]bool, len(ids))
	for i, id := range ids {
		v, ok := values[id]
		if !ok {
			return nil, 0, fmt.Errorf("%w: keyframe id %d missing value", ErrInvalidFrame, id)
		}
		if pv, existed := prev[id]; !existed || pv != v {
			changed[i] = true
		}
	}

	buf := encodeChangedBitmap(changed)
	var tmp [binary.MaxVarintLen64]byte
	for i, id := range ids {
		if !changed[i] {
			continue
		}
		delta := int64(math.Round(float64(values[id]-prev[id]) / float64(scale)))
		n := binary.PutUvarint(tmp[:], zigzagEncode(delta))
		buf = append(buf, tmp[:n]...)
	}
	return buf, FlagKDelta, nil
}

// DecodeKeyframe decodes a KEYFRAME payload produced by EncodeKeyframe.
//
// For Full payloads (kdelta=false) the result is read directly from the
// payload; ids/prev/scale are ignored.
//
// For KDELTA payloads (kdelta=true), ids must be the same sorted ID list
// used at encode time and prev the same prior-keyframe value map: the
// decoder reapplies the encoded deltas on top of prev (ADR-011). An
// unchanged id that is absent from prev is rejected as an orphaned delta
// (ADR-013: "no orphaned deltas" — every stream must start from a golden
// keyframe). The returned map contains exactly the IDs in ids.
func DecodeKeyframe(payload []byte, kdelta bool, ids []uint64, prev map[uint64]float32, scale float32) (map[uint64]float32, error) {
	if !kdelta {
		r := &reader{b: payload}
		count, err := r.u32()
		if err != nil {
			return nil, err
		}
		out := make(map[uint64]float32, min(int(count), r.remaining()/12+1))
		for i := uint32(0); i < count; i++ {
			id, err := r.u64()
			if err != nil {
				return nil, err
			}
			v, err := r.f32()
			if err != nil {
				return nil, err
			}
			out[id] = v
		}
		if r.remaining() != 0 {
			return nil, fmt.Errorf("%w: keyframe has %d trailing bytes", ErrInvalidFrame, r.remaining())
		}
		return out, nil
	}

	if ids == nil {
		return nil, fmt.Errorf("%w: KDELTA decode requires ids", ErrInvalidFrame)
	}
	if !sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }) {
		return nil, fmt.Errorf("%w: KDELTA ids must be sorted ascending", ErrInvalidFrame)
	}
	if scale <= 0 || math.IsNaN(float64(scale)) || math.IsInf(float64(scale), 0) {
		return nil, fmt.Errorf("%w: invalid quant_scale %v for KDELTA", ErrInvalidFrame, scale)
	}

	r := &reader{b: payload}
	changed, err := decodeChangedBitmap(r, len(ids))
	if err != nil {
		return nil, err
	}

	out := make(map[uint64]float32, len(ids))
	for i, id := range ids {
		if !changed[i] {
			pv, existed := prev[id]
			if !existed {
				return nil, fmt.Errorf("%w: KDELTA id %d unchanged but absent from prior keyframe (orphaned delta)", ErrInvalidFrame, id)
			}
			out[id] = pv
			continue
		}
		zz, n := binary.Uvarint(r.b[r.pos:])
		if n <= 0 {
			return nil, fmt.Errorf("%w: KDELTA malformed delta varint for id %d", ErrInvalidFrame, id)
		}
		r.pos += n
		delta := zigzagDecode(zz)
		out[id] = prev[id] + float32(float64(delta)*float64(scale))
	}
	if r.remaining() != 0 {
		return nil, fmt.Errorf("%w: KDELTA has %d trailing bytes", ErrInvalidFrame, r.remaining())
	}
	return out, nil
}

// encodeChangedBitmap encodes a per-index changed mask as a variable
// length, base-128 little-endian bitmap: 7 mask bits per byte (LSB
// first), continuation bit (0x80) set on every byte but the last. Trailing
// unset bits beyond the highest set index are omitted; an all-false mask
// encodes as a single zero byte.
func encodeChangedBitmap(changed []bool) []byte {
	last := -1
	for i, c := range changed {
		if c {
			last = i
		}
	}
	if last == -1 {
		return []byte{0x00}
	}
	nGroups := last/7 + 1
	out := make([]byte, 0, nGroups)
	for g := 0; g < nGroups; g++ {
		var group byte
		for b := 0; b < 7; b++ {
			idx := g*7 + b
			if idx < len(changed) && changed[idx] {
				group |= 1 << uint(b)
			}
		}
		if g < nGroups-1 {
			group |= 0x80
		}
		out = append(out, group)
	}
	return out
}

// decodeChangedBitmap reads a bitmap written by encodeChangedBitmap for n
// tracked indices, guarding against unbounded continuation chains in
// malformed/fuzzed input.
func decodeChangedBitmap(r *reader, n int) ([]bool, error) {
	changed := make([]bool, n)
	bitPos := 0
	maxGroups := n/7 + 2
	for g := 0; ; g++ {
		if g >= maxGroups {
			return nil, fmt.Errorf("%w: changed_bitmap continuation chain too long", ErrInvalidFrame)
		}
		bb, err := r.u8()
		if err != nil {
			return nil, err
		}
		for b := 0; b < 7; b++ {
			idx := bitPos + b
			if idx < n && bb&(1<<uint(b)) != 0 {
				changed[idx] = true
			}
		}
		bitPos += 7
		if bb&0x80 == 0 {
			break
		}
	}
	return changed, nil
}

func zigzagEncode(v int64) uint64 { return uint64((v << 1) ^ (v >> 63)) }

func zigzagDecode(v uint64) int64 { return int64(v>>1) ^ -int64(v&1) }
