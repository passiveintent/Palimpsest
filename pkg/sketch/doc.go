/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package sketch implements the encode-path accumulator: implicit
// measurement-matrix hashing, the per-emitter lifecycle tracker, storm
// detection, and HKDF-based ephemeral seed derivation.
//
// See ADR-002 (hash-implicit Phi), ADR-003 (open-loop residuals), ADR-004
// (storm fallback), ADR-008 (ephemeral identity / lifecycle), ADR-009
// (churn circuit breaker), ADR-012 (HKDF seed derivation), and ADR-013
// (per-window reset).
package sketch
