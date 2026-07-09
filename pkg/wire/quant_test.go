/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import (
	"math"
	"testing"
)

func TestQuantizeDequantizeRoundTrip(t *testing.T) {
	scale := float32(0.05)
	valuesByBits := map[uint8][]float64{
		// int8 range at this scale is +/- 128*0.05 = +/-6.4.
		8: {0, 1.2, -1.2, 6.0, -6.0, 0.001, -0.001, 3.14159},
		// int16 range at this scale is +/- 32768*0.05 = +/-1638.4.
		16: {0, 1.2, -1.2, 100, -100, 1600, -1600, 0.001, -0.001, 3.14159},
	}
	for _, bits := range []uint8{8, 16} {
		values := valuesByBits[bits]
		payload, err := Quantize(values, bits, scale)
		if err != nil {
			t.Fatalf("Quantize(bits=%d): %v", bits, err)
		}
		got, err := Dequantize(payload, uint32(len(values)), bits, scale)
		if err != nil {
			t.Fatalf("Dequantize(bits=%d): %v", bits, err)
		}
		for i, v := range values {
			// Quantization error must not exceed half a scale step
			// beyond floating point slop.
			if math.Abs(got[i]-v) > float64(scale)/2+1e-6 {
				t.Fatalf("bits=%d i=%d: want ~%v got %v", bits, i, v, got[i])
			}
		}
	}
}

func TestQuantizeClamps(t *testing.T) {
	payload, err := Quantize([]float64{1e9, -1e9}, 8, 1)
	if err != nil {
		t.Fatalf("Quantize: %v", err)
	}
	if int8(payload[0]) != 127 || int8(payload[1]) != -128 {
		t.Fatalf("want clamp to [-128,127], got %v", payload)
	}
}

func TestQuantizeRejectsBadScale(t *testing.T) {
	for _, scale := range []float32{0, -1, float32(math.NaN()), float32(math.Inf(1))} {
		if _, err := Quantize([]float64{1}, 8, scale); err == nil {
			t.Fatalf("scale=%v: want error, got nil", scale)
		}
	}
}

func TestQuantizeRejectsBadBits(t *testing.T) {
	if _, err := Quantize([]float64{1}, 4, 1); err == nil {
		t.Fatal("bits=4: want error, got nil")
	}
	if _, err := Dequantize([]byte{1}, 1, 4, 1); err == nil {
		t.Fatal("bits=4: want error, got nil")
	}
}

func TestDequantizeRejectsLengthMismatch(t *testing.T) {
	if _, err := Dequantize([]byte{1, 2, 3}, 10, 8, 1); err == nil {
		t.Fatal("want length-mismatch error, got nil")
	}
	if _, err := Dequantize([]byte{1, 2, 3}, 10, 16, 1); err == nil {
		t.Fatal("want length-mismatch error, got nil")
	}
}
