/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import (
	"fmt"
	"sort"
)

// FallbackResult is the decoded payload of a FALLBACK frame (ADR-004):
// heavy-hitters plus aggregates, sent when pre-quantization energy spikes
// (storm) before recovery would corrupt.
type FallbackResult struct {
	Values     map[uint64]float32
	TotalSum   float64
	TotalCount uint64
}

// EncodeFallback encodes a FALLBACK payload: k u32 + repeated{id u64,
// value f32} + total_sum f64 + total_count u64. ids must be sorted
// ascending and values must have an entry for every id.
func EncodeFallback(ids []uint64, values map[uint64]float32, totalSum float64, totalCount uint64) ([]byte, error) {
	if !sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }) {
		return nil, fmt.Errorf("%w: fallback ids must be sorted ascending", ErrInvalidFrame)
	}
	buf := appendU32(nil, uint32(len(ids)))
	for _, id := range ids {
		v, ok := values[id]
		if !ok {
			return nil, fmt.Errorf("%w: fallback id %d missing value", ErrInvalidFrame, id)
		}
		buf = appendU64(buf, id)
		buf = appendF32(buf, v)
	}
	buf = appendF64(buf, totalSum)
	buf = appendU64(buf, totalCount)
	return buf, nil
}

// DecodeFallback decodes a payload produced by EncodeFallback.
func DecodeFallback(payload []byte) (*FallbackResult, error) {
	r := &reader{b: payload}
	k, err := r.u32()
	if err != nil {
		return nil, err
	}
	values := make(map[uint64]float32, min(int(k), r.remaining()/12+1))
	for i := uint32(0); i < k; i++ {
		id, err := r.u64()
		if err != nil {
			return nil, err
		}
		v, err := r.f32()
		if err != nil {
			return nil, err
		}
		values[id] = v
	}
	totalSum, err := r.f64()
	if err != nil {
		return nil, err
	}
	totalCount, err := r.u64()
	if err != nil {
		return nil, err
	}
	if r.remaining() != 0 {
		return nil, fmt.Errorf("%w: fallback has %d trailing bytes", ErrInvalidFrame, r.remaining())
	}
	return &FallbackResult{Values: values, TotalSum: totalSum, TotalCount: totalCount}, nil
}
