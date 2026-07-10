//go:build !race

/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package recover

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// benchRegressionTolerance is the allowed slack over the checked-in
// baseline before TestBenchmarkRecoverRegression fails. testdata/
// bench_baseline.txt is set so this tolerance's ceiling lands exactly on
// PERF.md's 0.5s stretch target (400ms baseline * 1.25 = 500ms): generous
// enough to absorb CI-runner-vs-dev-machine variance (this repo's own
// benchmarks are measured on "a single developer machine, not a dedicated
// benchmark host" — see PERF.md), while still catching a real regression
// back toward the pre-optimization ~800ms/3.1s territory this perf pass
// started from (see PERF.md's before/after table).
const benchRegressionTolerance = 1.25

// TestBenchmarkRecoverRegression is the CI benchmark-regression smoke test
// (Prompt 12 acceptance criteria): fails if BenchmarkRecover regresses more
// than benchRegressionTolerance against testdata/bench_baseline.txt. Built
// with the !race tag: go test -race's instrumentation overhead (5-20x,
// unevenly distributed across parallel vs. serial code) would make a
// wall-clock comparison meaningless, so ci.yml runs this in its own
// non-race step instead of the existing -race step.
func TestBenchmarkRecoverRegression(t *testing.T) {
	baseline := readBenchBaseline(t)
	limit := baseline * benchRegressionTolerance

	result := testing.Benchmark(BenchmarkRecover)
	current := float64(result.NsPerOp())

	t.Logf("BenchmarkRecover: %.0f ns/op (baseline %.0f ns/op, limit %.0f ns/op = %.0f%% of baseline)",
		current, baseline, limit, benchRegressionTolerance*100)
	if current > limit {
		t.Errorf("BenchmarkRecover regressed: %.0f ns/op > %.0f ns/op limit (%.0f%% of baseline %.0f ns/op in testdata/bench_baseline.txt)",
			current, limit, benchRegressionTolerance*100, baseline)
	}
}

func readBenchBaseline(t *testing.T) float64 {
	t.Helper()
	data, err := os.ReadFile("testdata/bench_baseline.txt")
	if err != nil {
		t.Fatalf("reading testdata/bench_baseline.txt: %v", err)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil {
		t.Fatalf("parsing testdata/bench_baseline.txt: %v", err)
	}
	return v
}
