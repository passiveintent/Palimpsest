package recover

import (
	"testing"

	"github.com/purushpsm147/palimpsest/pkg/sketch"
	"github.com/purushpsm147/palimpsest/pkg/wire"
)

func TestDictionaryApplyDeltaBirthAndTombstone(t *testing.T) {
	dict := NewDictionary()
	nameA := []byte("series_a|agg=sum")
	idA := sketch.SeriesID(nameA)

	if birth := dict.ApplyDelta(wire.DictDelta{ID: idA, Name: nameA, InitValue: 5}); !birth {
		t.Fatal("first ApplyDelta for a new id should report birth=true")
	}
	if birth := dict.ApplyDelta(wire.DictDelta{ID: idA, Name: nameA, InitValue: 6}); birth {
		t.Fatal("re-applying a delta for an already-active id should report birth=false")
	}
	ids := dict.ActiveIDs()
	if len(ids) != 1 || ids[0] != idA {
		t.Fatalf("ActiveIDs = %v, want [%d]", ids, idA)
	}

	// Tombstone removes it.
	if birth := dict.ApplyDelta(wire.DictDelta{ID: idA, Flags: wire.DictFlagTombstone}); birth {
		t.Fatal("tombstone delta should report birth=false")
	}
	if ids := dict.ActiveIDs(); len(ids) != 0 {
		t.Fatalf("ActiveIDs after tombstone = %v, want empty", ids)
	}

	// A later birth of the same ID is a re-birth (birth=true again).
	if birth := dict.ApplyDelta(wire.DictDelta{ID: idA, Name: nameA, InitValue: 7}); !birth {
		t.Fatal("re-birth after tombstone should report birth=true")
	}
}

func TestDictionaryVerifyKeyframe(t *testing.T) {
	dict := NewDictionary()
	nameA := []byte("a|agg=sum")
	nameB := []byte("b|agg=sum")
	idA := sketch.SeriesID(nameA)
	idB := sketch.SeriesID(nameB)
	dict.ApplyDelta(wire.DictDelta{ID: idA, Name: nameA})
	dict.ApplyDelta(wire.DictDelta{ID: idB, Name: nameB})

	root := wire.ComputeDictRoot([]uint64{idA, idB})
	if !dict.VerifyKeyframe(root) {
		t.Fatal("VerifyKeyframe should match the correct dict_root")
	}
	if dict.VerifyKeyframe(root ^ 1) {
		t.Fatal("VerifyKeyframe should reject an incorrect dict_root")
	}

	// Tombstoning changes the active set, so the old root should no
	// longer verify.
	dict.ApplyDelta(wire.DictDelta{ID: idB, Flags: wire.DictFlagTombstone})
	if dict.VerifyKeyframe(root) {
		t.Fatal("VerifyKeyframe should reject a stale dict_root after a tombstone")
	}
	if !dict.VerifyKeyframe(wire.ComputeDictRoot([]uint64{idA})) {
		t.Fatal("VerifyKeyframe should match the updated dict_root after tombstoning")
	}
}

func TestDictionaryCoverage(t *testing.T) {
	dict := NewDictionary()

	if present, total := dict.Coverage(3); present != 0 || total != 3 {
		t.Fatalf("Coverage before any ObserveEmitter = (%d,%d), want (0,3)", present, total)
	}

	dict.ObserveEmitter(1)
	if present, total := dict.Coverage(3); present != 1 || total != 3 {
		t.Fatalf("Coverage after 1 ObserveEmitter = (%d,%d), want (1,3)", present, total)
	}

	dict.ObserveEmitter(2)
	dict.ObserveEmitter(1) // repeat: must not double-count
	if present, total := dict.Coverage(3); present != 2 || total != 3 {
		t.Fatalf("Coverage after 2 distinct ObserveEmitter = (%d,%d), want (2,3)", present, total)
	}
}

func TestDictionaryLastKeyframe(t *testing.T) {
	dict := NewDictionary()
	nameA := []byte("a|agg=sum")
	idA := sketch.SeriesID(nameA)
	dict.ApplyDelta(wire.DictDelta{ID: idA, Name: nameA, InitValue: 12.5})

	got := dict.LastKeyframe()
	if got[idA] != 12.5 {
		t.Fatalf("LastKeyframe[idA] = %v, want 12.5", got[idA])
	}

	dict.ApplyKeyframeValues(map[uint64]float32{idA: 13.0})
	got = dict.LastKeyframe()
	if got[idA] != 13.0 {
		t.Fatalf("LastKeyframe[idA] after ApplyKeyframeValues = %v, want 13.0", got[idA])
	}

	// Mutating the returned map must not affect the Dictionary's state.
	got[idA] = 999
	if fresh := dict.LastKeyframe(); fresh[idA] != 13.0 {
		t.Fatalf("LastKeyframe leaked a mutable reference: got %v, want 13.0", fresh[idA])
	}

	// A keyframe value for an unknown (never-born) id is ignored.
	dict.ApplyKeyframeValues(map[uint64]float32{0xDEAD: 1})
	if _, ok := dict.LastKeyframe()[0xDEAD]; ok {
		t.Fatal("ApplyKeyframeValues should not create entries for unknown ids")
	}
}

func TestDictionaryBuildCSRMatchesBuckets(t *testing.T) {
	dict := NewDictionary()
	names := [][]byte{
		[]byte("a|agg=sum"),
		[]byte("b|agg=sum"),
		[]byte("c|agg=sum"),
	}
	ids := make([]uint64, len(names))
	for i, name := range names {
		ids[i] = sketch.SeriesID(name)
		dict.ApplyDelta(wire.DictDelta{ID: ids[i], Name: name})
	}

	const seed, m, d = 12345, 200, 6
	csr := dict.BuildCSR(seed, m, d)

	activeIDs := dict.ActiveIDs()
	if csr.NRows != len(names) || csr.NCols != m {
		t.Fatalf("BuildCSR shape = (%d,%d), want (%d,%d)", csr.NRows, csr.NCols, len(names), m)
	}

	for row, id := range activeIDs {
		// Find the name for this id to recompute the expected buckets.
		var name []byte
		for i, nid := range ids {
			if nid == id {
				name = names[i]
			}
		}
		wantIdx, wantSign := sketch.Buckets(name, seed, m, d)
		base := csr.RowPtr[row]
		if int(csr.RowPtr[row+1]-base) != d {
			t.Fatalf("row %d has %d entries, want %d", row, csr.RowPtr[row+1]-base, d)
		}
		invSqrtD := 1.0
		for k := 0; k < d; k++ {
			gotCol := csr.ColIdx[int(base)+k]
			if gotCol != int32(wantIdx[k]) {
				t.Errorf("row %d entry %d: col = %d, want %d", row, k, gotCol, wantIdx[k])
			}
			gotVal := csr.Vals[int(base)+k]
			wantSignF := 1.0
			if wantSign[k] < 0 {
				wantSignF = -1.0
			}
			if (gotVal > 0) != (wantSignF > 0) {
				t.Errorf("row %d entry %d: sign mismatch got %v want sign %v", row, k, gotVal, wantSignF)
			}
			_ = invSqrtD
		}
	}

	// Tombstoning one series removes its row entirely.
	dict.ApplyDelta(wire.DictDelta{ID: ids[1], Flags: wire.DictFlagTombstone})
	csr2 := dict.BuildCSR(seed, m, d)
	if csr2.NRows != len(names)-1 {
		t.Fatalf("BuildCSR after tombstone: NRows = %d, want %d", csr2.NRows, len(names)-1)
	}
}
