package sketch

import (
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/purushpsm147/palimpsest/pkg/wire"
)

// TestGoldenVectors cross-checks this package against vectors produced by
// the Python conformance oracle (oracle/README.md, `make golden`):
// SeriesID/Buckets (hash_vectors.json), DeriveEphemeralSeed
// (hkdf_vectors.json), and Tracker's dictionary lifecycle
// (kdelta_sequence.json's births/tombstones/active-id progression).
//
// docs/adr/ADR-001: "Python feasibility harness promoted to conformance
// oracle generating golden vectors Go must match byte-exact."
func TestGoldenVectors(t *testing.T) {
	t.Run("hash", testGoldenHash)
	t.Run("hkdf", testGoldenHKDF)
	t.Run("lifecycle", testGoldenLifecycle)
}

type goldenBySeed struct {
	Seed uint64   `json:"seed"`
	Idx  []uint32 `json:"idx"`
	Sign []int8   `json:"sign"`
}

type goldenHashCase struct {
	Name     string         `json:"name"`
	SeriesID uint64         `json:"series_id"`
	BySeed   []goldenBySeed `json:"by_seed"`
}

type hashVectorsFile struct {
	TenantKey string           `json:"tenant_key"`
	M         int              `json:"m"`
	D         int              `json:"d"`
	Cases     []goldenHashCase `json:"cases"`
}

func testGoldenHash(t *testing.T) {
	var hv hashVectorsFile
	loadGoldenJSON(t, "hash_vectors.json", &hv)

	if len(hv.Cases) != 50 {
		t.Fatalf("want 50 hash_vectors cases, got %d", len(hv.Cases))
	}

	for _, c := range hv.Cases {
		name := []byte(c.Name)

		if got := SeriesID(name); got != c.SeriesID {
			t.Errorf("SeriesID(%q) = %#x, want %#x", c.Name, got, c.SeriesID)
		}

		for _, bs := range c.BySeed {
			idx, sign := Buckets(name, bs.Seed, hv.M, hv.D)
			if len(idx) != len(bs.Idx) || len(sign) != len(bs.Sign) {
				t.Fatalf("Buckets(%q, seed=%#x): length mismatch got idx=%d sign=%d, want idx=%d sign=%d",
					c.Name, bs.Seed, len(idx), len(sign), len(bs.Idx), len(bs.Sign))
			}
			for i := range idx {
				if idx[i] != bs.Idx[i] {
					t.Errorf("Buckets(%q, seed=%#x).idx[%d] = %d, want %d", c.Name, bs.Seed, i, idx[i], bs.Idx[i])
				}
				if sign[i] != bs.Sign[i] {
					t.Errorf("Buckets(%q, seed=%#x).sign[%d] = %d, want %d", c.Name, bs.Seed, i, sign[i], bs.Sign[i])
				}
			}
		}
	}
}

type goldenHKDFCase struct {
	TenantKey    string `json:"tenant_key"`
	ShardID      uint64 `json:"shard_id"`
	EpochIdx     uint32 `json:"epoch_idx"`
	ViewID       uint16 `json:"view_id"`
	ExpectedSeed uint64 `json:"expected_seed"`
}

type hkdfVectorsFile struct {
	Cases []goldenHKDFCase `json:"cases"`
}

func testGoldenHKDF(t *testing.T) {
	var hv hkdfVectorsFile
	loadGoldenJSON(t, "hkdf_vectors.json", &hv)

	if len(hv.Cases) == 0 {
		t.Fatal("hkdf_vectors.json has no cases")
	}

	for _, c := range hv.Cases {
		got := DeriveEphemeralSeed([]byte(c.TenantKey), c.ShardID, c.EpochIdx, c.ViewID)
		if got != c.ExpectedSeed {
			t.Errorf("DeriveEphemeralSeed(%q, %d, %d, %d) = %#x, want %#x",
				c.TenantKey, c.ShardID, c.EpochIdx, c.ViewID, got, c.ExpectedSeed)
		}
	}
}

type goldenBirth struct {
	ID        uint64  `json:"id"`
	Name      string  `json:"name"`
	InitValue float64 `json:"init_value"`
}

type goldenTombstone struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
}

type goldenKDeltaStep struct {
	Step        int                `json:"step"`
	Golden      bool               `json:"golden"`
	FlagsKDelta bool               `json:"flags_kdelta"`
	Births      []goldenBirth      `json:"births"`
	Tombstones  []goldenTombstone  `json:"tombstones"`
	IDs         []uint64           `json:"ids"`
	Values      map[string]float64 `json:"values"`
	DictRoot    uint64             `json:"dict_root"`
	PayloadHex  string             `json:"payload_hex"`
}

type kdeltaSequenceFile struct {
	Scale float64            `json:"scale"`
	Steps []goldenKDeltaStep `json:"steps"`
}

// testGoldenLifecycle replays kdelta_sequence.json's births/tombstones
// through a real Tracker (ADR-008) and checks that the resulting active-ID
// set and dict_root match the oracle's at every step -- the same
// birth/drift/tombstone sequence pkg/wire's golden test decodes from the
// KEYFRAME payload side. Series names are fixed by the generator
// (oracle/gen_golden.py: gen_kdelta_sequence) and repeated here since only
// birth/tombstone steps carry a name in the JSON.
func testGoldenLifecycle(t *testing.T) {
	var seq kdeltaSequenceFile
	loadGoldenJSON(t, "kdelta_sequence.json", &seq)
	if len(seq.Steps) != 4 {
		t.Fatalf("want 4 kdelta_sequence steps, got %d", len(seq.Steps))
	}

	const (
		nameA = "cpu_usage|region=us-east,cluster=prod|agg=sum"
		nameB = "cpu_usage|region=us-west,cluster=prod|agg=sum"
		nameC = "mem_usage|region=us-east,cluster=prod|agg=sum"
		nameE = "disk_io|region=us-east,cluster=prod|agg=sum"
	)
	idA, idB, idC := SeriesID([]byte(nameA)), SeriesID([]byte(nameB)), SeriesID([]byte(nameC))

	tr := NewTracker(nil, nil)
	base := time.Unix(1700000000, 0)
	ttl := 30 * time.Second

	checkActiveAndRoot := func(t *testing.T, step int, wantIDs []uint64, wantRoot uint64) {
		t.Helper()
		got := tr.ActiveIDs()
		if !uint64SetEqual(got, wantIDs) {
			t.Fatalf("step %d: ActiveIDs = %v, want %v", step, got, wantIDs)
		}
		if root := wire.ComputeDictRoot(got); root != wantRoot {
			t.Fatalf("step %d: dict_root = %#x, want %#x", step, root, wantRoot)
		}
	}

	// Step 0: golden keyframe opens the stream with a full birth set
	// (ADR-008/ADR-013).
	step0 := seq.Steps[0]
	for _, b := range step0.Births {
		id, isNew, _ := tr.Observe([]byte(b.Name), b.InitValue, base)
		if !isNew {
			t.Fatalf("step 0: Observe(%q) isNew=false, want true (birth)", b.Name)
		}
		if id != b.ID {
			t.Fatalf("step 0: Observe(%q) id=%#x, want %#x", b.Name, id, b.ID)
		}
	}
	requireDictDeltasMatchBirths(t, 0, tr.DrainDeltas(), step0.Births)
	checkActiveAndRoot(t, 0, step0.IDs, step0.DictRoot)

	// Step 1: E is born; A drifts (not a birth, no dict_delta).
	t1 := base.Add(time.Minute)
	step1 := seq.Steps[1]
	for _, b := range step1.Births {
		id, isNew, _ := tr.Observe([]byte(b.Name), b.InitValue, t1)
		if !isNew {
			t.Fatalf("step 1: Observe(%q) isNew=false, want true (birth)", b.Name)
		}
		if id != b.ID {
			t.Fatalf("step 1: Observe(%q) id=%#x, want %#x", b.Name, id, b.ID)
		}
	}
	aValue1 := requireValue(t, step1, idA)
	if _, isNew, _ := tr.Observe([]byte(nameA), aValue1, t1); isNew {
		t.Fatalf("step 1: drift Observe(%q) reported isNew=true, want false", nameA)
	}
	requireDictDeltasMatchBirths(t, 1, tr.DrainDeltas(), step1.Births)
	checkActiveAndRoot(t, 1, step1.IDs, step1.DictRoot)

	// Step 2: C drifts; no births or tombstones.
	t2 := t1.Add(time.Minute)
	step2 := seq.Steps[2]
	cValue2 := requireValue(t, step2, idC)
	tr.Observe([]byte(nameC), cValue2, t2)
	if got := tr.DrainDeltas(); len(got) != 0 {
		t.Fatalf("step 2: DrainDeltas = %+v, want empty (no births)", got)
	}
	checkActiveAndRoot(t, 2, step2.IDs, step2.DictRoot)

	// Step 3: D is tombstoned; B drifts. Touch every surviving series just
	// before expiry so only D (last touched at birth, step 0) times out.
	tTouch := t2.Add(time.Minute)
	step3 := seq.Steps[3]
	bValue3 := requireValue(t, step3, idB)
	tr.Observe([]byte(nameA), requireValue(t, step3, idA), tTouch)
	tr.Observe([]byte(nameB), bValue3, tTouch)
	tr.Observe([]byte(nameC), requireValue(t, step3, idC), tTouch)
	tr.Observe([]byte(nameE), requireValue(t, step3, SeriesID([]byte(nameE))), tTouch)

	tombstones := tr.Expire(tTouch.Add(5*time.Second), ttl)
	if len(tombstones) != len(step3.Tombstones) {
		t.Fatalf("step 3: Expire returned %d tombstones, want %d", len(tombstones), len(step3.Tombstones))
	}
	for i, want := range step3.Tombstones {
		got := tombstones[i]
		if got.ID != want.ID {
			t.Fatalf("step 3: tombstone[%d].ID = %#x, want %#x", i, got.ID, want.ID)
		}
		if !got.IsTombstone() {
			t.Fatalf("step 3: tombstone[%d] missing TOMBSTONE flag", i)
		}
		if got.InitValue != 0 || len(got.Name) != 0 {
			t.Fatalf("step 3: tombstone[%d] must have zero init_value and empty name, got %+v", i, got)
		}
	}
	checkActiveAndRoot(t, 3, step3.IDs, step3.DictRoot)
}

func requireValue(t *testing.T, step goldenKDeltaStep, id uint64) float64 {
	t.Helper()
	key := strconv.FormatUint(id, 10) // matches Python's json.dumps of an int-keyed dict key
	v, ok := step.Values[key]
	if !ok {
		t.Fatalf("step %d: values[%s] missing", step.Step, key)
	}
	return v
}

func requireDictDeltasMatchBirths(t *testing.T, step int, got []wire.DictDelta, births []goldenBirth) {
	t.Helper()
	if len(got) != len(births) {
		t.Fatalf("step %d: DrainDeltas returned %d deltas, want %d (%+v vs %+v)", step, len(got), len(births), got, births)
	}
	byID := make(map[uint64]goldenBirth, len(births))
	for _, b := range births {
		byID[b.ID] = b
	}
	for _, dd := range got {
		want, ok := byID[dd.ID]
		if !ok {
			t.Fatalf("step %d: unexpected dict_delta id %#x", step, dd.ID)
		}
		if dd.IsTombstone() {
			t.Fatalf("step %d: dict_delta id %#x unexpectedly a tombstone", step, dd.ID)
		}
		if float64(dd.InitValue) != want.InitValue {
			t.Fatalf("step %d: dict_delta id %#x init_value = %v, want %v", step, dd.ID, dd.InitValue, want.InitValue)
		}
		if string(dd.Name) != want.Name {
			t.Fatalf("step %d: dict_delta id %#x name = %q, want %q", step, dd.ID, dd.Name, want.Name)
		}
	}
}

func uint64SetEqual(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	sa := append([]uint64(nil), a...)
	sb := append([]uint64(nil), b...)
	sort.Slice(sa, func(i, j int) bool { return sa[i] < sa[j] })
	sort.Slice(sb, func(i, j int) bool { return sb[i] < sb[j] })
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

func loadGoldenJSON(t *testing.T, filename string, v interface{}) {
	t.Helper()
	const dir = "../../testdata/golden"
	path := dir + "/" + filename
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no golden vectors at %s: %v (run `make golden`)", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
}
