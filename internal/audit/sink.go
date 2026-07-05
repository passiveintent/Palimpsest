/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package audit

import (
	"context"

	"github.com/purushpsm147/palimpsest/internal/ports"
)

// Sink wraps a ports.AnomalySink, feeding every emitted event to an
// Auditor in addition to forwarding it to Next — mirroring
// cmd/palimpsestd's existing fanoutAnomalySink (webhook) pattern, so
// --truth supplements rather than replaces whatever sink(s) are already
// configured.
type Sink struct {
	Next    ports.AnomalySink
	Auditor *Auditor
}

// EmitAnomaly forwards ev to Next and records it with Auditor.Observe
// regardless of Next's outcome (an audit sample should not depend on a
// downstream sink's success), returning Next's error.
func (s Sink) EmitAnomaly(ctx context.Context, ev ports.AnomalyEvent) error {
	s.Auditor.Observe(ev)
	return s.Next.EmitAnomaly(ctx, ev)
}
