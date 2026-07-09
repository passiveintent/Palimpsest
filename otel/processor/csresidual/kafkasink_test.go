/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package csresidual

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// TestKafkaFrameSink_WriteFrameDeliversOverRealClient covers the wiring
// this file adds on top of internal/adapters/kafka (which has its own,
// more thorough unit tests for Sink/SpoolingSink themselves): a
// kafkaFrameSink built from KafkaOutputConfig actually publishes a
// wire.Frame that decodes back correctly on the other end.
func TestKafkaFrameSink_WriteFrameDeliversOverRealClient(t *testing.T) {
	const topic = "csresidual-frames"
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, topic))
	if err != nil {
		t.Fatalf("kfake.NewCluster: %v", err)
	}
	defer cluster.Close()

	sink, err := newKafkaFrameSink(KafkaOutputConfig{
		Brokers:       cluster.ListenAddrs(),
		Topic:         topic,
		SpoolDir:      t.TempDir(),
		SpoolMaxBytes: "10MB",
		DrainInterval: time.Second,
	}, "acme-corp")
	if err != nil {
		t.Fatalf("newKafkaFrameSink: %v", err)
	}
	defer sink.close()

	f := &wire.Frame{
		Magic: wire.Magic, Version: wire.Version, FrameType: wire.FrameTypeResidual,
		EmitterID: 1, ShardID: 2, Seq: 3, Bits: 8,
	}
	if err := sink.writeFrame(f); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	cl, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...), kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatalf("kgo.NewClient: %v", err)
	}
	defer cl.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fetches := cl.PollFetches(ctx)
	if errs := fetches.Errors(); len(errs) > 0 {
		t.Fatalf("fetch errors: %v", errs)
	}
	recs := fetches.Records()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	got, err := wire.Unmarshal(recs[0].Value)
	if err != nil {
		t.Fatalf("wire.Unmarshal: %v", err)
	}
	if got.ShardID != f.ShardID || got.Seq != f.Seq || got.EmitterID != f.EmitterID {
		t.Fatalf("decoded frame = %+v, want shard=%d seq=%d emitter=%d", got, f.ShardID, f.Seq, f.EmitterID)
	}
}

// TestKafkaFrameSink_OutageSpoolsToDisk confirms writeFrame durably spools
// rather than losing a frame when the broker is unreachable (the detailed
// spool/drain semantics themselves are internal/adapters/kafka's own unit
// tests; this only checks the wiring surfaces the same behavior here).
func TestKafkaFrameSink_OutageSpoolsToDisk(t *testing.T) {
	spoolDir := t.TempDir()
	sink, err := newKafkaFrameSink(KafkaOutputConfig{
		Brokers:       []string{"127.0.0.1:1"}, // nothing listening: every publish fails
		Topic:         "unreachable-topic",
		SpoolDir:      spoolDir,
		SpoolMaxBytes: "10MB",
		DrainInterval: time.Second,
	}, "acme-corp")
	if err != nil {
		t.Fatalf("newKafkaFrameSink: %v", err)
	}
	defer sink.close()

	f := &wire.Frame{Magic: wire.Magic, Version: wire.Version, FrameType: wire.FrameTypeResidual, ShardID: 1, Seq: 1, Bits: 8}
	if err := sink.writeFrame(f); err != nil {
		t.Fatalf("writeFrame during simulated outage: %v (should have spooled, not errored)", err)
	}

	entries, err := os.ReadDir(spoolDir)
	if err != nil {
		t.Fatalf("reading spool dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("spool dir has %d files, want 1", len(entries))
	}
}
