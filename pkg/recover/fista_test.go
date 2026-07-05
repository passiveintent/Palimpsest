package recover

import (
	"fmt"
	"testing"

	"github.com/purushpsm147/palimpsest/pkg/sketch"
	"github.com/purushpsm147/palimpsest/pkg/wire"
)

func defaultTestOptions() Options {
	return Options{Iters: 200, Lambda: 0.05, PowerIters: 40, Threshold: 0.3}
}

// TestRecoverCoverageZeroGated is the ADR-013 acceptance fixture: a
// recovery window where no emitter has been observed yet must gate to
// ConfidenceFallback without misreporting a solved result.
func TestRecoverCoverageZeroGated(t *testing.T) {
	dict := NewDictionary()
	name := []byte("a|agg=sum")
	dict.ApplyDelta(wire.DictDelta{ID: sketch.SeriesID(name), Name: name})
	// Deliberately no dict.ObserveEmitter call.

	const m, d = 100, 4
	opts := defaultTestOptions()
	opts.EmittersExpected = 3

	res, err := Recover(make([]float64, m), dict, sketch.Params{M: m, D: d, Seed: 1}, opts)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if res.Confidence != ConfidenceFallback {
		t.Fatalf("Confidence = %q, want %q", res.Confidence, ConfidenceFallback)
	}
	if res.Coverage != 0 || res.CoverageTotal != 3 {
		t.Fatalf("Coverage = (%d,%d), want (0,3)", res.Coverage, res.CoverageTotal)
	}
	if len(res.SupportIDs) != 0 {
		t.Fatalf("SupportIDs = %v, want empty for a coverage-zero gate", res.SupportIDs)
	}
}

// TestRecoverEmptyDictionaryGated checks the other fallback path: an empty
// dictionary (no known candidates at all) also gates to fallback rather
// than running FISTA over zero columns.
func TestRecoverEmptyDictionaryGated(t *testing.T) {
	dict := NewDictionary()
	const m, d = 50, 4
	opts := defaultTestOptions()

	res, err := Recover(make([]float64, m), dict, sketch.Params{M: m, D: d, Seed: 1}, opts)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if res.Confidence != ConfidenceFallback {
		t.Fatalf("Confidence = %q, want %q", res.Confidence, ConfidenceFallback)
	}
}

// TestRecoverTombstoneExcludedFromSupport is the ADR-008 acceptance
// fixture: a tombstoned series must never appear in a Recover result's
// SupportIDs, even when its sketch contribution is still present in y
// (e.g. a stale/adversarial residual, or one injected before the
// tombstone took effect).
func TestRecoverTombstoneExcludedFromSupport(t *testing.T) {
	const m, d = 500, 6
	const seed = 42
	nameA := []byte("series_a|agg=sum")
	nameB := []byte("series_b|agg=sum")
	nameC := []byte("series_c|agg=sum")
	idA, idB, idC := sketch.SeriesID(nameA), sketch.SeriesID(nameB), sketch.SeriesID(nameC)

	dict := NewDictionary()
	dict.ApplyDelta(wire.DictDelta{ID: idA, Name: nameA})
	dict.ApplyDelta(wire.DictDelta{ID: idB, Name: nameB})
	dict.ApplyDelta(wire.DictDelta{ID: idC, Name: nameC})
	dict.ApplyDelta(wire.DictDelta{ID: idB, Flags: wire.DictFlagTombstone}) // B dies
	dict.ObserveEmitter(1)

	acc := sketch.NewAccumulator(sketch.Params{M: m, D: d, Seed: seed})
	acc.Update(nameA, 8.0)
	acc.Update(nameB, 9.0) // still injected into y despite the decoder tombstoning it
	acc.Update(nameC, 7.5)
	y, _ := acc.Flush()

	opts := defaultTestOptions()
	opts.EmittersExpected = 1
	res, err := Recover(y, dict, sketch.Params{M: m, D: d, Seed: seed}, opts)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}

	var foundA, foundB, foundC bool
	for _, id := range res.SupportIDs {
		switch id {
		case idA:
			foundA = true
		case idB:
			foundB = true
		case idC:
			foundC = true
		}
	}
	if foundB {
		t.Fatalf("tombstoned series B appeared in SupportIDs: %v", res.SupportIDs)
	}
	if !foundA || !foundC {
		t.Fatalf("expected A and C recovered, got SupportIDs=%v (foundA=%v foundC=%v)", res.SupportIDs, foundA, foundC)
	}
}

// TestRecoverDriftUnknownSeriesHighResidual is the ADR-008 drift
// acceptance fixture: a series unknown to the decoder's Dictionary
// contributes real energy to y that no known candidate explains, so the
// final residual should be substantially higher than an otherwise
// identical scenario where every contributing series is known.
func TestRecoverDriftUnknownSeriesHighResidual(t *testing.T) {
	const m, d = 500, 6
	const seed = 7
	nameKnown := []byte("known_series|agg=sum")
	nameUnknown := []byte("unknown_series|agg=sum")
	idKnown := sketch.SeriesID(nameKnown)
	idUnknown := sketch.SeriesID(nameUnknown)

	acc := sketch.NewAccumulator(sketch.Params{M: m, D: d, Seed: seed})
	acc.Update(nameKnown, 5.0)
	acc.Update(nameUnknown, 50.0) // large residual from a series the decoder doesn't know about
	y, _ := acc.Flush()

	opts := defaultTestOptions()
	opts.EmittersExpected = 1

	// Decoder only knows about nameKnown: the drift scenario.
	driftDict := NewDictionary()
	driftDict.ApplyDelta(wire.DictDelta{ID: idKnown, Name: nameKnown})
	driftDict.ObserveEmitter(1)
	driftRes, err := Recover(y, driftDict, sketch.Params{M: m, D: d, Seed: seed}, opts)
	if err != nil {
		t.Fatalf("Recover (drift): %v", err)
	}

	// Baseline: decoder knows about both series.
	fullDict := NewDictionary()
	fullDict.ApplyDelta(wire.DictDelta{ID: idKnown, Name: nameKnown})
	fullDict.ApplyDelta(wire.DictDelta{ID: idUnknown, Name: nameUnknown})
	fullDict.ObserveEmitter(1)
	fullRes, err := Recover(y, fullDict, sketch.Params{M: m, D: d, Seed: seed}, opts)
	if err != nil {
		t.Fatalf("Recover (full): %v", err)
	}

	if driftRes.Residual <= fullRes.Residual*2 {
		t.Fatalf("drift residual (%v) not meaningfully higher than fully-known residual (%v)", driftRes.Residual, fullRes.Residual)
	}
}

func TestRecoverRejectsMismatchedLength(t *testing.T) {
	dict := NewDictionary()
	_, err := Recover(make([]float64, 10), dict, sketch.Params{M: 20, D: 4, Seed: 1}, defaultTestOptions())
	if err == nil {
		t.Fatal("expected an error for len(y) != p.M")
	}
}

func TestRecoverRejectsBadOptions(t *testing.T) {
	dict := NewDictionary()
	params := sketch.Params{M: 10, D: 4, Seed: 1}
	y := make([]float64, 10)

	cases := []Options{
		{Iters: 0, Lambda: 0.05, PowerIters: 10, Threshold: 0.3},
		{Iters: fistaMaxIters + 1, Lambda: 0.05, PowerIters: 10, Threshold: 0.3},
		{Iters: 10, Lambda: 0.05, PowerIters: 0, Threshold: 0.3},
		{Iters: 10, Lambda: 0.05, PowerIters: 10, Threshold: 0},
	}
	for i, o := range cases {
		if _, err := Recover(y, dict, params, o); err == nil {
			t.Errorf("case %d: expected an error for invalid Options %+v", i, o)
		}
	}
}

// TestFISTARecoversSimpleSparseSignal is a small, fast, non-golden sanity
// check that the FISTA + debias pipeline recovers a clean sparse signal
// end to end.
func TestFISTARecoversSimpleSparseSignal(t *testing.T) {
	const n, m, d = 300, 200, 6
	const seed = 99

	dict := NewDictionary()
	names := make([][]byte, n)
	for i := 0; i < n; i++ {
		name := []byte(fmt.Sprintf("series%04x|agg=sum", i))
		names[i] = name
		dict.ApplyDelta(wire.DictDelta{ID: sketch.SeriesID(name), Name: name})
	}
	dict.ObserveEmitter(1)

	acc := sketch.NewAccumulator(sketch.Params{M: m, D: d, Seed: seed})
	trueResiduals := map[uint64]float64{
		sketch.SeriesID(names[10]):  8.0,
		sketch.SeriesID(names[50]):  -6.5,
		sketch.SeriesID(names[123]): 5.0,
	}
	acc.Update(names[10], 8.0)
	acc.Update(names[50], -6.5)
	acc.Update(names[123], 5.0)
	y, _ := acc.Flush()

	opts := defaultTestOptions()
	opts.EmittersExpected = 1
	res, err := Recover(y, dict, sketch.Params{M: m, D: d, Seed: seed}, opts)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}

	byID := make(map[uint64]float64, len(res.SupportIDs))
	for i, id := range res.SupportIDs {
		byID[id] = res.Values[i]
	}
	for id, want := range trueResiduals {
		got, ok := byID[id]
		if !ok {
			t.Fatalf("expected id %#x (residual %v) in SupportIDs, got %v", id, want, res.SupportIDs)
		}
		if diff := got - want; diff > 0.5 || diff < -0.5 {
			t.Fatalf("id %#x recovered value = %v, want ~%v", id, got, want)
		}
	}
}
