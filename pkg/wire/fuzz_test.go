package wire

import "testing"

// FuzzUnmarshal asserts Unmarshal tolerates arbitrary bytes: it may
// return an error, but it must never panic.
func FuzzUnmarshal(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("PLMP"))
	f.Add(make([]byte, 77))

	seed := baseFrame()
	if b, err := Marshal(seed); err == nil {
		f.Add(b)
	}

	withDeltas := baseFrame()
	withDeltas.DictDeltas = []DictDelta{
		{ID: 1, InitValue: 2.5, Name: []byte("abc")},
		{ID: 2, Flags: DictFlagTombstone},
	}
	withDeltas.SnapshotBlob = []byte{1, 2, 3}
	if b, err := Marshal(withDeltas); err == nil {
		f.Add(b)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		fr, err := Unmarshal(data)
		if err != nil {
			return
		}
		// If it decoded, re-marshaling it must not panic either, and
		// should itself decode successfully (unless it violates a
		// Marshal-time-only invariant already implied by a valid decode).
		if b, merr := Marshal(fr); merr == nil {
			if _, rerr := Unmarshal(b); rerr != nil {
				t.Fatalf("re-Unmarshal of re-Marshaled frame failed: %v", rerr)
			}
		}
	})
}
