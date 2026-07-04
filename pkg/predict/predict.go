package predict

import "github.com/purushpsm147/palimpsest/pkg/wire"

// Predictor supplies the open-loop baseline (ADR-003) that a residual is
// measured against before it is sketched. "Open-loop" means the baseline
// only ever changes in response to exact data the encoder and decoder both
// possess — a birth's init_value (Seed) or a keyframe's exact values
// (LoadKeyframe) — never in response to (lossy, recovered) residuals. This
// is what lets encoder and decoder agree on the baseline without a
// feedback channel.
type Predictor interface {
	// ID identifies the predictor implementation carried on the wire
	// (Frame.Predictor), so a decoder can select a compatible one.
	ID() wire.PredictorID

	// Predict returns the current baseline for id. ok is false when the
	// predictor has no baseline for id yet (never Seeded, or Evicted and
	// not re-Seeded): callers should take the bootstrap path — treat this
	// sample as a birth/re-birth rather than computing a residual against
	// a nonexistent baseline.
	Predict(id uint64) (float64, bool)

	// Seed initializes (or re-initializes) id's baseline to v. Called on
	// dictionary birth (ADR-008): the first sample for a new logical
	// series becomes its baseline outright rather than contributing a
	// sketched residual.
	Seed(id uint64, v float64)

	// Evict removes id's baseline. Called when id is tombstoned
	// (ADR-008): a later birth of the same ID starts fresh via Seed.
	Evict(id uint64)

	// LoadKeyframe replaces the predictor's entire baseline set with
	// values, e.g. after encoding/decoding a keyframe: this is the only
	// steady-state (non-birth) point at which baselines move, keeping the
	// coding open-loop.
	LoadKeyframe(values map[uint64]float64)

	// LastWindowValues returns a snapshot of the predictor's current
	// baseline for every known id. Used to build the "prev" map a KDELTA
	// keyframe is delta-coded against (ADR-011).
	LastWindowValues() map[uint64]float64
}

// Hold is the v0 predictor (ADR-003): the baseline for a series is held
// constant at whatever it was last explicitly set to (via Seed or
// LoadKeyframe) until the next such call. It never updates in response to
// Predict or to sketched/recovered residuals.
//
// Hold is NOT safe for concurrent use.
type Hold struct {
	values map[uint64]float64
}

// NewHold returns an empty Hold predictor.
func NewHold() *Hold {
	return &Hold{values: make(map[uint64]float64)}
}

// ID implements Predictor.
func (h *Hold) ID() wire.PredictorID { return wire.PredictorHold }

// Predict implements Predictor.
func (h *Hold) Predict(id uint64) (float64, bool) {
	v, ok := h.values[id]
	return v, ok
}

// Seed implements Predictor.
func (h *Hold) Seed(id uint64, v float64) {
	h.values[id] = v
}

// Evict implements Predictor.
func (h *Hold) Evict(id uint64) {
	delete(h.values, id)
}

// LoadKeyframe implements Predictor.
func (h *Hold) LoadKeyframe(values map[uint64]float64) {
	next := make(map[uint64]float64, len(values))
	for id, v := range values {
		next[id] = v
	}
	h.values = next
}

// LastWindowValues implements Predictor.
func (h *Hold) LastWindowValues() map[uint64]float64 {
	out := make(map[uint64]float64, len(h.values))
	for id, v := range h.values {
		out[id] = v
	}
	return out
}
