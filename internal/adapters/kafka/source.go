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
	"fmt"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// SourceConfig configures a Source.
type SourceConfig struct {
	Brokers []string
	Topic   string
	Group   string

	// OnError, if non-nil, is called for every per-record error (a decode
	// failure) and every fetch-level error; it does not stop Run.
	OnError func(err error)

	// Opts, if non-nil, are appended after the required
	// seed-broker/group/topic/offset-commit options — a test seam.
	Opts []kgo.Opt
}

// Source implements ports.FrameSource over a Kafka consumer group: Run polls
// the group's assigned partitions and, for every record, decodes it as a
// wire.Frame and invokes handle. Delivery is at-least-once: offsets are
// committed only after every record in a poll batch has been handed to
// handle, so a crash between delivery and commit is redelivered on restart
// rather than silently skipped (ADR-013's per-window contributor ledger
// absorbs the resulting duplicate — see the package doc).
type Source struct {
	cfg SourceConfig
	cl  *kgo.Client
}

// NewSource returns a Source that will join cfg.Group and consume cfg.Topic
// once Run is called.
func NewSource(cfg SourceConfig) (*Source, error) {
	if cfg.Topic == "" {
		return nil, fmt.Errorf("kafka: SourceConfig.Topic is required")
	}
	if cfg.Group == "" {
		return nil, fmt.Errorf("kafka: SourceConfig.Group is required")
	}
	opts := append([]kgo.Opt{
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ConsumerGroup(cfg.Group),
		kgo.ConsumeTopics(cfg.Topic),
		kgo.DisableAutoCommit(),
		kgo.RequireStableFetchOffsets(),
	}, cfg.Opts...)
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("kafka: creating consumer client: %w", err)
	}
	return &Source{cfg: cfg, cl: cl}, nil
}

// Run implements ports.FrameSource. It polls until ctx is canceled, then
// leaves the consumer group and returns nil.
func (s *Source) Run(ctx context.Context, handle func(*wire.Frame)) error {
	defer s.cl.Close()

	for {
		fetches := s.cl.PollFetches(ctx)
		if ctx.Err() != nil {
			return nil
		}

		fetches.EachError(func(topic string, partition int32, err error) {
			s.onError(fmt.Errorf("kafka: fetch error (topic=%s partition=%d): %w", topic, partition, err))
		})

		// EachPartition (rather than the flattened EachRecord) so that, per
		// partition, records are handled and only then committed in one
		// pass — preserving intra-partition (i.e. intra-shard, given
		// Sink's partitioning) order end to end.
		fetches.EachPartition(func(p kgo.FetchTopicPartition) {
			p.EachRecord(func(r *kgo.Record) {
				f, err := wire.Unmarshal(r.Value)
				if err != nil {
					s.onError(fmt.Errorf("kafka: decoding record (topic=%s partition=%d offset=%d): %w", r.Topic, r.Partition, r.Offset, err))
					return
				}
				handle(f)
			})
		})

		// Commit only after every record above was handed to handle: a
		// crash before this point redelivers the whole batch on restart
		// (at-least-once), never silently drops it.
		if err := s.cl.CommitUncommittedOffsets(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.onError(fmt.Errorf("kafka: committing offsets: %w", err))
		}
	}
}

func (s *Source) onError(err error) {
	if s.cfg.OnError != nil {
		s.cfg.OnError(err)
	}
}
