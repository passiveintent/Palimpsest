# ADR-NNN: <Title>

## Status

Proposed | Accepted | Superseded by ADR-NNN | Deprecated

## Context

What forces are at play (statistical, operational, or organizational)?
What alternatives were on the table? Reference the invariant(s) in
`.github/copilot-instructions.md` this decision touches, if any.

## Decision

What we are doing, stated as a concrete, falsifiable claim wherever possible.

## Consequences

What becomes easier or harder as a result. Note any invariant this decision
must not violate (e.g. SW drift stays on input distributions only; EVT-zone
violations remain unsuppressible; calibration data is never deleted).

## Falsification hooks

The test(s) that would prove this decision wrong, and where they live
(e.g. `tests/<module>/test_<name>.py::test_<case>`, or a `gate` marker in
`docs/GATES.md`). A decision with no falsification hook is not yet an ADR.
