/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package httpsrc

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/purushpsm147/palimpsest/pkg/wire"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestSource_ReceivesPostedFrame(t *testing.T) {
	addr := freeAddr(t)
	src := New(addr)

	var mu sync.Mutex
	var received []*wire.Frame
	var errs []error
	src.OnError = func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- src.Run(ctx, func(f *wire.Frame) {
			mu.Lock()
			received = append(received, f)
			mu.Unlock()
		})
	}()
	waitForServer(t, addr)

	frame := &wire.Frame{
		Magic: wire.Magic, Version: wire.Version, FrameType: wire.FrameTypeResidual,
		EmitterID: 1, ShardID: 2, Seq: 7, M: 4, D: 2, Bits: 8,
		QuantScale: 1.0, Payload: []byte{0, 0, 0, 0},
	}
	body, err := wire.Marshal(frame)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	resp, err := http.Post("http://"+addr+"/v1/frames", "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	// Malformed body -> 400, handle not called for it.
	resp2, err := http.Post("http://"+addr+"/v1/frames", "application/octet-stream", bytes.NewReader([]byte("garbage")))
	if err != nil {
		t.Fatalf("POST (garbage): %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("status (garbage) = %d, want 400", resp2.StatusCode)
	}

	// Wrong method -> 405.
	resp3, err := http.Get("http://" + addr + "/v1/frames")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status (GET) = %d, want 405", resp3.StatusCode)
	}

	resp4, err := http.Get("http://" + addr + "/debug/vars")
	if err != nil {
		t.Fatalf("GET /debug/vars: %v", err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("status (/debug/vars) = %d, want 200", resp4.StatusCode)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || received[0].Seq != 7 {
		t.Fatalf("received = %+v, want exactly one frame with Seq=7", received)
	}
	if len(errs) != 1 {
		t.Fatalf("OnError called %d times, want 1 (for the garbage POST)", len(errs))
	}
}

func waitForServer(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("server at %s never came up", addr)
}
