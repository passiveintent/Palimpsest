/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package core

import (
	"testing"
	"time"
)

// BenchmarkWatermark measures Watermark.OnFrameArrival's bookkeeping
// throughput: the ADR-013 watermark/repair engine's hot path, called once
// per arriving RESIDUAL frame in handleResidual.
func BenchmarkWatermark(b *testing.B) {
	w := NewWatermark(3, 10*time.Minute)
	now := time.Unix(1_700_000_000, 0)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.OnFrameArrival(uint32(i), now)
		now = now.Add(10 * time.Second)
	}
}
