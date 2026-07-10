/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package audit

import "expvar"

// Publish registers a's live Report under expvar at the given top-level
// name (e.g. "palimpsest_audit"), mirroring internal/metrics.Metrics.Publish.
// Safe to call more than once on the same Auditor; only the first call
// takes effect.
func (a *Auditor) Publish(name string) {
	a.publishOnce.Do(func() {
		expvar.Publish(name, expvar.Func(func() any { return a.Report() }))
	})
}
