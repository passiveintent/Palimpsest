package core

import (
	"testing"

	"github.com/purushpsm147/palimpsest/pkg/recover"
	"github.com/purushpsm147/palimpsest/pkg/wire"
)

func TestMatcher_EvalDeviation(t *testing.T) {
	m := &Matcher{}

	fallback := recover.Result{Confidence: recover.ConfidenceFallback, SupportIDs: []uint64{1}, Values: []float64{9}}
	if got := m.EvalDeviation(fallback); got != nil {
		t.Fatalf("EvalDeviation(fallback) = %v, want nil", got)
	}

	res := recover.Result{
		Confidence:    recover.ConfidenceRecovered,
		SupportIDs:    []uint64{10, 20},
		Values:        []float64{1.5, -2.5},
		Residual:      0.1,
		Coverage:      1,
		CoverageTotal: 1,
		Revision:      2,
	}
	events := m.EvalDeviation(res)
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	if events[0].SeriesID != 10 || events[0].Value != 1.5 || events[0].Substrate != "deviation" || events[0].Revision != 2 {
		t.Fatalf("events[0] = %+v, unexpected", events[0])
	}
}

func TestMatcher_EvalNamedSeries(t *testing.T) {
	m := &Matcher{}
	y := []float64{3, -3, 3, -3}
	idx := []uint32{0, 1, 2, 3}
	sign := []int8{1, -1, 1, -1}
	value, errEstimate := m.EvalNamedSeries(y, idx, sign)
	wantValue, wantErr := recover.PointQuery(y, idx, sign)
	if value != wantValue || errEstimate != wantErr {
		t.Fatalf("EvalNamedSeries = (%v, %v), want (%v, %v)", value, errEstimate, wantValue, wantErr)
	}
}

func TestMatcher_EvalKeyframe(t *testing.T) {
	m := &Matcher{DriftThreshold: 5.0}
	cur := map[uint64]float32{1: 10, 2: 100, 3: 7}
	prev := map[uint64]float32{1: 4, 2: 100} // 1 drifted by 6 (over threshold), 2 unchanged, 3 is a birth (no prior)

	events := m.EvalKeyframe(cur, prev)
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1 (only id 1 should drift)", len(events))
	}
	if events[0].SeriesID != 1 || events[0].Substrate != "keyframe" {
		t.Fatalf("events[0] = %+v, unexpected", events[0])
	}
}

func TestMatcher_EvalAbsence(t *testing.T) {
	m := &Matcher{}
	events := m.EvalAbsence([]uint64{7, 8})
	if len(events) != 2 || events[0].Substrate != "absence" || events[1].SeriesID != 8 {
		t.Fatalf("EvalAbsence = %+v, unexpected", events)
	}
}

func TestMatcher_EvalFallback(t *testing.T) {
	m := &Matcher{}
	fb := &wire.FallbackResult{Values: map[uint64]float32{5: 1.5}, TotalSum: 10, TotalCount: 3}
	events := m.EvalFallback(fb)
	if len(events) != 1 || events[0].SeriesID != 5 || events[0].Confidence != recover.ConfidenceFallback {
		t.Fatalf("EvalFallback = %+v, unexpected", events)
	}
}
