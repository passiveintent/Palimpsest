package predict

import (
	"testing"

	"github.com/purushpsm147/palimpsest/pkg/wire"
)

func TestHoldID(t *testing.T) {
	h := NewHold()
	if h.ID() != wire.PredictorHold {
		t.Fatalf("ID() = %v, want %v", h.ID(), wire.PredictorHold)
	}
}

func TestHoldSeedPredictEvict(t *testing.T) {
	h := NewHold()
	if _, ok := h.Predict(1); ok {
		t.Fatal("Predict on unseeded id should report ok=false")
	}

	h.Seed(1, 10)
	v, ok := h.Predict(1)
	if !ok || v != 10 {
		t.Fatalf("Predict(1) = (%v, %v), want (10, true)", v, ok)
	}

	// Predict must not move on its own: baseline stays held.
	if v, _ := h.Predict(1); v != 10 {
		t.Fatalf("baseline drifted to %v without a Seed/LoadKeyframe", v)
	}

	h.Evict(1)
	if _, ok := h.Predict(1); ok {
		t.Fatal("Predict after Evict should report ok=false")
	}
}

func TestHoldLoadKeyframeReplacesBaselines(t *testing.T) {
	h := NewHold()
	h.Seed(1, 10)
	h.Seed(2, 20)

	h.LoadKeyframe(map[uint64]float64{2: 200, 3: 300})

	if _, ok := h.Predict(1); ok {
		t.Fatal("id 1 should be gone after LoadKeyframe dropped it")
	}
	if v, ok := h.Predict(2); !ok || v != 200 {
		t.Fatalf("Predict(2) = (%v,%v), want (200,true)", v, ok)
	}
	if v, ok := h.Predict(3); !ok || v != 300 {
		t.Fatalf("Predict(3) = (%v,%v), want (300,true)", v, ok)
	}
}

func TestHoldLastWindowValuesIsIndependentSnapshot(t *testing.T) {
	h := NewHold()
	h.Seed(1, 10)

	snap := h.LastWindowValues()
	if len(snap) != 1 || snap[1] != 10 {
		t.Fatalf("LastWindowValues = %v, want map[1:10]", snap)
	}

	// Mutating the returned map must not affect the predictor's own state.
	snap[1] = 999
	snap[2] = 42
	if v, _ := h.Predict(1); v != 10 {
		t.Fatalf("Predict(1) = %v after mutating snapshot, want unaffected 10", v)
	}
	if _, ok := h.Predict(2); ok {
		t.Fatal("Predict(2) should still be unseeded; snapshot mutation leaked into predictor state")
	}
}

func TestHoldSatisfiesPredictorInterface(t *testing.T) {
	var _ Predictor = NewHold()
}
