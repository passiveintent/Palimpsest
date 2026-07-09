/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package kafka implements a ports.FrameSource (consumer group) and a
// ports.FrameSink (producer) over Apache Kafka, for deployments that want a
// durable transport between an agent (otel/processor/csresidual, or
// cmd/plsim for testing) and palimpsestd instead of the shared-filesystem
// fswatch convention.
//
// Library choice: github.com/twmb/franz-go over segmentio/kafka-go.
// franz-go is a pure-Go, actively maintained client with an idempotent
// producer enabled by default (the broker itself dedupes a producer's own
// retries, "exactly-once-adjacent" at the transport layer) and a
// transactions API available if a future revision needs it; its consumer
// group implementation supports manual offset commit, which Source needs to
// get an honest at-least-once contract (commit only after handle() has run
// for every record in a fetch). Neither library gives end-to-end
// exactly-once against palimpsestd's own state — that guarantee already
// exists one layer up, in the ADR-013 per-window per-emitter contributor
// ledger (internal/core.WindowState.Contributors), which this package
// relies on rather than re-implements: Kafka's job here is at-least-once
// delivery, not deduplication.
//
// Partitioning: Sink keys every record by tenant_id||shard_id (PartitionKey)
// so all frames for one shard land on one partition and are delivered in
// send order within it. Cross-partition (i.e. cross-shard) ordering is
// deliberately not preserved and does not need to be: ADR-013 keys all
// recovery state on (emitter_id, epoch, seq) carried inside the frame
// itself, never on transport arrival order. tenant_id has no wire
// representation (ADR-012: tenancy is a cryptographic boundary via the HKDF
// seed derivation, not a field on Frame) so it is supplied out-of-band by
// the caller — the tenant a given Sink/topic is scoped to.
package kafka

import "strconv"

// PartitionKey returns the Kafka record key for a frame belonging to
// tenantID and shardID: tenantID||shardID, so every frame for one shard
// (across every window and emitter) hashes to the same partition and is
// therefore delivered to a Source in send order. tenantID is caller-scoped
// (see package doc); it is never derived from the wire frame itself.
func PartitionKey(tenantID string, shardID uint64) []byte {
	return []byte(tenantID + "|" + strconv.FormatUint(shardID, 10))
}
