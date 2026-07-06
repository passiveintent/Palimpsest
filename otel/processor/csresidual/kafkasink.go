/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package csresidual

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/passiveintent/Palimpsest/internal/adapters/kafka"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// produceTimeout bounds a single publish (or drain) attempt against the
// broker. franz-go's producer retries a record effectively forever
// (RecordRetries defaults to MaxInt64) unless the context passed to
// ProduceSync carries its own deadline; without this bound, an outage would
// make writeFrame itself hang instead of falling through to
// SpoolingSink's spool-on-failure path, since flushPipeline calls writeFrame
// synchronously once per (shard, view) pipeline per flush.
const produceTimeout = 5 * time.Second

// kafkaFrameSink adapts internal/adapters/kafka (Sink, wrapped in a
// SpoolingSink for broker-outage resilience) to this processor's own
// frameSink interface. Depending on a root-module internal/ package from
// here is allowed — Go's internal-import rule is scoped by import-path
// tree, not module boundaries, and otel/... shares the
// github.com/passiveintent/Palimpsest/... prefix root/go.mod's internal/
// packages live under — and is exactly the "deps in otel module are
// acceptable" case: it pulls the Kafka client into this module's build
// graph, which ADR-007 only forbids for pkg/*, not for otel/.
type kafkaFrameSink struct {
	spool *kafka.SpoolingSink

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newKafkaFrameSink builds a kafkaFrameSink from a resolved KafkaOutputConfig
// and the processor's tenant_id (the tenant half of every record's
// partition key; see kafka.PartitionKey).
func newKafkaFrameSink(cfg KafkaOutputConfig, tenantID string) (*kafkaFrameSink, error) {
	producer, err := kafka.NewSink(kafka.SinkConfig{Brokers: cfg.Brokers, Topic: cfg.Topic, TenantID: tenantID})
	if err != nil {
		return nil, fmt.Errorf("csresidual: configuring output.kafka: %w", err)
	}
	maxBytes, err := parseByteSize(cfg.SpoolMaxBytes)
	if err != nil {
		// Unreachable given Config.Validate already checked this, but
		// newMetricsProcessor is also callable directly (processor_test.go
		// does), so this stays a real error rather than a panic.
		return nil, fmt.Errorf("csresidual: output.kafka.spool_max_bytes: %w", err)
	}
	spool, err := kafka.NewSpoolingSink(producer, cfg.SpoolDir, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("csresidual: configuring output.kafka.spool_dir: %w", err)
	}
	return &kafkaFrameSink{spool: spool}, nil
}

// writeFrame implements frameSink: publish, or durably spool on failure
// (see kafka.SpoolingSink).
func (k *kafkaFrameSink) writeFrame(f *wire.Frame) error {
	ctx, cancel := context.WithTimeout(context.Background(), produceTimeout)
	defer cancel()
	return k.spool.Send(ctx, f)
}

// startDrainLoop retries the spool backlog on a fixed tick, independent of
// any pipeline's own flush cadence, so a (shard, view) pipeline that goes
// quiet during an outage still has its backlog drained promptly once the
// broker reconnects ("drain on reconnect" — a quiet pipeline may otherwise
// never call writeFrame again to trigger that drain itself).
func (k *kafkaFrameSink) startDrainLoop(interval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	k.cancel = cancel
	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				drainCtx, cancel := context.WithTimeout(ctx, produceTimeout)
				_ = k.spool.DrainOnce(drainCtx)
				cancel()
			}
		}
	}()
}

// close stops the drain loop and closes the underlying producer (one final
// best-effort drain happens inside SpoolingSink.Close itself).
func (k *kafkaFrameSink) close() error {
	if k.cancel != nil {
		k.cancel()
	}
	k.wg.Wait()
	return k.spool.Close()
}
