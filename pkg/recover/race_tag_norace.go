//go:build !race

/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package recover

// wallTimeRaceMultiplier is 1 outside -race builds; see race_tag_race.go.
const wallTimeRaceMultiplier = 1
