//go:build integration

/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

// These tests require a real, running Kafka broker (docker-compose, KRaft,
// single node — see demo/docker-compose.yml's "kafka" profile and
// demo/README.md's Kafka section) and are excluded from a normal `go test
// ./...` by the "integration" build tag (ACCEPTANCE: "unit green in CI;
// integration green locally"). Run them with:
//
//	docker compose -f ../../../demo/docker-compose.yml --profile kafka up -d kafka
//	go test -tags integration ./internal/adapters/kafka/... -run Integration -v
//	docker compose -f ../../../demo/docker-compose.yml --profile kafka down
//
// They stand in for "e2e plsim -> kafka -> palimpsestd": kafkaTestEncoder
// (internal/adapters/kafka/engine_redelivery_test.go) mirrors plsim's own
// encode pipeline, and a real core.Engine here mirrors palimpsestd's decode
// path, exactly as internal/core/e2e_test.go's in-process tests do for the
// non-Kafka transports — only the transport in between is real here.
package kafka

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/passiveintent/Palimpsest/internal/core"
	"github.com/passiveintent/Palimpsest/internal/forensics"
	"github.com/passiveintent/Palimpsest/internal/metrics"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// integrationBrokers returns the seed broker address(es) for these tests,
// overridable via KAFKA_INTEGRATION_BROKERS.
func integrationBrokers() []string {
	if v := os.Getenv("KAFKA_INTEGRATION_BROKERS"); v != "" {
		return strings.Split(v, ",")
	}
	return []string{"localhost:9094"}
}

// newIntegrationTopic creates a fresh, uniquely-named topic on the real
// broker (so tests don't interfere with each other or a prior run) and
// registers its deletion via t.Cleanup.
func newIntegrationTopic(t *testing.T, brokers []string, partitions int32) string {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatalf("kgo.NewClient(%v): %v", brokers, err)
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)

	topic := fmt.Sprintf("palimpsest-integration-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := adm.CreateTopic(ctx, partitions, 1, nil, topic); err != nil {
		t.Fatalf("creating topic %q on %v (is `docker compose --profile kafka up -d kafka` running? see demo/README.md): %v", topic, brokers, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = adm.DeleteTopics(ctx, topic)
	})
	return topic
}

func TestIntegration_OrderingPerPartition(t *testing.T) {
	brokers := integrationBrokers()
	topic := newIntegrationTopic(t, brokers, 8)

	sink, err := NewSink(SinkConfig{Brokers: brokers, Topic: topic, TenantID: "integration-tenant"})
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	defer sink.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const shardA, shardB = uint64(1), uint64(2)
	const nPerShard = 15
	for seq := uint32(0); seq < nPerShard; seq++ {
		if err := sink.Send(ctx, testFrame(shardA, seq, 100)); err != nil {
			t.Fatalf("Send shardA seq=%d: %v", seq, err)
		}
		if err := sink.Send(ctx, testFrame(shardB, seq, 200)); err != nil {
			t.Fatalf("Send shardB seq=%d: %v", seq, err)
		}
	}

	src, err := NewSource(SourceConfig{Brokers: brokers, Topic: topic, Group: fmt.Sprintf("g-%d", time.Now().UnixNano())})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	var mu sync.Mutex
	gotA, gotB := []uint32{}, []uint32{}
	runCtx, runCancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- src.Run(runCtx, func(f *wire.Frame) {
			mu.Lock()
			defer mu.Unlock()
			switch f.ShardID {
			case shardA:
				gotA = append(gotA, f.Seq)
			case shardB:
				gotB = append(gotB, f.Seq)
			}
		})
	}()

	deadline := time.Now().Add(20 * time.Second)
	for {
		mu.Lock()
		enough := len(gotA) >= nPerShard && len(gotB) >= nPerShard
		mu.Unlock()
		if enough || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	runCancel()
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	assertStrictlyIncreasing(t, "shard A", gotA, nPerShard)
	assertStrictlyIncreasing(t, "shard B", gotB, nPerShard)
}

func assertStrictlyIncreasing(t *testing.T, label string, got []uint32, want int) {
	t.Helper()
	if len(got) != want {
		t.Fatalf("%s: got %d frames, want %d", label, len(got), want)
	}
	for i, seq := range got {
		if seq != uint32(i) {
			t.Fatalf("%s: frame %d has seq %d, want %d (per-shard order must match send order)", label, i, seq, i)
		}
	}
}

func TestIntegration_RedeliveryDeduped(t *testing.T) {
	brokers := integrationBrokers()
	topic := newIntegrationTopic(t, brokers, 1)

	const (
		shardID         = uint64(501)
		emitterID       = uint64(50001)
		viewID          = uint16(0)
		m, d, bits      = 200, 6, 8
		allowedLateness = 2
	)
	tenantKey := []byte("kafka-integration-redelivery-key")

	sink := &fakeAnomalySink{}
	met := metrics.New()
	eng := core.New(core.Config{
		TenantKey: tenantKey, MaxResidual: 40.0, AllowedLateness: allowedLateness,
		RepairHorizon: 5 * time.Minute, EmittersExpected: 1,
		FISTAIters: 200, FISTALambda: 0.05, FISTAPowerIters: 30, FISTAThreshold: 0.3,
		DriftThreshold: 1e9,
	}, sink, nil, met, forensics.NewSnapshotStore(time.Hour, 100))

	names := make([]string, 30)
	for i := range names {
		names[i] = fmt.Sprintf("kafka_integration_metric_%02d|shard=501|agg=sum", i)
	}
	baseline := func() map[string]float64 {
		v := make(map[string]float64, len(names))
		for _, n := range names {
			v[n] = 10.0
		}
		return v
	}
	anomalous := func() map[string]float64 {
		v := baseline()
		v[names[0]] = 18.0
		return v
	}

	enc := newKafkaTestEncoder(tenantKey, shardID, emitterID, viewID, m, d, bits)
	now := time.Now()

	kSink, err := NewSink(SinkConfig{Brokers: brokers, Topic: topic, TenantID: "integration-tenant"})
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	defer kSink.Close()
	sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer sendCancel()
	send := func(f *wire.Frame) {
		t.Helper()
		if err := kSink.Send(sendCtx, f); err != nil {
			t.Fatalf("Send seq=%d: %v", f.Seq, err)
		}
	}

	send(enc.golden(0, baseline(), now))
	dupFrame := enc.residual(1, anomalous(), now)
	send(dupFrame)
	for s := uint32(2); s <= uint32(1+allowedLateness); s++ {
		send(enc.residual(s, baseline(), now))
	}

	src, err := NewSource(SourceConfig{Brokers: brokers, Topic: topic, Group: fmt.Sprintf("g-%d", time.Now().UnixNano())})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	runDone := make(chan error, 1)
	go func() { runDone <- src.Run(runCtx, func(f *wire.Frame) { eng.HandleFrame(context.Background(), f) }) }()

	countWindow1 := func() int {
		n := 0
		for _, ev := range sink.Events() {
			if ev.Substrate == "deviation" && ev.WindowID == 1 {
				n++
			}
		}
		return n
	}
	waitFor(t, "initial window-1 deviation event", func() bool { return countWindow1() > 0 })
	initialCount := countWindow1()

	send(dupFrame) // at-least-once redelivery, simulated as an upstream resend over the real broker
	waitFor(t, "duplicate_frames_dropped to increment", func() bool { return met.DuplicateFramesDropped() > 0 })

	runCancel()
	<-runDone

	if got := countWindow1(); got != initialCount {
		t.Fatalf("window-1 event count changed after redelivered duplicate: got %d, want %d", got, initialCount)
	}
	for _, ev := range sink.Events() {
		if ev.Substrate == "deviation" && ev.WindowID == 1 && ev.Revision > 0 {
			t.Fatalf("redelivered duplicate produced a spurious revision>0 event: %+v", ev)
		}
	}
}

// TestIntegration_BrokerOutageSpoolDrainsAndRevisionCorrect is the
// broker-outage acceptance scenario: emitter B's contribution to window 1
// fails to publish (simulated broker outage — see the package doc on why an
// address swap stands in for `docker compose stop kafka` here: a real
// multi-minute container outage is exercised manually, per demo/README.md,
// not on every test run), spools to disk, and — once "reconnected" —
// drains to the real broker, where the real Source delivers it to the same
// Engine emitter A's contribution already produced an initial (coverage=1,
// revision=0) solve for. That late arrival must trigger a correct repair:
// coverage=2, revision>0 (TestE2E_WatermarkRepair's assertions, reused here
// over real transport with a real on-disk spool in the loop).
func TestIntegration_BrokerOutageSpoolDrainsAndRevisionCorrect(t *testing.T) {
	brokers := integrationBrokers()
	topic := newIntegrationTopic(t, brokers, 1)

	const (
		shardID         = uint64(502)
		emitterA        = uint64(50101)
		emitterB        = uint64(50102)
		viewID          = uint16(0)
		m, d, bits      = 150, 6, 8
		allowedLateness = 2
	)
	tenantKey := []byte("kafka-integration-outage-key")

	sink := &fakeAnomalySink{}
	met := metrics.New()
	eng := core.New(core.Config{
		TenantKey: tenantKey, MaxResidual: 40.0, AllowedLateness: allowedLateness,
		RepairHorizon: 5 * time.Minute, EmittersExpected: 2,
		FISTAIters: 200, FISTALambda: 0.05, FISTAPowerIters: 30, FISTAThreshold: 0.3,
		DriftThreshold: 1e9,
	}, sink, nil, met, forensics.NewSnapshotStore(time.Hour, 100))

	names := make([]string, 20)
	for i := range names {
		names[i] = fmt.Sprintf("kafka_outage_metric_%02d|shard=502|agg=sum", i)
	}
	baseline := func() map[string]float64 {
		v := make(map[string]float64, len(names))
		for _, n := range names {
			v[n] = 10.0
		}
		return v
	}
	anomalous := func() map[string]float64 {
		v := baseline()
		v[names[0]] = 18.0
		return v
	}

	now := time.Now()
	encA := newKafkaTestEncoder(tenantKey, shardID, emitterA, viewID, m, d, bits)
	encB := newKafkaTestEncoder(tenantKey, shardID, emitterB, viewID, m, d, bits)

	realSinkA, err := NewSink(SinkConfig{Brokers: brokers, Topic: topic, TenantID: "integration-tenant"})
	if err != nil {
		t.Fatalf("NewSink (A): %v", err)
	}
	defer realSinkA.Close()

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer sendCancel()

	// Start the real Source consuming before anything is produced, so it
	// picks up frames as they arrive rather than requiring a separate
	// catch-up pass.
	src, err := NewSource(SourceConfig{Brokers: brokers, Topic: topic, Group: fmt.Sprintf("g-%d", time.Now().UnixNano())})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	runDone := make(chan error, 1)
	go func() { runDone <- src.Run(runCtx, func(f *wire.Frame) { eng.HandleFrame(context.Background(), f) }) }()

	// Window 0: both emitters report (warm-up golden keyframes), both up.
	if err := realSinkA.Send(sendCtx, encA.golden(0, baseline(), now)); err != nil {
		t.Fatalf("A golden: %v", err)
	}
	if err := realSinkA.Send(sendCtx, encB.golden(0, baseline(), now)); err != nil {
		t.Fatalf("B golden: %v", err)
	}

	// Window 1: A reports anomalous (up, direct). B's frame for window 1
	// fails to publish — simulated outage, an unreachable producer — and
	// must spool rather than being lost.
	frameB1 := encB.residual(1, anomalous(), now)
	spoolDir := t.TempDir()
	downProducer, err := NewSink(SinkConfig{Brokers: []string{"127.0.0.1:1"}, Topic: topic, TenantID: "integration-tenant"})
	if err != nil {
		t.Fatalf("NewSink (down): %v", err)
	}
	spoolB, err := NewSpoolingSink(downProducer, spoolDir, 10<<20)
	if err != nil {
		t.Fatalf("NewSpoolingSink: %v", err)
	}
	outageCtx, outageCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := spoolB.Send(outageCtx, frameB1); err != nil {
		outageCancel()
		t.Fatalf("Send during simulated outage: %v (should have spooled successfully)", err)
	}
	outageCancel()
	if entries, _ := spoolFiles(spoolDir); len(entries) != 1 {
		t.Fatalf("spool dir has %d files during simulated outage, want 1", len(entries))
	}

	if err := realSinkA.Send(sendCtx, encA.residual(1, anomalous(), now)); err != nil {
		t.Fatalf("A residual seq=1: %v", err)
	}
	for s := uint32(2); s <= uint32(1+allowedLateness); s++ {
		if err := realSinkA.Send(sendCtx, encA.residual(s, baseline(), now)); err != nil {
			t.Fatalf("A residual seq=%d: %v", s, err)
		}
	}

	countWindow1 := func() []int {
		var revisions []int
		for _, ev := range sink.Events() {
			if ev.Substrate == "deviation" && ev.WindowID == 1 {
				revisions = append(revisions, ev.Revision)
			}
		}
		return revisions
	}
	waitFor(t, "initial window-1 deviation event (coverage=1, A only)", func() bool { return len(countWindow1()) > 0 })
	for _, ev := range sink.Events() {
		if ev.Substrate == "deviation" && ev.WindowID == 1 && ev.Revision == 0 && ev.Coverage != 1 {
			t.Fatalf("initial solve coverage = %d, want 1 (only emitter A had reported)", ev.Coverage)
		}
	}

	// "Reconnect": a fresh producer pointed at the real broker, wrapping the
	// SAME spool dir, drains the backlog B accumulated during the outage.
	upProducer, err := NewSink(SinkConfig{Brokers: brokers, Topic: topic, TenantID: "integration-tenant"})
	if err != nil {
		t.Fatalf("NewSink (up): %v", err)
	}
	defer upProducer.Close()
	spoolB2, err := NewSpoolingSink(upProducer, spoolDir, 10<<20)
	if err != nil {
		t.Fatalf("NewSpoolingSink (resumed): %v", err)
	}
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := spoolB2.DrainOnce(drainCtx); err != nil {
		drainCancel()
		t.Fatalf("DrainOnce after reconnect: %v", err)
	}
	drainCancel()

	waitFor(t, "revised (revision>0) window-1 event after B's spooled frame drains", func() bool {
		for _, rev := range countWindow1() {
			if rev > 0 {
				return true
			}
		}
		return false
	})
	for _, ev := range sink.Events() {
		if ev.Substrate == "deviation" && ev.WindowID == 1 && ev.Revision > 0 && ev.Coverage != 2 {
			t.Fatalf("revised solve coverage = %d, want 2 (both emitters now counted)", ev.Coverage)
		}
	}

	runCancel()
	<-runDone
}

func waitFor(t *testing.T, desc string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", desc)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
