/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package csresidual

import (
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// snapshotEntryWireBytes approximates one wire.SnapshotEntry's encoded
// size (id u64 + ts_ms u64 + value f32, docs/SPEC.md snapshot_blob format)
// for instanceBuffers' approximate global byte budget.
const snapshotEntryWireBytes = 8 + 8 + 4

// instanceEntry is one instance's ring buffer within a logical group.
type instanceEntry struct {
	rb *sketch.RingBuffer
}

// instanceBuffers is one pipeline's agent-side instance ring buffer
// (ADR-008: "pod/instance labels live in an agent-side ring buffer"),
// keyed first by pre-aggregate logical group then by instance key. It
// bounds memory two ways (guardrail: "Ring buffer bounded memory (global +
// per-series cap)"): each sketch.RingBuffer already self-trims to its
// construction-time window on every Push, and instanceBuffers additionally
// caps the number of distinct instances tracked per group
// (max_instances_per_logical) and an approximate global byte budget
// (max_total_bytes) reconciled on every drain() housekeeping pass, since
// sketch.RingBuffer does not itself expose a sample count to query.
//
// instanceBuffers is safe for concurrent use: the flush/consume path and
// the optional pull HTTP handler both reach it.
type instanceBuffers struct {
	mu            sync.Mutex
	window        time.Duration
	maxPerGroup   int
	maxTotalBytes int64
	approxBytes   int64
	groups        map[string]map[string]*instanceEntry
}

func newInstanceBuffers(window time.Duration, maxPerGroup int, maxTotalBytes int64) *instanceBuffers {
	return &instanceBuffers{
		window:        window,
		maxPerGroup:   maxPerGroup,
		maxTotalBytes: maxTotalBytes,
		groups:        make(map[string]map[string]*instanceEntry),
	}
}

// push records one instance-level sample. If instanceKey is new to
// groupKey and admitting it would exceed either bound, the sample is
// silently dropped from the ring buffer (it still contributed to
// aggregate.go's fold, which is not gated by ring-buffer capacity) rather
// than growing memory without bound.
func (b *instanceBuffers) push(groupKey, instanceKey string, id uint64, tsMs uint64, value float64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	instances, ok := b.groups[groupKey]
	if !ok {
		instances = make(map[string]*instanceEntry)
		b.groups[groupKey] = instances
	}
	entry, ok := instances[instanceKey]
	if !ok {
		if len(instances) >= b.maxPerGroup {
			return
		}
		if b.maxTotalBytes > 0 && b.approxBytes >= b.maxTotalBytes {
			return
		}
		entry = &instanceEntry{rb: sketch.NewRingBuffer(id, b.window)}
		instances[instanceKey] = entry
	}
	entry.rb.Push(tsMs, value)
	b.approxBytes += snapshotEntryWireBytes
}

// snapshotGroup decodes and merges every instance's current ring-buffer
// content under groupKey into one entry list. snapshot.go attaches these,
// across possibly several flagged groups, to a single frame.snapshot_blob.
func (b *instanceBuffers) snapshotGroup(groupKey string) []wire.SnapshotEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshotGroupLocked(groupKey)
}

func (b *instanceBuffers) snapshotGroupLocked(groupKey string) []wire.SnapshotEntry {
	instances, ok := b.groups[groupKey]
	if !ok {
		return nil
	}
	var out []wire.SnapshotEntry
	for _, entry := range instances {
		decoded, err := wire.DecodeSnapshot(entry.rb.Snapshot(), wire.CodecNone)
		if err != nil {
			// entry.rb.Snapshot() only ever produces bytes this same
			// wire package understands; a decode failure here would mean
			// the wire package's own round-trip broke.
			continue
		}
		out = append(out, decoded...)
	}
	return out
}

// drain runs periodic housekeeping: expires samples older than ttl in
// every tracked ring buffer (ADR-009's ring-buffer TTL, independent of
// each buffer's own construction-time window), garbage-collects instances
// that end up empty (e.g. a killed pod whose samples have all aged out),
// and recomputes the approximate global byte budget from what's left so
// expired data does not hang in memory (guardrail).
func (b *instanceBuffers) drain(ttl time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var total int64
	for groupKey, instances := range b.groups {
		for instanceKey, entry := range instances {
			entry.rb.Drain(ttl)
			decoded, err := wire.DecodeSnapshot(entry.rb.Snapshot(), wire.CodecNone)
			if err != nil || len(decoded) == 0 {
				delete(instances, instanceKey)
				continue
			}
			total += int64(len(decoded)) * snapshotEntryWireBytes
		}
		if len(instances) == 0 {
			delete(b.groups, groupKey)
		}
	}
	b.approxBytes = total
}

// instanceCount reports how many distinct instances are currently tracked
// under groupKey (test/observability helper).
func (b *instanceBuffers) instanceCount(groupKey string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.groups[groupKey])
}

// pullEntry is the JSON shape returned by the pull endpoint (ADR-012:
// "GET /v1/instances?logical=<name>&since=<unix_ms> -> JSON list").
type pullEntry struct {
	ID    uint64  `json:"id"`
	TSMs  uint64  `json:"ts_ms"`
	Value float32 `json:"value"`
}

// newPullServer builds the optional ADR-012 pull endpoint: mTLS + bearer
// token only, zero listening sockets unless explicitly enabled
// (Config.Pull.Enabled defaults false, and Validate rejects Enabled
// without both MTLS and a bearer token). lookup resolves a `logical` query
// param (a pre-aggregate group key) to whatever instance samples are
// currently buffered for it, across every pipeline (shard x view) this
// processor runs; newPullServer itself applies the `since` filter.
func newPullServer(cfg PullConfig, lookup func(logical string) []wire.SnapshotEntry) (*http.Server, error) {
	token := os.Getenv(cfg.BearerTokenEnv)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/instances", func(w http.ResponseWriter, r *http.Request) {
		if !bearerAuthorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		logical := r.URL.Query().Get("logical")
		if logical == "" {
			http.Error(w, `missing required query param "logical"`, http.StatusBadRequest)
			return
		}
		var sinceMs uint64
		if s := r.URL.Query().Get("since"); s != "" {
			v, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				http.Error(w, `invalid "since" (want unix millis)`, http.StatusBadRequest)
				return
			}
			sinceMs = v
		}

		entries := lookup(logical)
		out := make([]pullEntry, 0, len(entries))
		for _, e := range entries {
			if e.TSMs < sinceMs {
				continue
			}
			out = append(out, pullEntry{ID: e.ID, TSMs: e.TSMs, Value: e.Value})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	if cfg.MTLS {
		caPEM, err := os.ReadFile(cfg.TLSClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("csresidual: reading pull.tls_client_ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("csresidual: pull.tls_client_ca_file contains no usable certificates")
		}
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("csresidual: loading pull TLS server cert/key: %w", err)
		}
		srv.TLSConfig = &tls.Config{
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	return srv, nil
}

// bearerAuthorized reports whether r carries "Authorization: Bearer
// <token>" matching want, in constant time.
func bearerAuthorized(r *http.Request, want string) bool {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) <= len(prefix) || auth[:len(prefix)] != prefix {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(auth[len(prefix):]), []byte(want)) == 1
}
