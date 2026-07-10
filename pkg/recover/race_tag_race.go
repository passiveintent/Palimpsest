//go:build race

/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package recover

// wallTimeRaceMultiplier scales absolute wall-time test budgets (see
// group_test.go's testGroupCaseWallTime) when built with -race: the race
// detector's instrumentation overhead (measured here at ~15-20x for this
// package's escalation path) is orthogonal to real performance and would
// otherwise make those budgets fail under -race for reasons that have
// nothing to do with an actual regression.
const wallTimeRaceMultiplier = 20
