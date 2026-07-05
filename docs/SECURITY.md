# Security & Threat Model

Threat model per [ADR-012](adr/ADR-012-security-tenancy.md), covering the
wire codec, the OTel processor (`otel/processor/csresidual`), and the
reconstructor (`cmd/palimpsestd`) as implemented.

## Threat model summary (from ADR-012)

- **No listening sockets by default.** Agents push frames and (optionally)
  snapshot blobs speculatively; there is no pull/query endpoint unless an
  operator explicitly opts in. Nothing to scan, nothing to SSRF.
- **Tenant boundary is hard.** Sketches never sum across tenants. Seeds are
  derived per-tenant via `HKDF-SHA256(tenant_key, shard_id || epoch_index ||
  view_id)` (see [docs/SPEC.md](SPEC.md)), not a public/well-known
  convention — an attacker who doesn't hold `tenant_key` cannot forge bucket
  assignments or correlate another tenant's sketch.
- **Compressed sensing is not encryption.** The measurement matrix Phi and
  quantized residuals are not a confidentiality mechanism; treat frames on
  the wire as you would any other telemetry payload (TLS in transit, access
  control at rest).
- **Optional pull endpoint** (`pull.enabled`, default `false`): when enabled,
  requires mTLS *and* a bearer token (`pull.bearer_token_env`). Off by
  default.

## NetworkPolicy example

Illustrative only — agents that never enable `pull.enabled` need no ingress
at all; only egress to the collector/backend is required.

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: palimpsest-agent-deny-ingress
spec:
  podSelector:
    matchLabels:
      app: palimpsest-agent
  policyTypes:
    - Ingress
    - Egress
  ingress: []  # no listening sockets; deny all ingress
  egress:
    - to:
        - podSelector:
            matchLabels:
              app: palimpsest-collector
      ports:
        - protocol: TCP
          port: 4317
```

See [demo/networkpolicy-csresidual.yaml](../demo/networkpolicy-csresidual.yaml)
for the fuller, two-policy version (deny-all-ingress by default, plus the
narrow allow rule needed only if an operator has deliberately opted into
`pull.enabled`).

## Key rotation

`tenant_key` (read from the environment named by `tenant_key_env`, never
from YAML or a flag — ADR-012) is the sole HKDF input keying every seed an
agent derives: `HKDF-SHA256(tenant_key, shard_id || epoch_index ||
view_id)`. Two properties of that derivation shape how rotation has to
work:

- **Seeds are pure functions of `(tenant_key, shard_id, epoch_index,
  view_id)`.** There is no stored/negotiated seed state anywhere — an
  agent and `palimpsestd` agree on Phi purely because they both hold the
  same `tenant_key` and compute the same epoch index. Change the key and
  every seed downstream of it changes.
- **Epochs are independent coordinate systems** (`docs/SPEC.md`): an
  encoder opens each new epoch with a golden keyframe + full dict, and
  `palimpsestd` bootstraps that epoch's dictionary/predictor state from
  scratch. This is the natural, already-existing seam to rotate on.

**Rotate at an epoch boundary, not mid-epoch.** `epoch_rotate` (collector
config, default 1h) already forces a new epoch — and therefore a fresh
seed derivation — on a schedule. To rotate `tenant_key`:

1. Generate the new key (same requirements as the old one: high-entropy,
   held only in a secret store your agents and `palimpsestd` can both
   read from — never committed, never logged).
2. Update the secret backing `tenant_key_env` for **every agent in the
   tenant and for `palimpsestd`'s `--tenant-key-env`**, and restart/reload
   them so the new value takes effect starting at each process's *next*
   epoch rotation. Because seed derivation includes `epoch_index`, an
   agent that picks up the new key mid-epoch and one still on the old key
   simply produce frames for what `palimpsestd` sees as two different
   coordinate systems until both cross into the same new epoch — not
   silent data corruption, but also not usable recovery in the meantime
   (see the next point).
3. **A key mismatch during the transition surfaces as DEGRADED, not a
   security failure**: if an agent's frames stop matching
   `palimpsestd`'s expected `dict_root`/Phi for that epoch (e.g. it
   rotated early, or `palimpsestd` didn't pick up the new key in time),
   that shard's view is marked DEGRADED (gated, metered, no anomalies
   emitted) until a golden keyframe under the *matching* key heals it —
   see `internal/core/engine.go`'s `handleKeyframe`. Confirm
   `degraded_shards` (expvar) returns to 0 shortly after a rotation
   window closes; a sustained nonzero value means some agent didn't
   actually pick up the new key.
4. Once every agent and `palimpsestd` for that tenant are confirmed on the
   new key (no `dict_root` mismatches, `degraded_shards` back to 0), the
   old key can be destroyed. Keep both readable for at least one full
   `epoch_rotate` interval past step 2 so in-flight/late frames from the
   old epoch aren't orphaned mid-rotation.

**What this repo does not provide today**: there is no automated rotation
tool, no dual-key overlap support in the wire format itself (a given
frame's seed is derived from exactly one key), and no re-encryption of
already-written keyframe/snapshot data at rest (rotation changes future
seed derivation, not past plaintext — see "compressed sensing is not
encryption" above). Treat the steps above as an operational runbook, not a
promise that this repo automates them for you.
