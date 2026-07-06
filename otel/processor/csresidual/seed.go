/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package csresidual

import (
	"fmt"
	"os"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/sketch"
)

// loadTenantKey reads the HKDF input keying material from the environment
// variable named envVar (Config.TenantKeyEnv). Per ADR-012, the tenant key
// is secret and must never be derived from public/predictable data; this
// processor only ever reads it from the environment (never logs it, never
// accepts it directly in YAML) so it doesn't end up committed to config
// files or collector logs.
func loadTenantKey(envVar string) ([]byte, error) {
	v := os.Getenv(envVar)
	if v == "" {
		return nil, fmt.Errorf("csresidual: environment variable %q (tenant_key_env) is empty or unset", envVar)
	}
	return []byte(v), nil
}

// deriveSeed wraps sketch.DeriveEphemeralSeed (HKDF-SHA256(tenant_key,
// shard_id || epoch_index || view_id), ADR-012) as this processor's single
// entry point for per-(shard, epoch, view) seed derivation, so every
// pipeline computes its Accumulator seed identically.
func deriveSeed(tenantKey []byte, shardID uint64, epochIdx uint32, viewID uint16) uint64 {
	return sketch.DeriveEphemeralSeed(tenantKey, shardID, epochIdx, viewID)
}

// epochIndex returns the ADR-013 epoch index for t: a wall-clock-aligned
// bucket of width rotate, delayed by emitterID's deterministic
// sketch.EpochJitter offset into [0, jitterWindow) (ADR-013 §Addendum
// "herd jitter": spreads a fleet sharing one rotate interval across
// jitterWindow instead of all rotating at the same instant). Using an
// absolute wall-clock bucket (rather than a process-start-relative counter)
// means independent emitters/restarts that share a rotate interval still
// land on the same *unshifted* boundaries, which is what lets a decoder
// treat "epoch" as a coordinate system independent of any single agent's
// uptime; jitterWindow only shifts *when* each emitter crosses that
// boundary, never the boundary grid itself.
func epochIndex(t time.Time, rotate, jitterWindow time.Duration, emitterID uint64) uint64 {
	return sketch.EpochIndex(t, rotate, jitterWindow, emitterID)
}

// loadKeyRing builds a sketch.KeyRing from cfg.TenantKeys. Each entry's key
// material is read from the environment variable it names. Returns an error
// if any env var is empty; the config's Validate() must have been called
// first, so by the time loadKeyRing is called env vars are guaranteed set.
func loadKeyRing(entries []TenantKeyEntry) (sketch.KeyRing, error) {
	ring := make(sketch.KeyRing, len(entries))
	for _, e := range entries {
		v := os.Getenv(e.EnvVar)
		if v == "" {
			return nil, fmt.Errorf("csresidual: tenant_keys version=%d: env var %q is empty or unset", e.Version, e.EnvVar)
		}
		ring[e.Version] = []byte(v)
	}
	return ring, nil
}
