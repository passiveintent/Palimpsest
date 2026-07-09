/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package zstdcodec

import (
	"bytes"
	"sync"
	"testing"

	"github.com/passiveintent/Palimpsest/pkg/wire"
)

func TestCompressorRoundTrip(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := [][]byte{
		nil,
		{},
		[]byte("hello"),
		bytes.Repeat([]byte("series.name=value|"), 5000),
	}
	for _, plain := range cases {
		compressed, err := c.Compress(plain)
		if err != nil {
			t.Fatalf("Compress(%d bytes): %v", len(plain), err)
		}
		got, err := c.Decompress(compressed, 1<<20)
		if err != nil {
			t.Fatalf("Decompress(%d bytes): %v", len(plain), err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("round trip mismatch: got %d bytes, want %d bytes", len(got), len(plain))
		}
	}
}

func TestCompressorDecompressBoundsMaxBytes(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plain := bytes.Repeat([]byte("x"), 1000)
	compressed, err := c.Compress(plain)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if _, err := c.Decompress(compressed, 10); err == nil {
		t.Fatal("Decompress: want error when decoded size exceeds maxBytes, got nil")
	}
}

// TestCompressorConcurrent exercises Compress/Decompress from many
// goroutines against one shared Compressor, matching the concurrency
// contract wire.Compressor documents (the registry is process-global).
func TestCompressorConcurrent(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const workers = 32
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			plain := bytes.Repeat([]byte{byte(i)}, 4096)
			compressed, err := c.Compress(plain)
			if err != nil {
				errCh <- err
				return
			}
			got, err := c.Decompress(compressed, 1<<20)
			if err != nil {
				errCh <- err
				return
			}
			if !bytes.Equal(got, plain) {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Errorf("concurrent Compress/Decompress: %v", err)
		}
	}
}

func TestRegister(t *testing.T) {
	if err := Register(); err != nil {
		t.Fatalf("Register: %v", err)
	}
	entries := []wire.SnapshotEntry{{ID: 1, TSMs: 1000, Value: 1.5}}
	blob, err := wire.EncodeSnapshot(entries, wire.CodecZstd)
	if err != nil {
		t.Fatalf("EncodeSnapshot with CodecZstd after Register: %v", err)
	}
	got, err := wire.DecodeSnapshot(blob, wire.CodecZstd)
	if err != nil {
		t.Fatalf("DecodeSnapshot with CodecZstd after Register: %v", err)
	}
	if len(got) != 1 || got[0] != entries[0] {
		t.Fatalf("DecodeSnapshot = %+v, want %+v", got, entries)
	}
}
