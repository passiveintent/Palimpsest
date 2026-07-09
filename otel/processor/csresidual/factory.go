/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package csresidual implements the Palimpsest "csresidual" OpenTelemetry
// Collector metrics processor: it sketches high-cardinality metric
// residuals into small linear measurements (compressed sensing) rather
// than shipping every series to a per-series-billed backend.
//
// See README.md for OCB (OpenTelemetry Collector Builder) build
// instructions and an annotated example config, and
// docs/adr/ADR-005/007/008/009/010/011/012/013 in the repo root for the
// design decisions this package implements. Collector deps are confined
// to this module (ADR-007): pkg/* (the core sketch/wire/predict math)
// stays stdlib + xxhash only.
/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package csresidual

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"
)

// componentType is this processor's registered component.Type, i.e. the
// `csresidual:` key under a collector config's `processors:` block.
var componentType = component.MustNewType("csresidual")

// NewFactory returns the csresidual processor.Factory. An OTel Collector
// build (see README.md) registers it so `csresidual:` becomes usable in a
// pipeline's `processors:` list. Only metrics pipelines are supported;
// CreateTraces/CreateLogs are left at their default
// pipeline.ErrSignalNotSupported behavior.
func NewFactory() processor.Factory {
	return processor.NewFactory(
		componentType,
		createDefaultConfig,
		processor.WithMetrics(createMetricsProcessor, component.StabilityLevelDevelopment),
	)
}

// createDefaultConfig returns the defaults documented in README.md.
// TenantID, TenantKeyEnv, ShardBy, LogicalKey, InstanceKey, and Views have
// no sane default (ADR-008/ADR-012 require the operator to declare their
// own identity/tenancy) and are left zero; Config.Validate rejects a
// config that leaves the required ones unset.
func createDefaultConfig() component.Config {
	return &Config{
		Aggregates:            []string{"sum", "count", "max"},
		M:                     2000,
		D:                     6,
		Bits:                  8,
		FlushInterval:         10 * time.Second,
		KeyframeEvery:         6,
		KeyframeFullDictEvery: 10,
		GoldenKeyframeEvery:   10,
		EpochRotate:           time.Hour,
		EpochJitterWindow:     60 * time.Second,
		SeriesTTL:             90 * time.Second,
		Compression:           CompressionConfig{Codec: "gzip"},
		Storm: StormConfig{
			EnergyMultiplier: 25,
			FallbackTopK:     100,
		},
		Snapshot: SnapshotConfig{
			Enabled:           true,
			ResidualThreshold: 2.0,
			MaxSeriesPerFlush: 100,
			BlobTTL:           time.Hour,
		},
		RingBuffer: RingBufferConfig{
			Window:                 15 * time.Minute,
			MaxInstancesPerLogical: 1000,
			MaxTotalBytes:          "100MB",
		},
		Churn: ChurnConfig{
			MaxBirthsPerLogicalPerMin: 100,
		},
		Pull: PullConfig{
			Enabled:        false,
			Addr:           ":8889",
			MTLS:           true,
			BearerTokenEnv: "PALIMPSEST_PULL_TOKEN",
		},
		DrilldownHint: true,
		Tiers: []TierConfig{
			{Match: `^billing_.*`, Tier: tierExact},
			{Match: `.*`, Tier: tierSketched},
		},
		Shadow: false,
		Output: OutputConfig{
			Type: outputTypeFile,
			Dir:  "/var/lib/palimpsest/frames",
			Kafka: KafkaOutputConfig{
				SpoolDir:      "/var/lib/palimpsest/kafka-spool",
				SpoolMaxBytes: "1GB",
				DrainInterval: 10 * time.Second,
			},
		},
	}
}

// createMetricsProcessor builds the internal metricsProcessor and wraps it
// as a processor.Metrics via processorhelper.NewMetrics, which handles
// span/obsreport bookkeeping and calling next once consumeMetrics returns.
func createMetricsProcessor(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Metrics) (processor.Metrics, error) {
	pCfg, ok := cfg.(*Config)
	if !ok {
		return nil, fmt.Errorf("csresidual: invalid config type %T", cfg)
	}

	mp, err := newMetricsProcessor(pCfg, set.TelemetrySettings.Logger, nil)
	if err != nil {
		return nil, err
	}

	return processorhelper.NewMetrics(
		ctx,
		set,
		cfg,
		next,
		mp.consumeMetrics,
		processorhelper.WithStart(mp.start),
		processorhelper.WithShutdown(mp.shutdown),
	)
}
