# Security & Threat Model

Initial threat model per
[ADR-012](adr/ADR-012-security-tenancy.md). This is a pre-alpha first pass;
Prompt 8 of the build plan expands this file further (key rotation
guidance) once the wire codec and processor exist.

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

## Key rotation

Deferred to Prompt 8 (`docs/SECURITY.md` is explicitly revisited there,
alongside `demo/` NetworkPolicy examples). Not yet designed.
