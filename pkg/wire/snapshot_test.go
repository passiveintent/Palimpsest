/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import (
	"reflect"
	"testing"
)

func TestSnapshotRoundTrip(t *testing.T) {
	entries := []SnapshotEntry{
		{ID: 1, TSMs: 1000, Value: 1.5},
		{ID: 2, TSMs: 2000, Value: -2.5},
	}
	for _, gz := range []bool{false, true} {
		blob, err := EncodeSnapshot(entries, gz)
		if err != nil {
			t.Fatalf("EncodeSnapshot(gzip=%v): %v", gz, err)
		}
		got, err := DecodeSnapshot(blob, gz)
		if err != nil {
			t.Fatalf("DecodeSnapshot(gzip=%v): %v", gz, err)
		}
		if !reflect.DeepEqual(got, entries) {
			t.Fatalf("gzip=%v: want %v got %v", gz, entries, got)
		}
	}
}

func TestSnapshotEmpty(t *testing.T) {
	blob, err := EncodeSnapshot(nil, false)
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	got, err := DecodeSnapshot(blob, false)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}
}

func TestSnapshotBadGzipRejected(t *testing.T) {
	if _, err := DecodeSnapshot([]byte{1, 2, 3, 4}, true); err == nil {
		t.Fatal("want error decoding non-gzip data as gzip, got nil")
	}
}
