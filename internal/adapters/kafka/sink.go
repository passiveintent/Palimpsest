/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package kafka

import (
	"context"
	"fmt"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// SinkConfig configures a Sink.
type SinkConfig struct {
	Brokers []string
	Topic   string

	// TenantID is prefixed onto every record's partition key (see
	// PartitionKey); it never rides on the wire (ADR-012).
	TenantID string

	// Opts, if non-nil, are appended after the required seed-broker/topic
	// options — a test seam (kfake's in-memory cluster needs no extra
	// options, but a caller wiring TLS/SASL for a real deployment does).
	Opts []kgo.Opt
}

// Sink implements ports.FrameSink: it publishes each wire.Frame, marshaled,
// to a Kafka topic (see the package doc for partitioning and delivery
// semantics).
type Sink struct {
	cl       *kgo.Client
	topic    string
	tenantID string
}

// NewSink returns a Sink connected to cfg.Brokers. It does not block waiting
// for the cluster to be reachable; the first Send will surface a connection
// error if the brokers are unavailable.
func NewSink(cfg SinkConfig) (*Sink, error) {
	if cfg.Topic == "" {
		return nil, fmt.Errorf("kafka: SinkConfig.Topic is required")
	}
	opts := append([]kgo.Opt{
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.DefaultProduceTopic(cfg.Topic),
	}, cfg.Opts...)
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("kafka: creating producer client: %w", err)
	}
	return &Sink{cl: cl, topic: cfg.Topic, tenantID: cfg.TenantID}, nil
}

// Send implements ports.FrameSink: it marshals f and publishes it,
// synchronously waiting for the broker's acknowledgement (or ctx's
// cancellation, or the client's configured retry budget to be exhausted).
func (s *Sink) Send(ctx context.Context, f *wire.Frame) error {
	b, err := wire.Marshal(f)
	if err != nil {
		return fmt.Errorf("kafka: marshaling frame: %w", err)
	}
	rec := &kgo.Record{
		Topic: s.topic,
		Key:   PartitionKey(s.tenantID, f.ShardID),
		Value: b,
	}
	if err := s.cl.ProduceSync(ctx, rec).FirstErr(); err != nil {
		return fmt.Errorf("kafka: producing to topic %q: %w", s.topic, err)
	}
	return nil
}

// Close implements ports.FrameSink: it flushes any buffered records and
// closes the underlying client.
func (s *Sink) Close() error {
	s.cl.Close()
	return nil
}
