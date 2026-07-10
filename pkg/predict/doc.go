/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package predict implements the baseline predictor interface used to turn
// raw samples into residuals before they enter the sketch.
//
// See ADR-003 (open-loop residuals / HOLD predictor) and ADR-008 (Seed on
// birth, Evict on tombstone).
package predict
