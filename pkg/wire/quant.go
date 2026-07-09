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
)

// Quantize packs residuals into a RESIDUAL payload (docs/SPEC.md:
// "packed int8/int16 x m (per bits field)"). Each value is divided by
// scale, rounded to the nearest integer (ties away from zero), clamped to
// the representable range for bits, and packed little-endian.
//
// scale must be finite and strictly positive (it is the divisor).
func Quantize(values []float64, bits uint8, scale float32) ([]byte, error) {
	if scale <= 0 || math.IsNaN(float64(scale)) || math.IsInf(float64(scale), 0) {
		return nil, fmt.Errorf("%w: invalid quant_scale %v", ErrInvalidFrame, scale)
	}
	switch bits {
	case 8:
		out := make([]byte, len(values))
		for i, v := range values {
			out[i] = byte(int8(clampRound(v, scale, -128, 127)))
		}
		return out, nil
	case 16:
		out := make([]byte, len(values)*2)
		for i, v := range values {
			q := int16(clampRound(v, scale, -32768, 32767))
			binary.LittleEndian.PutUint16(out[i*2:], uint16(q))
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: unsupported quant bits %d", ErrInvalidFrame, bits)
	}
}

// Dequantize unpacks a RESIDUAL payload of m values back into float64s:
// out[i] = quantized[i] * scale.
func Dequantize(payload []byte, m uint32, bits uint8, scale float32) ([]float64, error) {
	switch bits {
	case 8:
		if uint32(len(payload)) != m {
			return nil, fmt.Errorf("%w: residual payload length %d != m %d", ErrInvalidFrame, len(payload), m)
		}
		out := make([]float64, m)
		for i := range out {
			out[i] = float64(int8(payload[i])) * float64(scale)
		}
		return out, nil
	case 16:
		if uint32(len(payload)) != m*2 {
			return nil, fmt.Errorf("%w: residual payload length %d != 2*m %d", ErrInvalidFrame, len(payload), m)
		}
		out := make([]float64, m)
		for i := range out {
			q := int16(binary.LittleEndian.Uint16(payload[i*2:]))
			out[i] = float64(q) * float64(scale)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: unsupported quant bits %d", ErrInvalidFrame, bits)
	}
}

func clampRound(v float64, scale float32, lo, hi int64) int64 {
	q := math.Round(v / float64(scale))
	if q < float64(lo) {
		return lo
	}
	if q > float64(hi) {
		return hi
	}
	return int64(q)
}
