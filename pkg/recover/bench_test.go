/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package recover

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// BenchmarkRecover measures FISTA + debias decode time at
// testdata/golden/recovery_case1.json's scale (docs/PERF.md's "decode time
// (m=2000, N=50k)"): N=50k tracked series, m=2000 measurements, d=6
// hashes/series, k=50 planted deviations.
func BenchmarkRecover(b *testing.B) {
	const (
		n    = 50000
		m    = 2000
		d    = 6
		k    = 50
		seed = 0xC0FFEE
	)

	dict := NewDictionary()
	names := make([]string, n)
	for i := range names {
		name := fmt.Sprintf("bench_metric_%06d|agg=sum", i)
		names[i] = name
		dict.ApplyDelta(wire.DictDelta{ID: sketch.SeriesID([]byte(name)), Name: []byte(name)})
	}
	dict.ObserveEmitter(1)

	acc := sketch.NewAccumulator(sketch.Params{M: m, D: d, Seed: seed, Bits: 8})
	rng := rand.New(rand.NewSource(1))
	for _, i := range rng.Perm(n)[:k] {
		acc.Update([]byte(names[i]), 50.0+rng.Float64()*10)
	}
	y, _ := acc.Flush()

	params := sketch.Params{M: m, D: d, Seed: seed}
	opts := Options{Iters: 350, Lambda: 0.05, PowerIters: 50, Threshold: 0.3, EmittersExpected: 1}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Recover(y, dict, params, opts); err != nil {
			b.Fatal(err)
		}
	}
}
