/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package fswatch

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/purushpsm147/palimpsest/pkg/wire"
)

func writeFrameFile(t *testing.T, dir, name string, f *wire.Frame) {
	t.Helper()
	b, err := wire.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func testFrame(seq uint32) *wire.Frame {
	return &wire.Frame{
		Magic: wire.Magic, Version: wire.Version, FrameType: wire.FrameTypeResidual,
		EmitterID: 1, ShardID: 2, Epoch: 0, Seq: seq, M: 4, D: 2, Bits: 8,
		QuantScale: 1.0, Payload: []byte{0, 0, 0, 0},
	}
}

func TestSource_PicksUpAndMovesFrames(t *testing.T) {
	dir := t.TempDir()
	writeFrameFile(t, dir, "a.plmp", testFrame(1))
	writeFrameFile(t, dir, "b.plmp", testFrame(2))
	if err := os.WriteFile(filepath.Join(dir, "not-a-frame.txt"), []byte("ignored: wrong extension"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	src := New(dir, 10*time.Millisecond)

	var mu sync.Mutex
	var seqs []uint32
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- src.Run(ctx, func(f *wire.Frame) {
			mu.Lock()
			seqs = append(seqs, f.Seq)
			mu.Unlock()
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(seqs)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for frames to be handled, got %d", n)
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, name := range []string{"a.plmp", "b.plmp"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s: expected moved out of %s, stat err = %v", name, dir, err)
		}
		if _, err := os.Stat(filepath.Join(dir, processedSubdir, name)); err != nil {
			t.Fatalf("%s: expected present under %s: %v", name, processedSubdir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "not-a-frame.txt")); err != nil {
		t.Fatalf("non-.plmp file should be left alone: %v", err)
	}
}

func TestSource_MovesInvalidFrameAside(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.plmp"), []byte("not a real frame"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var errs []string
	var mu sync.Mutex
	src := New(dir, time.Hour)
	src.OnError = func(path string, err error) {
		mu.Lock()
		errs = append(errs, path)
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- src.Run(ctx, func(*wire.Frame) {}) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, processedSubdir, "bad.plmp"+invalidSuffix)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for invalid frame to be moved aside")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(errs) == 0 {
		t.Fatalf("expected OnError to be called for the malformed frame")
	}
}
