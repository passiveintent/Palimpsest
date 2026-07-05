package recover

import (
	"math"
	"testing"

	"github.com/purushpsm147/palimpsest/pkg/sketch"
)

// TestPointQuerySingleSeries is the ADR-009 acceptance fixture: at m=2000
// with only the queried series' contribution in y, PointQuery must
// recover its residual with < 5% relative error.
func TestPointQuerySingleSeries(t *testing.T) {
	const m, d = 2000, 6
	const seed = 0xABCD1234
	name := []byte("checkout_latency_ms|region=us-east,cluster=prod|agg=sum")
	const residual = 7.25

	acc := sketch.NewAccumulator(sketch.Params{M: m, D: d, Seed: seed})
	acc.Update(name, residual)
	y, _ := acc.Flush()

	idx, sign := sketch.Buckets(name, seed, m, d)
	value, errEst := PointQuery(y, idx, sign)

	relErr := math.Abs(value-residual) / math.Abs(residual)
	if relErr >= 0.05 {
		t.Fatalf("PointQuery relative error = %v, want < 0.05 (value=%v, want=%v)", relErr, value, residual)
	}
	if errEst < 0 {
		t.Fatalf("errEstimate = %v, want >= 0", errEst)
	}
}

// TestPointQueryWithInterference checks the median estimator stays close
// even with several other active series contributing noise to the same
// sketch.
func TestPointQueryWithInterference(t *testing.T) {
	const m, d = 2000, 6
	const seed = 42
	target := []byte("target_series|agg=sum")
	const residual = 10.0

	acc := sketch.NewAccumulator(sketch.Params{M: m, D: d, Seed: seed})
	acc.Update(target, residual)
	for i := 0; i < 200; i++ {
		name := []byte{byte('a' + i%26), byte('0' + i/26), byte(i % 7)}
		acc.Update(name, float64(i%5)-2)
	}
	y, _ := acc.Flush()

	idx, sign := sketch.Buckets(target, seed, m, d)
	value, _ := PointQuery(y, idx, sign)

	relErr := math.Abs(value-residual) / math.Abs(residual)
	if relErr >= 0.05 {
		t.Fatalf("PointQuery with interference relative error = %v, want < 0.05 (value=%v)", relErr, value)
	}
}

func TestMedian(t *testing.T) {
	if got := median([]float64{1, 2, 3}); got != 2 {
		t.Fatalf("median odd = %v, want 2", got)
	}
	if got := median([]float64{1, 2, 3, 4}); got != 2.5 {
		t.Fatalf("median even = %v, want 2.5", got)
	}
}
