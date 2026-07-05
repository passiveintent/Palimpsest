/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import (
	"errors"
	"hash/crc32"
	"testing"
)

func baseFrame() *Frame {
	return &Frame{
		Magic:      Magic,
		Version:    Version,
		FrameType:  FrameTypeKeyframe,
		Flags:      0,
		Bits:       8,
		EmitterID:  0x1122334455667788,
		ShardID:    7,
		Epoch:      42,
		Seq:        1000,
		ViewID:     0,
		M:          256,
		D:          4,
		Predictor:  1,
		Reserved:   0,
		Energy:     1.5,
		QuantScale: 0.01,
		DictRoot:   0xdeadbeefcafef00d,
		Payload:    []byte{1, 2, 3, 4},
	}
}

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		f    *Frame
	}{
		{"minimal", func() *Frame { f := baseFrame(); f.Payload = nil; return f }()},
		{"with payload", baseFrame()},
		{"with dict deltas", func() *Frame {
			f := baseFrame()
			f.DictDeltas = []DictDelta{
				{ID: 1, Flags: 0, InitValue: 3.14, Name: []byte("series.one")},
				{ID: 2, Flags: DictFlagTombstone, InitValue: 0, Name: nil},
			}
			return f
		}()},
		{"with snapshot blob", func() *Frame {
			f := baseFrame()
			f.SnapshotBlob = []byte{9, 9, 9, 9, 9}
			return f
		}()},
		{"fallback frame type", func() *Frame {
			f := baseFrame()
			f.FrameType = FrameTypeFallback
			f.Payload = []byte("fallback-payload")
			return f
		}()},
		{"residual frame type", func() *Frame {
			f := baseFrame()
			f.FrameType = FrameTypeResidual
			f.Payload = []byte{0xFF, 0x00, 0x7F, 0x80}
			return f
		}()},
		{"kdelta flag set", func() *Frame {
			f := baseFrame()
			f.Flags = FlagKDelta
			return f
		}()},
		{"gzip flag with blob", func() *Frame {
			f := baseFrame()
			f.Flags = FlagGzip
			f.SnapshotBlob = []byte{1, 2, 3}
			return f
		}()},
		{"empty name dict delta non-tombstone", func() *Frame {
			f := baseFrame()
			f.DictDeltas = []DictDelta{{ID: 5, Flags: 0, InitValue: 0, Name: nil}}
			return f
		}()},
		{"large name", func() *Frame {
			f := baseFrame()
			name := make([]byte, 5000)
			for i := range name {
				name[i] = byte(i)
			}
			f.DictDeltas = []DictDelta{{ID: 9, InitValue: 1, Name: name}}
			return f
		}()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := Marshal(tt.f)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			got, err := Unmarshal(b)
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			assertFrameEqual(t, tt.f, got)

			// Round-trip again through the decoded frame to confirm
			// idempotency of Marshal(Unmarshal(Marshal(f))).
			b2, err := Marshal(got)
			if err != nil {
				t.Fatalf("re-Marshal: %v", err)
			}
			if string(b) != string(b2) {
				t.Fatalf("re-marshal bytes differ:\n  first=% x\n second=% x", b, b2)
			}
		})
	}
}

func assertFrameEqual(t *testing.T, want, got *Frame) {
	t.Helper()
	if got.Magic != want.Magic || got.Version != want.Version || got.FrameType != want.FrameType ||
		got.Flags != want.Flags || got.Bits != want.Bits || got.EmitterID != want.EmitterID ||
		got.ShardID != want.ShardID || got.Epoch != want.Epoch || got.Seq != want.Seq ||
		got.ViewID != want.ViewID || got.M != want.M || got.D != want.D ||
		got.Predictor != want.Predictor || got.Reserved != want.Reserved ||
		got.Energy != want.Energy || got.QuantScale != want.QuantScale || got.DictRoot != want.DictRoot {
		t.Fatalf("fixed fields differ:\nwant=%+v\ngot=%+v", want, got)
	}
	if len(got.DictDeltas) != len(want.DictDeltas) {
		t.Fatalf("dict_delta count: want %d got %d", len(want.DictDeltas), len(got.DictDeltas))
	}
	for i := range want.DictDeltas {
		w, g := want.DictDeltas[i], got.DictDeltas[i]
		if w.ID != g.ID || w.Flags != g.Flags || w.InitValue != g.InitValue || string(w.Name) != string(g.Name) {
			t.Fatalf("dict_delta[%d] differs:\nwant=%+v\ngot=%+v", i, w, g)
		}
	}
	if string(want.SnapshotBlob) != string(got.SnapshotBlob) {
		t.Fatalf("snapshot_blob differs: want=% x got=% x", want.SnapshotBlob, got.SnapshotBlob)
	}
	if string(want.Payload) != string(got.Payload) {
		t.Fatalf("payload differs: want=% x got=% x", want.Payload, got.Payload)
	}
}

func TestMarshalRejectsInvalidTombstone(t *testing.T) {
	cases := []DictDelta{
		{ID: 1, Flags: DictFlagTombstone, InitValue: 1, Name: nil},
		{ID: 2, Flags: DictFlagTombstone, InitValue: 0, Name: []byte("x")},
	}
	for _, dd := range cases {
		f := baseFrame()
		f.DictDeltas = []DictDelta{dd}
		if _, err := Marshal(f); !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("Marshal(%+v): want ErrInvalidFrame, got %v", dd, err)
		}
	}
}

func TestMarshalRejectsBadMagicOrVersion(t *testing.T) {
	f := baseFrame()
	f.Magic = [4]byte{'X', 'X', 'X', 'X'}
	if _, err := Marshal(f); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("bad magic: want ErrInvalidFrame, got %v", err)
	}

	f = baseFrame()
	f.Version = 99
	if _, err := Marshal(f); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("bad version: want ErrInvalidFrame, got %v", err)
	}
}

func TestUnmarshalCRCMismatch(t *testing.T) {
	b, err := Marshal(baseFrame())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	corrupt := append([]byte(nil), b...)
	corrupt[len(corrupt)/2] ^= 0xFF
	if _, err := Unmarshal(corrupt); !errors.Is(err, ErrCRCMismatch) {
		t.Fatalf("want ErrCRCMismatch, got %v", err)
	}
}

func TestUnmarshalBadMagic(t *testing.T) {
	b, err := Marshal(baseFrame())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	b[0] = 'Q'
	if _, err := Unmarshal(b); err == nil {
		t.Fatal("want error for bad magic, got nil")
	}
}

func TestUnmarshalShortBuffer(t *testing.T) {
	for n := 0; n < 8; n++ {
		if _, err := Unmarshal(make([]byte, n)); err == nil {
			t.Fatalf("Unmarshal(%d zero bytes): want error, got nil", n)
		}
	}
}

func TestUnmarshalTruncatedValidFrame(t *testing.T) {
	b, err := Marshal(baseFrame())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for n := 0; n < len(b); n++ {
		// Must never panic; error is acceptable and expected.
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Unmarshal(truncated to %d bytes) panicked: %v", n, r)
				}
			}()
			_, _ = Unmarshal(b[:n])
		}()
	}
}

func TestUnmarshalTrailingBytes(t *testing.T) {
	b, err := Marshal(baseFrame())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Append an extra byte to the body and recompute the CRC over it, so
	// the trailing-bytes check (not the CRC check) is what's exercised.
	body := append(append([]byte(nil), b[:len(b)-4]...), 0x00)
	crc := crc32.Checksum(body, crcTable)
	body = appendU32(body, crc)
	if _, err := Unmarshal(body); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("want ErrInvalidFrame for trailing bytes, got %v", err)
	}
}
