/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import (
	"reflect"
	"testing"
)

func TestFallbackRoundTrip(t *testing.T) {
	ids := []uint64{3, 7, 42}
	values := map[uint64]float32{3: 1.1, 7: 2.2, 42: 3.3}

	payload, err := EncodeFallback(ids, values, 123.456, 999)
	if err != nil {
		t.Fatalf("EncodeFallback: %v", err)
	}
	got, err := DecodeFallback(payload)
	if err != nil {
		t.Fatalf("DecodeFallback: %v", err)
	}
	if !reflect.DeepEqual(got.Values, values) {
		t.Fatalf("values: want %v got %v", values, got.Values)
	}
	if got.TotalSum != 123.456 {
		t.Fatalf("total_sum: want 123.456 got %v", got.TotalSum)
	}
	if got.TotalCount != 999 {
		t.Fatalf("total_count: want 999 got %v", got.TotalCount)
	}
}

func TestFallbackUnsortedIDsRejected(t *testing.T) {
	ids := []uint64{7, 3}
	values := map[uint64]float32{3: 1, 7: 2}
	if _, err := EncodeFallback(ids, values, 0, 0); err == nil {
		t.Fatal("want error for unsorted ids, got nil")
	}
}

func TestFallbackEmpty(t *testing.T) {
	payload, err := EncodeFallback(nil, nil, 0, 0)
	if err != nil {
		t.Fatalf("EncodeFallback: %v", err)
	}
	got, err := DecodeFallback(payload)
	if err != nil {
		t.Fatalf("DecodeFallback: %v", err)
	}
	if len(got.Values) != 0 || got.TotalSum != 0 || got.TotalCount != 0 {
		t.Fatalf("want zero-value result, got %+v", got)
	}
}
