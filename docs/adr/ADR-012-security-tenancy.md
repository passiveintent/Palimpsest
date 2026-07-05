# ADR-012: Security & Multi-Tenancy

**Status:** Accepted (pre-alpha; implementation pending)

## Problem

Pull endpoint is SSRF surface; cross-tenant sketch leak is real.

## Decision

Speculative push replaces the pull endpoint (zero listening sockets by
default). Tenancy is a hard boundary — sketches never sum across tenants.
Seeds derive via HKDF-SHA256(tenant_key, …), not public convention.
Explicit "CS is not encryption" policy.

## Addendum: key_version and tenant-key rotation (wire v2)

**Rationale.** Epoch rotation (ADR-013) bounds how long a given
`seed_ephemeral` is live, but it cannot survive a tenant_key **compromise**:
`epoch_index` is public (it is on the wire), so an adversary who obtains a
compromised key can recompute every past seed. `key_version` (a u8 stamped
in each v2 frame header, SPEC.md §"Frame Layout") lets a tenant cycle from
`key_v(n)` to `key_v(n+1)` after a compromise:

1. Add the new key to the `tenant_keys` list in the collector config with a
   higher `version` and set `active_key_version` to that version.
2. On config reload (no restart required), new frames are stamped with the
   new version; in-flight windows encoded under the old version are still
   decodable because the old key remains in the `KeyRing` until explicitly
   removed.
3. Frames within `repair_horizon` (ADR-013) encoded under the old version
   can still be merged and recovered; the decoder looks up each frame's
   `key_version` in its `KeyRing` and derives the correct seed.
4. Once all in-flight windows have closed (i.e. after `repair_horizon`
   elapses), remove the old key from `tenant_keys`.

**Invariants.**
- `key_version` 0 is the implicit version for v1 frames on decode.
- Unknown `key_version` → frame is gated at low confidence; the
  `keyring_miss` metric increments; the frame is never silently dropped.
- KDELTA keyframe chains span a single (epoch, key_version) coordinate
  system; a new key_version implies a coordinate-system break equivalent
  to a new epoch (the first frame must be a golden keyframe).
- `KeyRing` is multi-tenant-scoped: keys for different tenants are never
  mixed. Seeds still derive from HKDF-SHA256(tenant_key, shard_id ||
  epoch_index || view_id) — the algorithm is unchanged.
