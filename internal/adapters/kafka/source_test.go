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
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/passiveintent/Palimpsest/pkg/wire"
)

func TestSource_ConsumesAndDecodesInOrder(t *testing.T) {
	brokers := newFakeCluster(t, 4)

	sink, err := NewSink(SinkConfig{Brokers: brokers, Topic: testTopic, TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	defer sink.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const n = 25
	for seq := uint32(0); seq < n; seq++ {
		// One shard (42) so every frame is guaranteed the same partition;
		// this test is about intra-partition ordering, not cross-partition
		// fan-in, which TestSink_SameShardSamePartition already covers.
		if err := sink.Send(ctx, testFrame(42, seq, 7)); err != nil {
			t.Fatalf("Send seq=%d: %v", seq, err)
		}
	}

	src, err := NewSource(SourceConfig{Brokers: brokers, Topic: testTopic, Group: "g1"})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}

	var mu sync.Mutex
	var got []uint32
	runCtx, runCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- src.Run(runCtx, func(f *wire.Frame) {
			mu.Lock()
			got = append(got, f.Seq)
			mu.Unlock()
		})
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		count := len(got)
		mu.Unlock()
		if count >= n || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	runCancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != n {
		t.Fatalf("got %d frames, want %d", len(got), n)
	}
	for i, seq := range got {
		if seq != uint32(i) {
			t.Fatalf("frame %d has seq %d, want %d (per-shard order must match send order)", i, seq, i)
		}
	}
}

// TestSource_AtLeastOnceRedelivery covers the Delivery-semantics acceptance
// requirement directly: a consumer that crashes (or is simply restarted)
// before committing offsets for a fetch it already handed to handle() must
// see those same records again on the next Run — Source must never commit
// ahead of processing. This is the transport-level half of ADR-013's
// "at-least-once transport + dedupe ledger absorbs it" contract; the ledger
// half is TestE2E_DuplicateResidualFrameDeduped in internal/core.
func TestSource_AtLeastOnceRedelivery(t *testing.T) {
	brokers := newFakeCluster(t, 1) // single partition: simplest to reason about redelivery of the exact same records
	sink, err := NewSink(SinkConfig{Brokers: brokers, Topic: testTopic, TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	defer sink.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for seq := uint32(0); seq < 3; seq++ {
		if err := sink.Send(ctx, testFrame(1, seq, 5)); err != nil {
			t.Fatalf("Send seq=%d: %v", seq, err)
		}
	}

	const group = "crash-group"

	// Simulate a crash: consume with a raw consumer-group client (same
	// group Source will later use) but never call CommitOffsets before
	// closing, so nothing is ever acknowledged to the broker.
	firstPass := fetchWithoutCommitting(t, brokers, group, 3)
	if len(firstPass) != 3 {
		t.Fatalf("first pass: got %d frames, want 3", len(firstPass))
	}

	// A fresh Source in the SAME group, having never committed, must be
	// redelivered the identical records rather than resuming past them
	// (franz-go's default reset policy for a group with no committed
	// offset is AtStart, which is what makes this the correct default
	// rather than something Source has to configure itself).
	src, err := NewSource(SourceConfig{Brokers: brokers, Topic: testTopic, Group: group})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	var mu sync.Mutex
	var secondPass []uint32
	runCtx, runCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- src.Run(runCtx, func(f *wire.Frame) {
			mu.Lock()
			secondPass = append(secondPass, f.Seq)
			mu.Unlock()
		})
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		count := len(secondPass)
		mu.Unlock()
		if count >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	runCancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(secondPass) != 3 {
		t.Fatalf("second pass (post-crash redelivery): got %d frames, want 3 (at-least-once requires redelivery of uncommitted offsets)", len(secondPass))
	}
	for i := range firstPass {
		if firstPass[i] != secondPass[i] {
			t.Fatalf("redelivered frame %d seq=%d, want seq=%d (same records, same order)", i, secondPass[i], firstPass[i])
		}
	}
}

// fetchWithoutCommitting joins group with a plain consumer-group client
// (bypassing Source entirely), collects n frames, and closes without ever
// committing — modeling a crash between processing and acknowledgement.
func fetchWithoutCommitting(t *testing.T, brokers []string, group string, n int) []uint32 {
	t.Helper()
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(testTopic),
		kgo.DisableAutoCommit(),
	)
	if err != nil {
		t.Fatalf("kgo.NewClient: %v", err)
	}
	defer cl.Close() // no CommitOffsets call before this: nothing is ever acknowledged

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var got []uint32
	for len(got) < n {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			t.Fatalf("timed out collecting %d frames (got %d)", n, len(got))
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			t.Fatalf("fetch errors: %v", errs)
		}
		fetches.EachRecord(func(r *kgo.Record) {
			f, err := wire.Unmarshal(r.Value)
			if err != nil {
				t.Fatalf("wire.Unmarshal: %v", err)
			}
			got = append(got, f.Seq)
		})
	}
	return got
}

func TestSource_ContextCancelReturnsPromptly(t *testing.T) {
	brokers := newFakeCluster(t, 1)
	src, err := NewSource(SourceConfig{Brokers: brokers, Topic: testTopic, Group: "g-cancel"})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- src.Run(runCtx, func(*wire.Frame) {}) }()

	time.Sleep(100 * time.Millisecond) // let it settle into its poll loop
	start := time.Now()
	runCancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error on ctx cancel: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("Run took %s to return after ctx cancel, want promptly", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Run did not return within 5s of ctx cancellation")
	}
}
