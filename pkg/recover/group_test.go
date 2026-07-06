/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package recover

// Tests for ADR-014 Group-OMP (Prompt 11c): groupomp.go, restart in fista.go,
// and escalation in Recover().

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// groupTestTenantKey mirrors oracle/palimpsest_ref.py TEST_TENANT_KEY
// ("test-vector-key"), used to derive deterministic ephemeral seeds in
// synthetic scenario tests without depending on a sketch package constant.
var groupTestTenantKey = []byte("test-vector-key")

type groupCaseFile struct {
	N          int     `json:"n"`
	M          int     `json:"m"`
	D          int     `json:"d"`
	NGroup     int     `json:"n_group"`
	NScattered int     `json:"n_scattered"`
	Seed       uint64  `json:"seed"`
	Lambda     float64 `json:"lambda"`
	Threshold  float64 `json:"threshold"`
	Iters      int     `json:"iters"`
	PowerIters int     `json:"power_iters"`

	AZGroupLabel        string  `json:"az_group_label"`
	AZGroupID           uint64  `json:"az_group_id"`
	PlainFISTAResidual  float64 `json:"plain_fista_residual"`
	PlainFISTARecovered int     `json:"plain_fista_n_recovered"`

	Names            []string             `json:"names"`
	GroupSupport     []goldenSupportEntry `json:"group_support"`
	ScatteredSupport []goldenSupportEntry `json:"scattered_support"`
	Y                []float64            `json:"y"`
}

// buildGroupDict populates a Dictionary from all names in f.
func buildGroupDict(f groupCaseFile) *Dictionary {
	dict := NewDictionary()
	for _, name := range f.Names {
		nb := []byte(name)
		dict.ApplyDelta(wire.DictDelta{ID: sketch.SeriesID(nb), Name: nb})
	}
	dict.ObserveEmitter(1)
	return dict
}

// ============================================================
// Test 1: Golden AZ case — primary acceptance fixture
// ============================================================

func TestGoldenGroupCase(t *testing.T) {
	var f groupCaseFile
	loadGoldenJSON(t, "recovery_group_case.json", &f)
	dict := buildGroupDict(f)
	params := sketch.Params{M: f.M, D: f.D, Seed: f.Seed}
	base := Options{Iters: f.Iters, Lambda: f.Lambda, PowerIters: f.PowerIters, Threshold: f.Threshold}

	t.Run("plain_fista_documents_failure", func(t *testing.T) {
		testGroupCasePlainFailure(t, f, dict, params, base)
	})
	t.Run("recover_group_flags_az_group_and_recall_scattered", func(t *testing.T) {
		testGroupCaseLatchRecovery(t, f, dict, params, base)
	})
	t.Run("wall_time_at_most_2x_plain", func(t *testing.T) {
		testGroupCaseWallTime(t, f, dict, params, base)
	})
}

func testGroupCasePlainFailure(t *testing.T, f groupCaseFile, dict *Dictionary, params sketch.Params, base Options) {
	t.Helper()
	opts := base
	opts.EmittersExpected = 1
	res, err := Recover(f.Y, dict, params, opts)
	if err != nil {
		t.Fatalf("plain Recover: %v", err)
	}
	if res.Confidence != ConfidenceRecovered {
		t.Fatalf("confidence = %q, want %q", res.Confidence, ConfidenceRecovered)
	}
	// Document the ADR-004 cliff: either residual > 0.3 OR raw support > M/10.
	// "raw_support < 600" was removed — lambda conformance at 0.05 cannot zero
	// background singletons when 500 AZ-group series push the noise floor to ~6×
	// lamOverL (ADR-014 §Negative result records the phase-transition analysis).
	cliff := res.Residual > 0.3 || res.RawSupport > f.M/10
	if !cliff {
		t.Fatalf("plain FISTA shows no cliff: residual=%.4f raw=%d M/10=%d",
			res.Residual, res.RawSupport, f.M/10)
	}
	t.Logf("plain FISTA residual=%.4f raw_support=%d supportIDs=%d (M/10=%d) restarts=%d — cliff confirmed",
		res.Residual, res.RawSupport, len(res.SupportIDs), f.M/10, res.Restarts)
}

func testGroupCaseLatchRecovery(t *testing.T, f groupCaseFile, dict *Dictionary, params sketch.Params, base Options) {
	t.Helper()
	opts := base
	opts.EmittersExpected = 1
	opts.EscalateThreshold = 0.3 // Group-OMP path

	res, err := Recover(f.Y, dict, params, opts)
	if err != nil {
		t.Fatalf("Recover (escalating): %v", err)
	}
	if res.Confidence != ConfidenceRecoveredGroup {
		t.Fatalf("Confidence = %q, want %q", res.Confidence, ConfidenceRecoveredGroup)
	}

	// AZ group must be flagged.
	azFlagged := false
	for _, gid := range res.GroupIDs {
		if gid == f.AZGroupID {
			azFlagged = true
			break
		}
	}
	if !azFlagged {
		t.Fatalf("AZ group ID %d not in GroupIDs (len=%d)", f.AZGroupID, len(res.GroupIDs))
	}

	// Post-debias AZ member recall >= 0.90.
	azIDs := make(map[uint64]struct{}, f.NGroup)
	for i := 0; i < f.NGroup; i++ {
		nb := []byte(f.Names[i])
		azIDs[sketch.SeriesID(nb)] = struct{}{}
	}
	recovByID := make(map[uint64]struct{}, len(res.SupportIDs))
	for _, id := range res.SupportIDs {
		recovByID[id] = struct{}{}
	}
	var membersIn int
	for id := range azIDs {
		if _, ok := recovByID[id]; ok {
			membersIn++
		}
	}
	memberRecall := float64(membersIn) / float64(f.NGroup)
	if memberRecall < 0.90 {
		t.Fatalf("AZ member recall = %.2f (%d/%d), want >= 0.90", memberRecall, membersIn, f.NGroup)
	}

	// Scattered recall >= 0.90.
	var hit int
	for _, s := range f.ScatteredSupport {
		if _, ok := recovByID[s.ID]; ok {
			hit++
		}
	}
	scatterRecall := float64(hit) / float64(len(f.ScatteredSupport))
	if scatterRecall < 0.90 {
		t.Fatalf("scattered recall = %.2f (%d/%d), want >= 0.90",
			scatterRecall, hit, len(f.ScatteredSupport))
	}

	// H0 z-score > 10 (assert; log expected O(10–70)).
	zScore := computeAZZScore(t, f, dict, params, opts)
	t.Logf("AZ H0 z-score = %.1f (assert > 10)", zScore)
	if zScore < 10.0 {
		t.Errorf("AZ H0 z-score = %.1f, want > 10", zScore)
	}

	t.Logf("Recover(OMP): residual=%.4f member_recall=%.2f (%d/%d) scattered=%.2f (%d/%d) groups=%d support=%d restarts=%d",
		res.Residual, memberRecall, membersIn, f.NGroup,
		scatterRecall, hit, len(f.ScatteredSupport), len(res.GroupIDs), len(res.SupportIDs), res.Restarts)
}

// computeAZZScore computes the H0 z-score of the AZ group on the plain residual.
func computeAZZScore(t *testing.T, f groupCaseFile, dict *Dictionary, params sketch.Params, opts Options) float64 {
	t.Helper()
	csr := dict.BuildCSR(params.Seed, params.M, params.D)
	n := csr.NRows
	ids := dict.ActiveIDs()
	lambdaAbs := scaledLambda(csr, f.Y, opts.Lambda, n)
	x, _ := fista(csr, f.Y, lambdaAbs, opts.Iters, opts.PowerIters)
	// Use M/10 (pre-OMP plain context) for z-score computation so we measure
	// the AZ group's detectability from the diagnostic baseline, not the
	// M/5-debiased residual where many AZ members are already explained.
	diagRows := cappedSupport(x, opts.Threshold, params.M/10)
	diagVals := debias(csr, f.Y, diagRows, debiasRidge)
	r := make([]float64, params.M)
	reconstructInto(csr, diagRows, diagVals, r)
	for i := range r {
		r[i] = f.Y[i] - r[i]
	}
	rNorm := l2norm(r)
	sigmaHat := rNorm / math.Sqrt(float64(params.M))
	if sigmaHat == 0 {
		return 0
	}

	grouper := DefaultGrouper()
	gm := make(map[uint64]*groupEntry)
	for i, id := range ids {
		gid := grouper(id, dict)
		g := gm[gid]
		if g == nil {
			g = &groupEntry{id: gid}
			gm[gid] = g
		}
		g.rows = append(g.rows, i)
	}
	azG := gm[f.AZGroupID]
	if azG == nil {
		return 0
	}
	var normSq float64
	for _, i := range azG.rows {
		var dot float64
		for k := csr.RowPtr[i]; k < csr.RowPtr[i+1]; k++ {
			dot += csr.Vals[k] * r[csr.ColIdx[k]]
		}
		normSq += dot * dot
	}
	score := math.Sqrt(normSq) / math.Sqrt(float64(len(azG.rows)))
	std := sigmaHat / math.Sqrt(2*float64(len(azG.rows)))
	if std == 0 {
		return 0
	}
	return (score - sigmaHat) / std
}

func testGroupCaseWallTime(t *testing.T, f groupCaseFile, dict *Dictionary, params sketch.Params, base Options) {
	t.Helper()
	opts := base
	opts.EmittersExpected = 1

	t0 := time.Now()
	_, err := Recover(f.Y, dict, params, opts)
	dPlain := time.Since(t0)
	if err != nil {
		t.Fatalf("plain Recover: %v", err)
	}

	optsEsc := opts
	optsEsc.EscalateThreshold = 0.3
	t1 := time.Now()
	_, err = Recover(f.Y, dict, params, optsEsc)
	dGroup := time.Since(t1)
	if err != nil {
		t.Fatalf("escalating Recover: %v", err)
	}

	ratio := float64(dGroup) / float64(dPlain)
	t.Logf("plain FISTA: %v  RecoverGroup: %v  ratio=%.2f×", dPlain, dGroup, ratio)
	if dGroup > time.Duration(float64(dPlain)*2.0) {
		t.Fatalf("RecoverGroup %v > 2.0× plain %v (ratio=%.2f×)", dGroup, dPlain, ratio)
	}
}

// ============================================================
// Test 2: Monotone-ish
// ============================================================

func TestRestartPreventsGroupLatch(t *testing.T) {
	var f groupCaseFile
	loadGoldenJSON(t, "recovery_group_case.json", &f)
	dict := buildGroupDict(f)
	params := sketch.Params{M: f.M, D: f.D, Seed: f.Seed}
	opts := Options{
		Iters: f.Iters, Lambda: f.Lambda, PowerIters: f.PowerIters,
		Threshold: f.Threshold, EmittersExpected: 1,
	}

	plain, err := Recover(f.Y, dict, params, opts)
	if err != nil {
		t.Fatalf("plain Recover: %v", err)
	}
	t.Logf("plain  residual=%.4f restarts=%d raw_support=%d supportIDs=%d",
		plain.Residual, plain.Restarts, plain.RawSupport, len(plain.SupportIDs))

	group, err := RecoverGroup(f.Y, dict, params, DefaultGrouper(), opts)
	if err != nil {
		t.Fatalf("RecoverGroup: %v", err)
	}
	t.Logf("group  residual=%.4f restarts=%d support=%d groups=%d",
		group.Residual, group.Restarts, len(group.SupportIDs), len(group.GroupIDs))

	if group.Residual >= plain.Residual {
		t.Fatalf("group residual %.4f >= plain %.4f; Group-OMP must win", group.Residual, plain.Residual)
	}
}

// ============================================================
// Test 3: Escalation fires via support-size
// ============================================================

func TestEscalationFiresViaSupportSize(t *testing.T) {
	var f groupCaseFile
	loadGoldenJSON(t, "recovery_group_case.json", &f)
	dict := buildGroupDict(f)
	params := sketch.Params{M: f.M, D: f.D, Seed: f.Seed}
	opts := Options{
		Iters: f.Iters, Lambda: f.Lambda, PowerIters: f.PowerIters,
		Threshold: f.Threshold, EmittersExpected: 1,
		EscalateThreshold: 999.0, // residual arm disabled
	}

	res, err := Recover(f.Y, dict, params, opts)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	t.Logf("confidence=%q residual=%.4f raw_support=%d supportIDs=%d M/10=%d",
		res.Confidence, res.Residual, res.RawSupport, len(res.SupportIDs), f.M/10)

	if res.Confidence != ConfidenceRecoveredGroup {
		t.Fatalf("support-size escalation did not fire: confidence=%q (raw=%d M/10=%d EscThresh=999)",
			res.Confidence, res.RawSupport, f.M/10)
	}
}

// ============================================================
// Test 4: Regression guard
// ============================================================

func TestEscalationDoesNotRegressCase1(t *testing.T) {
	var f recoveryCase1File
	loadGoldenJSON(t, "recovery_case1.json", &f)
	if len(f.Support) != f.K {
		t.Fatalf("support=%d != k=%d", len(f.Support), f.K)
	}
	dict := NewDictionary()
	for _, name := range f.Names {
		nb := []byte(name)
		dict.ApplyDelta(wire.DictDelta{ID: sketch.SeriesID(nb), Name: nb})
	}
	dict.ObserveEmitter(1)

	params := sketch.Params{M: f.M, D: f.D, Seed: f.Seed}
	opts := Options{
		Iters: f.Iters, Lambda: f.Lambda, PowerIters: f.PowerIters,
		Threshold: f.Threshold, EmittersExpected: 1, EscalateThreshold: 0.3,
	}

	res, err := Recover(f.Y, dict, params, opts)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if res.Confidence == ConfidenceRecoveredGroup {
		t.Errorf("escalation unexpectedly fired (residual=%.4f raw=%d M/10=%d)",
			res.Residual, res.RawSupport, f.M/10)
	}
	// Restarts <= 3: sanity bound. The best-seen restart is cheap insurance;
	// on easy convergent cases it should be dormant (ADR-014 §1).
	if res.Restarts > 3 {
		t.Errorf("Restarts = %d, want <= 3", res.Restarts)
	}
	checkRecallAndRMSE(t, "escalation_case1", f.Support, res, 0.95, 0.05)
	t.Logf("recovery_case1: confidence=%q residual=%.4f restarts=%d support=%d",
		res.Confidence, res.Residual, res.Restarts, len(res.SupportIDs))
}

func TestEscalationTriggersOnGroupCase(t *testing.T) {
	var f groupCaseFile
	loadGoldenJSON(t, "recovery_group_case.json", &f)
	dict := buildGroupDict(f)
	params := sketch.Params{M: f.M, D: f.D, Seed: f.Seed}
	opts := Options{
		Iters: f.Iters, Lambda: f.Lambda, PowerIters: f.PowerIters,
		Threshold: f.Threshold, EmittersExpected: 1, EscalateThreshold: 0.3,
	}

	res, err := Recover(f.Y, dict, params, opts)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if res.Confidence != ConfidenceRecoveredGroup {
		t.Fatalf("escalation did not fire: confidence=%q residual=%.4f raw=%d M/10=%d",
			res.Confidence, res.Residual, res.RawSupport, f.M/10)
	}
	azFlagged := false
	for _, gid := range res.GroupIDs {
		if gid == f.AZGroupID {
			azFlagged = true
			break
		}
	}
	if !azFlagged {
		t.Fatalf("AZ group ID %d not in GroupIDs (len=%d)", f.AZGroupID, len(res.GroupIDs))
	}
	t.Logf("escalation fired: confidence=%q residual=%.4f restarts=%d groups=%d",
		res.Confidence, res.Residual, res.Restarts, len(res.GroupIDs))
}

// ============================================================
// Test 5: Seed sweep and null test
// ============================================================

// TestGroupOMPSeedSweep rebuilds the golden-case y with 5 different
// measurement-matrix seeds and asserts AZ group detected in all 5.
// Uses the golden case's x_true (from group_support+scattered_support)
// with fresh Phi per seed (view_id 0,2,3,4,5; oracle used view_id=1).
func TestGroupOMPSeedSweep(t *testing.T) {
	var f groupCaseFile
	loadGoldenJSON(t, "recovery_group_case.json", &f)

	xTrueByName := make(map[string]float64, f.NGroup+f.NScattered)
	for _, s := range f.GroupSupport {
		xTrueByName[s.Name] = s.Residual
	}
	for _, s := range f.ScatteredSupport {
		xTrueByName[s.Name] = s.Residual
	}

	var detected int
	for _, viewID := range []int{0, 2, 3, 4, 5} {
		seed := sketch.DeriveEphemeralSeed(groupTestTenantKey, uint64(1), uint32(0), uint16(viewID))
		params := sketch.Params{M: f.M, D: f.D, Seed: seed}

		dict := buildGroupDict(f)
		csr := dict.BuildCSR(seed, f.M, f.D)
		ids := dict.ActiveIDs()

		xTrue := make([]float64, len(ids))
		for i, id := range ids {
			for _, nm := range f.Names {
				if sketch.SeriesID([]byte(nm)) == id {
					xTrue[i] = xTrueByName[nm]
					break
				}
			}
		}

		y := make([]float64, f.M)
		csr.MulTransposeInto(xTrue, y)

		opts := Options{
			Iters: f.Iters, Lambda: f.Lambda, PowerIters: f.PowerIters,
			Threshold: f.Threshold, EmittersExpected: 1, EscalateThreshold: 0.3,
		}
		res, err := Recover(y, dict, params, opts)
		if err != nil {
			t.Logf("viewID=%d: error %v", viewID, err)
			continue
		}
		t.Logf("viewID=%d: conf=%q residual=%.2f groups=%d raw=%d",
			viewID, res.Confidence, res.Residual, len(res.GroupIDs), res.RawSupport)
		for _, gid := range res.GroupIDs {
			if gid == f.AZGroupID {
				detected++
				break
			}
		}
	}
	if detected < 5 {
		t.Errorf("AZ group detected %d/5 seeds, want 5/5", detected)
	}
}

// TestGroupOMPNullTest asserts zero false alarms on the AZ group partition
// when only scattered singletons are anomalous (no correlated group).
func TestGroupOMPNullTest(t *testing.T) {
	const nSeries, m, d = 10000, 500, 6
	const nGroup, nScattered = 100, 10

	names := make([][]byte, nSeries)
	for i := range names {
		if i < nGroup {
			names[i] = []byte(fmt.Sprintf("nl_%05d|region=az-east-1,cluster=prod|agg=sum", i))
		} else {
			names[i] = []byte(fmt.Sprintf("nl_%05d|uid=%d,cluster=prod|agg=sum", i, i))
		}
	}

	rng := rand.New(rand.NewSource(5678))
	xTrue := make([]float64, nSeries) // NO group anomaly
	for _, idx := range rng.Perm(nSeries - nGroup)[:nScattered] {
		amp := rng.Float64()*4 + 8 // large singletons only
		if rng.Float64() < 0.5 {
			amp = -amp
		}
		xTrue[nGroup+idx] = amp
	}

	tmpDict := NewDictionary()
	tmpDict.ApplyDelta(wire.DictDelta{ID: sketch.SeriesID(names[0]), Name: names[0]})
	azGroupID := DefaultGrouper()(sketch.SeriesID(names[0]), tmpDict)

	var falseAlarms int
	for viewID := 0; viewID < 5; viewID++ {
		seed := sketch.DeriveEphemeralSeed(groupTestTenantKey, uint64(1), uint32(0), uint16(viewID))
		params := sketch.Params{M: m, D: d, Seed: seed}

		dict := NewDictionary()
		for _, nm := range names {
			dict.ApplyDelta(wire.DictDelta{ID: sketch.SeriesID(nm), Name: nm})
		}
		dict.ObserveEmitter(1)

		csr := dict.BuildCSR(seed, m, d)
		y := make([]float64, m)
		csr.MulTransposeInto(xTrue, y)

		opts := Options{
			Iters: 350, Lambda: 0.05, PowerIters: 50, Threshold: 0.3,
			EmittersExpected: 1, EscalateThreshold: 0.3,
		}
		res, err := Recover(y, dict, params, opts)
		if err != nil {
			t.Logf("null viewID=%d: error %v", viewID, err)
			continue
		}
		for _, gid := range res.GroupIDs {
			if gid == azGroupID {
				falseAlarms++
				t.Logf("null viewID=%d: AZ false alarm! conf=%q residual=%.2f", viewID, res.Confidence, res.Residual)
				break
			}
		}
	}
	if falseAlarms > 0 {
		t.Errorf("false alarms %d/5, want 0 (H0 tau too low)", falseAlarms)
	}
}
