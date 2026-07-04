package sketch

import (
	"testing"
	"time"

	"github.com/purushpsm147/palimpsest/pkg/predict"
	"github.com/purushpsm147/palimpsest/pkg/wire"
)

// TestTrackerLifecycle is the acceptance fixture: a hand-computed 3-series
// scenario exercising birth -> observe -> timeout -> tombstone, asserting
// dict_deltas are correct at each step and that the *next* residual after
// a birth reflects the (held) predictor baseline rather than being
// re-seeded to the new raw value.
func TestTrackerLifecycle(t *testing.T) {
	pred := predict.NewHold()
	tr := NewTracker(pred, nil)

	t0 := time.Unix(1000, 0)
	names := [][]byte{
		[]byte("svc.a.count|agg=sum"),
		[]byte("svc.b.count|agg=sum"),
		[]byte("svc.c.count|agg=sum"),
	}
	values := []float64{10, 20, 30}
	ids := make([]uint64, 3)

	// --- birth: all three series are new at t0 ---
	for i, name := range names {
		id, isNew, residual := tr.Observe(name, values[i], t0)
		if !isNew {
			t.Fatalf("series %d: expected isNew=true on first Observe", i)
		}
		if residual != 0 {
			t.Fatalf("series %d: expected residual=0 on birth, got %v", i, residual)
		}
		wantID := SeriesID(name)
		if id != wantID {
			t.Fatalf("series %d: id = %x, want SeriesID = %x", i, id, wantID)
		}
		ids[i] = id
	}

	// dict_deltas: exactly 3 births, matching name/id/init_value, and the
	// predictor was seeded to each birth's raw value.
	deltas := tr.DrainDeltas()
	if len(deltas) != 3 {
		t.Fatalf("DrainDeltas: got %d deltas, want 3", len(deltas))
	}
	byID := make(map[uint64]wire.DictDelta, 3)
	for _, d := range deltas {
		byID[d.ID] = d
	}
	for i, id := range ids {
		d, ok := byID[id]
		if !ok {
			t.Fatalf("series %d: no dict_delta for id %x", i, id)
		}
		if d.IsTombstone() {
			t.Fatalf("series %d: birth delta unexpectedly a tombstone", i)
		}
		if d.InitValue != float32(values[i]) {
			t.Fatalf("series %d: InitValue = %v, want %v", i, d.InitValue, values[i])
		}
		if string(d.Name) != string(names[i]) {
			t.Fatalf("series %d: Name = %q, want %q", i, d.Name, names[i])
		}
		if v, ok := pred.Predict(id); !ok || v != values[i] {
			t.Fatalf("series %d: predictor baseline = (%v,%v), want (%v,true)", i, v, ok, values[i])
		}
	}
	// DrainDeltas is a drain: a second call must be empty.
	if got := tr.DrainDeltas(); len(got) != 0 {
		t.Fatalf("second DrainDeltas: got %d deltas, want 0", len(got))
	}

	// --- observe again on series 0: NEXT residual reflects the held
	// predictor baseline (10), not a re-seed to the new value (25) ---
	t1 := t0.Add(10 * time.Second)
	id0, isNew0, residual0 := tr.Observe(names[0], 25, t1)
	if isNew0 {
		t.Fatal("second Observe on series 0 should not report isNew")
	}
	if id0 != ids[0] {
		t.Fatalf("id0 = %x, want %x", id0, ids[0])
	}
	wantResidual := 25 - values[0] // predictor still held at the birth value
	if residual0 != wantResidual {
		t.Fatalf("residual0 = %v, want %v (predictor baseline not re-seeded)", residual0, wantResidual)
	}
	// Non-birth observations must not queue a dict_delta.
	if got := tr.DrainDeltas(); len(got) != 0 {
		t.Fatalf("DrainDeltas after non-birth Observe: got %d, want 0", len(got))
	}
	// The predictor baseline itself is still 10 (open-loop: it did not
	// move to 25 just because we observed it).
	if v, ok := pred.Predict(id0); !ok || v != values[0] {
		t.Fatalf("predictor baseline for id0 = (%v,%v), want (%v,true) — should not have moved", v, ok, values[0])
	}

	// FullDict reflects the *last observed value* (25 for series 0, the
	// original births for the untouched series 1/2), not the seed value.
	full := tr.FullDict()
	if len(full) != 3 {
		t.Fatalf("FullDict: got %d entries, want 3", len(full))
	}
	fullByID := make(map[uint64]wire.DictDelta, 3)
	for _, d := range full {
		fullByID[d.ID] = d
	}
	if d := fullByID[ids[0]]; d.InitValue != 25 {
		t.Fatalf("FullDict[id0].InitValue = %v, want 25 (last observed value)", d.InitValue)
	}
	if d := fullByID[ids[1]]; d.InitValue != float32(values[1]) {
		t.Fatalf("FullDict[id1].InitValue = %v, want %v", d.InitValue, values[1])
	}

	// --- timeout: series 1 and 2 were never touched again; series 0 was
	// touched at t1. Expire at t1+55s with ttl=60s relative to each
	// series' own last-seen time: series 0's age is 55s (< ttl, survives),
	// series 1/2's age is t1+55s-t0 = 65s (>= ttl, tombstoned). ---
	ttl := 60 * time.Second
	texpire := t1.Add(55 * time.Second)
	tombstones := tr.Expire(texpire, ttl)

	if len(tombstones) != 2 {
		t.Fatalf("Expire: got %d tombstones, want 2 (series 1 and 2)", len(tombstones))
	}
	tombByID := make(map[uint64]wire.DictDelta, len(tombstones))
	for _, d := range tombstones {
		tombByID[d.ID] = d
	}
	for _, id := range []uint64{ids[1], ids[2]} {
		d, ok := tombByID[id]
		if !ok {
			t.Fatalf("missing tombstone for id %x", id)
		}
		if !d.IsTombstone() {
			t.Fatalf("delta for id %x: Flags=%d, want TOMBSTONE set", id, d.Flags)
		}
		if d.InitValue != 0 || len(d.Name) != 0 {
			t.Fatalf("tombstone for id %x must have zero InitValue and empty Name, got %+v", id, d)
		}
		if _, ok := pred.Predict(id); ok {
			t.Fatalf("predictor baseline for tombstoned id %x should be evicted", id)
		}
	}
	if _, tombstoned := tombByID[ids[0]]; tombstoned {
		t.Fatal("series 0 should have survived Expire (touched within ttl)")
	}

	// A tombstoned frame must survive wire.Marshal's validation.
	if _, err := wire.Marshal(&wire.Frame{
		Magic:      wire.Magic,
		Version:    wire.Version,
		FrameType:  wire.FrameTypeResidual,
		Bits:       8,
		DictDeltas: tombstones,
	}); err != nil {
		t.Fatalf("wire.Marshal rejected tombstone dict_deltas: %v", err)
	}

	active := tr.ActiveIDs()
	if len(active) != 1 || active[0] != ids[0] {
		t.Fatalf("ActiveIDs = %v, want [%x]", active, ids[0])
	}
}

func TestTrackerResetWindowClearsResidualsOnly(t *testing.T) {
	pred := predict.NewHold()
	tr := NewTracker(pred, nil)
	now := time.Unix(0, 0)

	name := []byte("svc.x|agg=sum")
	tr.Observe(name, 10, now)
	tr.DrainDeltas()
	id, _, residual := tr.Observe(name, 15, now.Add(time.Second))
	if residual == 0 {
		t.Fatal("expected non-zero residual before ResetWindow")
	}
	if len(tr.TopKResiduals(10)) != 1 {
		t.Fatalf("expected 1 windowed residual before ResetWindow, got %d", len(tr.TopKResiduals(10)))
	}

	tr.ResetWindow()

	if got := tr.TopKResiduals(10); len(got) != 0 {
		t.Fatalf("TopKResiduals after ResetWindow = %v, want empty", got)
	}
	// Persistent state must survive ResetWindow.
	if ids := tr.ActiveIDs(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("ActiveIDs after ResetWindow = %v, want [%x]", ids, id)
	}
	if v, ok := pred.Predict(id); !ok || v != 10 {
		t.Fatalf("predictor baseline after ResetWindow = (%v,%v), want (10,true)", v, ok)
	}
}

func TestTrackerTopKResiduals(t *testing.T) {
	pred := predict.NewHold()
	tr := NewTracker(pred, nil)
	now := time.Unix(0, 0)

	type series struct {
		name  []byte
		birth float64
		next  float64
	}
	all := []series{
		{[]byte("a|agg=sum"), 100, 105}, // residual  5
		{[]byte("b|agg=sum"), 100, 70},  // residual -30
		{[]byte("c|agg=sum"), 100, 112}, // residual  12
	}
	ids := make([]uint64, len(all))
	for i, s := range all {
		id, _, _ := tr.Observe(s.name, s.birth, now)
		ids[i] = id
	}
	tr.DrainDeltas()
	for i, s := range all {
		tr.Observe(s.name, s.next, now.Add(time.Second))
		_ = i
	}

	top2 := tr.TopKResiduals(2)
	if len(top2) != 2 {
		t.Fatalf("TopKResiduals(2): got %d entries, want 2", len(top2))
	}
	if _, ok := top2[ids[1]]; !ok || top2[ids[1]] != -30 {
		t.Fatalf("expected id[1] residual -30 in top2, got %v", top2)
	}
	if _, ok := top2[ids[2]]; !ok || top2[ids[2]] != 12 {
		t.Fatalf("expected id[2] residual 12 in top2, got %v", top2)
	}
	if _, ok := top2[ids[0]]; ok {
		t.Fatalf("id[0] (residual 5, smallest magnitude) should not be in top2, got %v", top2)
	}
}

// TestTrackerBreakerFlagged is the acceptance test for the Breaker's
// integration into Tracker: an excessive birth rate trips the breaker and
// that state is observable through the Tracker, and the rejected series
// never joins the dictionary.
func TestTrackerBreakerFlagged(t *testing.T) {
	pred := predict.NewHold()
	breaker := NewBreaker(2, time.Minute)
	tr := NewTracker(pred, breaker)
	now := time.Unix(0, 0)

	tr.Observe([]byte("a|agg=sum"), 1, now)
	tr.Observe([]byte("b|agg=sum"), 1, now)
	if tr.BreakerTripped() {
		t.Fatal("should not be tripped after exactly max births")
	}

	rejectedName := []byte("c|agg=sum")
	id, isNew, residual := tr.Observe(rejectedName, 1, now)
	if isNew {
		t.Fatal("3rd birth over max should be rejected (isNew=false)")
	}
	if residual != 0 {
		t.Fatalf("rejected birth residual = %v, want 0", residual)
	}
	if !tr.BreakerTripped() {
		t.Fatal("expected Tracker.BreakerTripped() == true after the breaker trips")
	}
	for _, activeID := range tr.ActiveIDs() {
		if activeID == id {
			t.Fatal("breaker-rejected series must not become an active dictionary entry")
		}
	}
	if _, ok := pred.Predict(id); ok {
		t.Fatal("breaker-rejected series must not seed the predictor")
	}
}

func TestTrackerNilPredictorAndBreaker(t *testing.T) {
	tr := NewTracker(nil, nil)
	now := time.Unix(0, 0)
	name := []byte("svc.z|agg=sum")

	id, isNew, residual := tr.Observe(name, 1, now)
	if !isNew || residual != 0 {
		t.Fatalf("birth with nil predictor: isNew=%v residual=%v, want true,0", isNew, residual)
	}
	_, isNew2, residual2 := tr.Observe(name, 5, now.Add(time.Second))
	if isNew2 || residual2 != 0 {
		t.Fatalf("non-birth with nil predictor: isNew=%v residual=%v, want false,0", isNew2, residual2)
	}
	if tr.BreakerTripped() {
		t.Fatal("BreakerTripped with nil breaker should be false")
	}
	tombstones := tr.Expire(now.Add(time.Hour), time.Minute)
	if len(tombstones) != 1 || tombstones[0].ID != id {
		t.Fatalf("Expire with nil predictor: got %+v", tombstones)
	}
}
