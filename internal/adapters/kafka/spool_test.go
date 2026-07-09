/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package kafka

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// fakeProducer is a Producer whose Send fails deterministically (rather than
// by racing a real broker outage) so spool/drain behavior can be tested
// without docker: see internal/adapters/kafka's integration test (build tag
// integration) for the real-broker-outage version of this scenario.
type fakeProducer struct {
	mu     sync.Mutex
	down   bool
	sent   []uint32 // Seq of every frame actually "delivered"
	closed bool
}

func (p *fakeProducer) setDown(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
}

func (p *fakeProducer) Send(_ context.Context, f *wire.Frame) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		return errors.New("fakeProducer: broker unavailable")
	}
	p.sent = append(p.sent, f.Seq)
	return nil
}

func (p *fakeProducer) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

func (p *fakeProducer) delivered() []uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]uint32(nil), p.sent...)
}

func TestSpoolingSink_PassthroughWhenUp(t *testing.T) {
	prod := &fakeProducer{}
	sink, err := NewSpoolingSink(prod, t.TempDir(), 1<<20)
	if err != nil {
		t.Fatalf("NewSpoolingSink: %v", err)
	}
	ctx := context.Background()
	for seq := uint32(0); seq < 3; seq++ {
		if err := sink.Send(ctx, testFrame(1, seq, 9)); err != nil {
			t.Fatalf("Send seq=%d: %v", seq, err)
		}
	}
	if got := prod.delivered(); len(got) != 3 {
		t.Fatalf("delivered = %v, want 3 frames passed straight through (no outage)", got)
	}
	if entries, _ := spoolFiles(sink.dir); len(entries) != 0 {
		t.Fatalf("spool dir has %d files after an up-broker run, want 0", len(entries))
	}
}

// TestSpoolingSink_OutageSpoolsThenDrainsInOrder covers the core acceptance
// scenario: frames sent during an outage are spooled instead of lost; once
// the producer recovers, the next Send drains the backlog first, in
// original order, before publishing anything new.
func TestSpoolingSink_OutageSpoolsThenDrainsInOrder(t *testing.T) {
	prod := &fakeProducer{}
	dir := t.TempDir()
	sink, err := NewSpoolingSink(prod, dir, 1<<20)
	if err != nil {
		t.Fatalf("NewSpoolingSink: %v", err)
	}
	ctx := context.Background()

	if err := sink.Send(ctx, testFrame(1, 0, 9)); err != nil {
		t.Fatalf("Send seq=0 (up): %v", err)
	}

	prod.setDown(true)
	for seq := uint32(1); seq <= 3; seq++ {
		// Send's contract is "delivered or durably spooled": a within-budget
		// spool is success (nil error), not a failure surfaced to the caller.
		if err := sink.Send(ctx, testFrame(1, seq, 9)); err != nil {
			t.Fatalf("Send seq=%d during outage: %v (should have spooled successfully)", seq, err)
		}
	}
	if entries, _ := spoolFiles(dir); len(entries) != 3 {
		t.Fatalf("spool dir has %d files during outage, want 3", len(entries))
	}
	if got := prod.delivered(); len(got) != 1 {
		t.Fatalf("delivered during outage = %v, want just seq=0 from before the outage", got)
	}

	prod.setDown(false)
	if err := sink.Send(ctx, testFrame(1, 4, 9)); err != nil {
		t.Fatalf("Send seq=4 (recovered): %v", err)
	}

	got := prod.delivered()
	want := []uint32{0, 1, 2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("delivered after recovery = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delivered[%d] = %d, want %d (backlog must drain in original order before the new frame)", i, got[i], want[i])
		}
	}
	if entries, _ := spoolFiles(dir); len(entries) != 0 {
		t.Fatalf("spool dir has %d files after a full drain, want 0", len(entries))
	}
}

func TestSpoolingSink_BoundedDiskDropsBeyondBudget(t *testing.T) {
	prod := &fakeProducer{}
	prod.setDown(true)
	dir := t.TempDir()

	f := testFrame(1, 0, 9)
	b, err := wire.Marshal(f)
	if err != nil {
		t.Fatalf("wire.Marshal: %v", err)
	}
	budget := int64(len(b)) + 1 // room for exactly one spooled frame

	sink, err := NewSpoolingSink(prod, dir, budget)
	if err != nil {
		t.Fatalf("NewSpoolingSink: %v", err)
	}
	ctx := context.Background()

	if err := sink.Send(ctx, f); err != nil {
		t.Fatalf("Send seq=0: %v (down, but within budget, should spool successfully)", err)
	}
	if entries, _ := spoolFiles(dir); len(entries) != 1 {
		t.Fatalf("spool dir has %d files after one within-budget frame, want 1", len(entries))
	}

	if err := sink.Send(ctx, testFrame(1, 1, 9)); err == nil {
		t.Fatalf("Send seq=1: want an error (over budget, frame must be dropped, not silently accepted)")
	}
	if entries, _ := spoolFiles(dir); len(entries) != 1 {
		t.Fatalf("spool dir has %d files after an over-budget send, want still 1 (the over-budget frame must not be written)", len(entries))
	}
}

// TestSpoolingSink_ResumesExistingBacklogOnRestart covers "drain on
// reconnect" surviving a process restart: a SpoolingSink constructed over a
// dir that already has spooled files (from a prior process that crashed
// mid-outage) must count them against its budget and drain them.
func TestSpoolingSink_ResumesExistingBacklogOnRestart(t *testing.T) {
	dir := t.TempDir()

	downProd := &fakeProducer{down: true}
	first, err := NewSpoolingSink(downProd, dir, 1<<20)
	if err != nil {
		t.Fatalf("NewSpoolingSink (first): %v", err)
	}
	ctx := context.Background()
	if err := first.Send(ctx, testFrame(1, 0, 9)); err != nil {
		t.Fatalf("Send during outage: %v (should have spooled successfully)", err)
	}
	if entries, _ := spoolFiles(dir); len(entries) != 1 {
		t.Fatalf("spool dir has %d files, want 1 before simulated restart", len(entries))
	}

	// Simulate a process restart: a brand new SpoolingSink over the same
	// dir, now with a healthy producer.
	upProd := &fakeProducer{}
	second, err := NewSpoolingSink(upProd, dir, 1<<20)
	if err != nil {
		t.Fatalf("NewSpoolingSink (second): %v", err)
	}
	if err := second.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if got := upProd.delivered(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("delivered after resumed drain = %v, want [0]", got)
	}
	if entries, _ := spoolFiles(dir); len(entries) != 0 {
		t.Fatalf("spool dir has %d files after resumed drain, want 0", len(entries))
	}
}

func TestSpoolingSink_CorruptSpoolFileIsSkippedNotWedged(t *testing.T) {
	prod := &fakeProducer{}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "00000000000000000001-corrupt.plmp.spool"), []byte("not a valid frame"), 0o644); err != nil {
		t.Fatalf("seeding corrupt spool file: %v", err)
	}

	sink, err := NewSpoolingSink(prod, dir, 1<<20)
	if err != nil {
		t.Fatalf("NewSpoolingSink: %v", err)
	}
	if err := sink.DrainOnce(context.Background()); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if entries, _ := spoolFiles(dir); len(entries) != 0 {
		t.Fatalf("spool dir has %d files after draining past a corrupt one, want 0 (corrupt files must be dropped, not retried forever)", len(entries))
	}
}
