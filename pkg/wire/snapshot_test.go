/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import (
	"errors"
	"reflect"
	"testing"
)

func TestSnapshotRoundTrip(t *testing.T) {
	entries := []SnapshotEntry{
		{ID: 1, TSMs: 1000, Value: 1.5},
		{ID: 2, TSMs: 2000, Value: -2.5},
	}
	for _, codec := range []Codec{CodecNone, CodecGzip} {
		blob, err := EncodeSnapshot(entries, codec)
		if err != nil {
			t.Fatalf("EncodeSnapshot(codec=%d): %v", codec, err)
		}
		got, err := DecodeSnapshot(blob, codec)
		if err != nil {
			t.Fatalf("DecodeSnapshot(codec=%d): %v", codec, err)
		}
		if !reflect.DeepEqual(got, entries) {
			t.Fatalf("codec=%d: want %v got %v", codec, entries, got)
		}
	}
}

func TestSnapshotEmpty(t *testing.T) {
	blob, err := EncodeSnapshot(nil, CodecNone)
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	got, err := DecodeSnapshot(blob, CodecNone)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}
}

func TestSnapshotBadGzipRejected(t *testing.T) {
	if _, err := DecodeSnapshot([]byte{1, 2, 3, 4}, CodecGzip); err == nil {
		t.Fatal("want error decoding non-gzip data as gzip, got nil")
	}
}

func TestSnapshotUnregisteredCodecRejected(t *testing.T) {
	const unregistered Codec = 99
	if _, err := EncodeSnapshot(nil, unregistered); !errors.Is(err, ErrUnregisteredCodec) {
		t.Fatalf("EncodeSnapshot(unregistered codec): want ErrUnregisteredCodec, got %v", err)
	}
	if _, err := DecodeSnapshot([]byte{1, 2, 3, 4}, unregistered); !errors.Is(err, ErrUnregisteredCodec) {
		t.Fatalf("DecodeSnapshot(unregistered codec): want ErrUnregisteredCodec, got %v", err)
	}
}
