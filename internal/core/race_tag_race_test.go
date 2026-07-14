//go:build race

/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package core

// wallTimeRaceMultiplier scales absolute wall-time test budgets (see
// merged_e2e_test.go's ADR-014 decode-budget check) when built with
// -race, mirroring pkg/recover's identically named constant: the race
// detector's instrumentation overhead (5-20x on the escalation path) is
// orthogonal to real performance and would otherwise fail those budgets
// for reasons that have nothing to do with an actual regression — CI runs
// this package under `go test -race`.
const wallTimeRaceMultiplier = 20
