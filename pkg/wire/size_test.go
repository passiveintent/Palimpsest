/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import "testing"

// TestEncodedSizeMatchesMarshal pins EncodedSize to Marshal's actual output
// length across every frame shape codec_test.go's round-trip table exercises
// (dict deltas, snapshot blobs, every frame type, tombstones, large names) so
// the two can never silently drift apart.
func TestEncodedSizeMatchesMarshal(t *testing.T) {
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
		{"kdelta flag set with deltas and blob", func() *Frame {
			f := baseFrame()
			f.Flags = FlagKDelta
			f.SnapshotBlob = []byte{1, 2, 3}
			f.DictDeltas = []DictDelta{{ID: 5, InitValue: 2, Name: []byte("x")}}
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
		{"empty frame", &Frame{Magic: Magic, Version: Version, FrameType: FrameTypeResidual}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := Marshal(tt.f)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if got, want := EncodedSize(tt.f), len(b); got != want {
				t.Fatalf("EncodedSize = %d, want %d (len(Marshal(f)))", got, want)
			}
		})
	}
}
