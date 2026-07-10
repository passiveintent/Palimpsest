/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package recover implements the decode-path sparse recovery solver
// (FISTA), the point-query fast path for named series, and debiasing.
//
// See ADR-002 (recovery over the implicit Phi), ADR-008 (dictionary
// lifecycle), ADR-009 (point-query substrate), and ADR-013 (coverage,
// watermark revisions).
package recover
