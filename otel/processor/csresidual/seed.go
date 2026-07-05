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
// bucket of width rotate. Using an absolute wall-clock bucket (rather than
// a process-start-relative counter) means independent emitters/restarts
// that share a rotate interval land on the same epoch boundaries, which is
// what lets a decoder treat "epoch" as a coordinate system independent of
// any single agent's uptime.
func epochIndex(t time.Time, rotate time.Duration) uint64 {
	if rotate <= 0 {
		return 0
	}
	return uint64(t.UnixNano() / rotate.Nanoseconds())
}
