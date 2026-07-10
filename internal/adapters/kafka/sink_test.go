/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package kafka

import (
	"context"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/passiveintent/Palimpsest/pkg/wire"
)

const testTopic = "palimpsest-frames"

// newFakeCluster returns an in-memory Kafka cluster (github.com/twmb/franz-go/pkg/kfake)
// with numPartitions partitions pre-created on testTopic, and its seed
// broker address. Used by every test in this package instead of a real
// broker (docker-compose is reserved for the build-tag=integration tests).
func newFakeCluster(t *testing.T, numPartitions int32) []string {
	t.Helper()
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(numPartitions, testTopic))
	if err != nil {
		t.Fatalf("kfake.NewCluster: %v", err)
	}
	t.Cleanup(cluster.Close)
	return cluster.ListenAddrs()
}

func testFrame(shardID uint64, seq uint32, emitterID uint64) *wire.Frame {
	return &wire.Frame{
		Magic:     wire.Magic,
		Version:   wire.Version,
		FrameType: wire.FrameTypeResidual,
		EmitterID: emitterID,
		ShardID:   shardID,
		Seq:       seq,
		Bits:      8,
	}
}

func TestSink_SendRoundTrips(t *testing.T) {
	brokers := newFakeCluster(t, 4)
	sink, err := NewSink(SinkConfig{Brokers: brokers, Topic: testTopic, TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	defer sink.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := testFrame(7, 3, 101)
	if err := sink.Send(ctx, f); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Read the raw record back with a plain client and confirm it decodes
	// to the same frame and carries the expected partition key.
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumeTopics(testTopic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatalf("kgo.NewClient: %v", err)
	}
	defer cl.Close()

	fetches := cl.PollFetches(ctx)
	if errs := fetches.Errors(); len(errs) > 0 {
		t.Fatalf("fetch errors: %v", errs)
	}
	recs := fetches.Records()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	rec := recs[0]
	if string(rec.Key) != string(PartitionKey("tenant-a", 7)) {
		t.Fatalf("record key = %q, want %q", rec.Key, PartitionKey("tenant-a", 7))
	}
	got, err := wire.Unmarshal(rec.Value)
	if err != nil {
		t.Fatalf("wire.Unmarshal: %v", err)
	}
	if got.ShardID != f.ShardID || got.Seq != f.Seq || got.EmitterID != f.EmitterID {
		t.Fatalf("decoded frame = %+v, want shard=%d seq=%d emitter=%d", got, f.ShardID, f.Seq, f.EmitterID)
	}
}

// TestSink_SameShardSamePartition covers the ADR-013 partitioning
// requirement: every frame for one (tenant, shard) — regardless of window
// or emitter — must land on the same partition, so a Source's per-partition
// delivery order is that shard's true send order.
func TestSink_SameShardSamePartition(t *testing.T) {
	brokers := newFakeCluster(t, 8)
	sink, err := NewSink(SinkConfig{Brokers: brokers, Topic: testTopic, TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	defer sink.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for seq := uint32(0); seq < 20; seq++ {
		if err := sink.Send(ctx, testFrame(42, seq, 999)); err != nil {
			t.Fatalf("Send seq=%d: %v", seq, err)
		}
	}

	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumeTopics(testTopic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatalf("kgo.NewClient: %v", err)
	}
	defer cl.Close()

	partitions := map[int32]bool{}
	seen := 0
	for seen < 20 {
		fetches := cl.PollFetches(ctx)
		if errs := fetches.Errors(); len(errs) > 0 {
			t.Fatalf("fetch errors: %v", errs)
		}
		fetches.EachRecord(func(r *kgo.Record) {
			partitions[r.Partition] = true
			seen++
		})
	}
	if len(partitions) != 1 {
		t.Fatalf("frames for one shard landed on %d distinct partitions, want 1: %v", len(partitions), partitions)
	}
}

// TestSink_DifferentTenantsDifferentKeys covers the tenant_id half of the
// partition key: two tenants sharing shard_id 1 must not silently collapse
// onto a key collision that could, on an unlucky hash, order-couple
// unrelated tenants' frames together (ADR-012: tenancy is a hard boundary).
func TestSink_DifferentTenantsDifferentKeys(t *testing.T) {
	if got, other := PartitionKey("tenant-a", 1), PartitionKey("tenant-b", 1); string(got) == string(other) {
		t.Fatalf("PartitionKey(%q,1) == PartitionKey(%q,1) == %q, want distinct keys", "tenant-a", "tenant-b", got)
	}
}
