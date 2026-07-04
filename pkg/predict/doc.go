// Package predict implements the baseline predictor interface used to turn
// raw samples into residuals before they enter the sketch.
//
// See ADR-003 (open-loop residuals / HOLD predictor) and ADR-008 (Seed on
// birth, Evict on tombstone).
package predict
