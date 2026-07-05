/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package httpsrc implements a ports.FrameSource that receives ADR-012
// speculative pushes over HTTP: POST /v1/frames with a wire-encoded frame
// as the request body. This is the one adapter that opts into a listening
// socket (a FrameSource has to receive pushes somehow); every other
// palimpsestd component stays socket-free by default.
/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package httpsrc

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/purushpsm147/palimpsest/pkg/wire"
)

// maxFrameBytes bounds one request body so a malformed/hostile client can't
// force unbounded memory use decoding it.
const maxFrameBytes = 16 << 20

// Source serves POST Addr+"/v1/frames".
type Source struct {
	Addr string

	// OnError, if non-nil, is called for every per-request error (bad
	// method, oversized/undecodable body); it does not stop the server.
	OnError func(err error)
}

// New returns a Source listening on addr (e.g. ":8080").
func New(addr string) *Source {
	return &Source{Addr: addr}
}

// Run implements ports.FrameSource: it serves until ctx is canceled, then
// shuts the HTTP server down gracefully.
func (s *Source) Run(ctx context.Context, handle func(*wire.Frame)) error {
	mux := http.NewServeMux()
	// expvar's registry (internal/metrics.Publish populates it) rides
	// alongside frame ingestion on the same address: this is the one
	// listening socket ADR-012 accepts opting into, so it's also the
	// natural place for basic ops visibility.
	mux.Handle("/debug/vars", expvar.Handler())
	mux.HandleFunc("/v1/frames", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxFrameBytes+1))
		if err != nil {
			s.onError(fmt.Errorf("httpsrc: reading request body: %w", err))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(body) > maxFrameBytes {
			s.onError(fmt.Errorf("httpsrc: request body exceeds %d bytes", maxFrameBytes))
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		f, err := wire.Unmarshal(body)
		if err != nil {
			s.onError(fmt.Errorf("httpsrc: %w", err))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		handle(f)
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Addr: s.Addr, Handler: mux}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		<-errCh
		return nil
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func (s *Source) onError(err error) {
	if s.OnError != nil {
		s.OnError(err)
	}
}
