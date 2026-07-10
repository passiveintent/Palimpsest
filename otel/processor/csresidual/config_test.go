/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package csresidual

import (
	"strings"
	"testing"
	"time"
)

// validConfig returns a Config that passes Validate() outright; each
// TestConfigValidate_Fields case mutates exactly one field away from this
// baseline to isolate what triggers its expected error.
func validConfig(t *testing.T) *Config {
	t.Helper()
	t.Setenv("CSRESIDUAL_TEST_TENANT_KEY", "super-secret-tenant-key")
	return &Config{
		TenantID:              "acme-corp",
		TenantKeyEnv:          "CSRESIDUAL_TEST_TENANT_KEY",
		ShardBy:               []string{"service.name"},
		LogicalKey:            []string{"service.name", "k8s.deployment.name"},
		InstanceKey:           []string{"k8s.pod.name"},
		Aggregates:            []string{"sum", "count", "max"},
		M:                     2000,
		D:                     6,
		Bits:                  8,
		FlushInterval:         10 * time.Second,
		KeyframeEvery:         6,
		KeyframeFullDictEvery: 10,
		EpochRotate:           time.Hour,
		SeriesTTL:             90 * time.Second,
		Storm:                 StormConfig{EnergyMultiplier: 25, FallbackTopK: 100},
		Snapshot:              SnapshotConfig{Enabled: true, ResidualThreshold: 2.0, MaxSeriesPerFlush: 100, BlobTTL: time.Hour},
		RingBuffer:            RingBufferConfig{Window: 15 * time.Minute, MaxInstancesPerLogical: 1000, MaxTotalBytes: "100MB"},
		Churn:                 ChurnConfig{MaxBirthsPerLogicalPerMin: 100},
		Pull:                  PullConfig{Enabled: false},
		DrilldownHint:         true,
		Tiers: []TierConfig{
			{Match: `^billing_.*`, Tier: tierExact},
			{Match: `.*`, Tier: tierSketched},
		},
		Output: OutputConfig{Dir: t.TempDir()},
	}
}

func TestConfigValidate_Valid(t *testing.T) {
	cfg := validConfig(t)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid config to pass, got: %v", err)
	}
}

func TestConfigValidate_Fields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"missing tenant_id", func(c *Config) { c.TenantID = "" }, "tenant_id is required"},
		{"missing tenant_key_env", func(c *Config) { c.TenantKeyEnv = "" }, "tenant_key_env is required"},
		{"tenant_key_env unset", func(c *Config) { c.TenantKeyEnv = "CSRESIDUAL_TEST_DOES_NOT_EXIST" }, "empty/unset environment variable"},
		{"missing logical_key", func(c *Config) { c.LogicalKey = nil }, "logical_key must declare"},
		{"missing aggregates", func(c *Config) { c.Aggregates = nil }, "aggregates must declare"},
		{"invalid aggregate", func(c *Config) { c.Aggregates = []string{"avg"} }, `unknown aggregate "avg"`},
		{"m not positive", func(c *Config) { c.M = 0 }, "m must be positive"},
		{"d not positive", func(c *Config) { c.D = -1 }, "d must be positive"},
		{"bits invalid", func(c *Config) { c.Bits = 32 }, "bits must be 8 or 16"},
		{"flush_interval not positive", func(c *Config) { c.FlushInterval = 0 }, "flush_interval must be positive"},
		{"keyframe_every not positive", func(c *Config) { c.KeyframeEvery = 0 }, "keyframe_every must be positive"},
		{"golden cadence both unset", func(c *Config) { c.KeyframeFullDictEvery = 0; c.GoldenKeyframeEvery = 0 }, "keyframe_full_dict_every or golden_keyframe_every is required"},
		{"golden cadence disagree", func(c *Config) { c.KeyframeFullDictEvery = 10; c.GoldenKeyframeEvery = 20 }, "disagree"},
		{"epoch_rotate not positive", func(c *Config) { c.EpochRotate = 0 }, "epoch_rotate must be positive"},
		{"series_ttl not positive", func(c *Config) { c.SeriesTTL = 0 }, "series_ttl must be positive"},
		{"storm multiplier too low", func(c *Config) { c.Storm.EnergyMultiplier = 1 }, "storm.energy_multiplier must be > 1"},
		{"storm topk not positive", func(c *Config) { c.Storm.FallbackTopK = 0 }, "storm.fallback_topk must be positive"},
		{"snapshot threshold not positive", func(c *Config) { c.Snapshot.ResidualThreshold = 0 }, "snapshot.residual_threshold must be positive"},
		{"snapshot max series not positive", func(c *Config) { c.Snapshot.MaxSeriesPerFlush = 0 }, "snapshot.max_series_per_flush must be positive"},
		{"snapshot blob ttl not positive", func(c *Config) { c.Snapshot.BlobTTL = 0 }, "snapshot.blob_ttl must be positive"},
		{"ringbuffer window not positive", func(c *Config) { c.RingBuffer.Window = 0 }, "ringbuffer.window must be positive"},
		{"ringbuffer max instances not positive", func(c *Config) { c.RingBuffer.MaxInstancesPerLogical = 0 }, "ringbuffer.max_instances_per_logical must be positive"},
		{"ringbuffer max bytes invalid", func(c *Config) { c.RingBuffer.MaxTotalBytes = "lots" }, "ringbuffer.max_total_bytes"},
		{"churn max births not positive", func(c *Config) { c.Churn.MaxBirthsPerLogicalPerMin = 0 }, "churn.max_births_per_logical_per_min must be positive"},
		{"pull enabled missing addr", func(c *Config) {
			c.Pull = PullConfig{Enabled: true, MTLS: true, BearerTokenEnv: "X", TLSCertFile: "a", TLSKeyFile: "b", TLSClientCAFile: "c"}
		}, "pull.addr is required"},
		{"pull enabled without mTLS", func(c *Config) {
			c.Pull = PullConfig{Enabled: true, Addr: ":8889", MTLS: false, BearerTokenEnv: "X"}
		}, "pull.mTLS must be true"},
		{"pull mTLS missing cert files", func(c *Config) {
			c.Pull = PullConfig{Enabled: true, Addr: ":8889", MTLS: true, BearerTokenEnv: "X"}
		}, "tls_cert_file"},
		{"pull enabled missing bearer env", func(c *Config) {
			c.Pull = PullConfig{Enabled: true, Addr: ":8889", MTLS: true, TLSCertFile: "a", TLSKeyFile: "b", TLSClientCAFile: "c"}
		}, "pull.bearer_token_env is required"},
		{"pull bearer env unset", func(c *Config) {
			c.Pull = PullConfig{Enabled: true, Addr: ":8889", MTLS: true, BearerTokenEnv: "CSRESIDUAL_TEST_DOES_NOT_EXIST", TLSCertFile: "a", TLSKeyFile: "b", TLSClientCAFile: "c"}
		}, "empty/unset environment variable"},
		{"tier invalid regex", func(c *Config) { c.Tiers = []TierConfig{{Match: "(", Tier: tierExact}} }, "invalid match regex"},
		{"tier invalid value", func(c *Config) { c.Tiers = []TierConfig{{Match: ".*", Tier: "maybe"}} }, `tier must be "exact"`},
		{"view missing name", func(c *Config) { c.Views = []ViewConfig{{Labels: []string{"a"}}} }, "views[0]: name is required"},
		{"view missing labels", func(c *Config) { c.Views = []ViewConfig{{Name: "v1"}} }, "views[0]: labels must declare"},
		{"view duplicate name", func(c *Config) {
			c.Views = []ViewConfig{{Name: "v1", Labels: []string{"a"}}, {Name: "v1", Labels: []string{"b"}}}
		}, "duplicate view name"},
		{"missing output dir", func(c *Config) { c.Output.Dir = "" }, "output.dir is required"},
		{"unknown output type", func(c *Config) { c.Output.Type = "sqs" }, `unknown value "sqs"`},
		{"kafka missing brokers", func(c *Config) {
			c.Output = OutputConfig{Type: "kafka", Kafka: KafkaOutputConfig{Topic: "t", SpoolDir: "/tmp/spool", SpoolMaxBytes: "100MB", DrainInterval: time.Second}}
		}, "output.kafka.brokers is required"},
		{"kafka missing topic", func(c *Config) {
			c.Output = OutputConfig{Type: "kafka", Kafka: KafkaOutputConfig{Brokers: []string{"b:9092"}, SpoolDir: "/tmp/spool", SpoolMaxBytes: "100MB", DrainInterval: time.Second}}
		}, "output.kafka.topic is required"},
		{"kafka missing spool_dir", func(c *Config) {
			c.Output = OutputConfig{Type: "kafka", Kafka: KafkaOutputConfig{Brokers: []string{"b:9092"}, Topic: "t", SpoolMaxBytes: "100MB", DrainInterval: time.Second}}
		}, "output.kafka.spool_dir is required"},
		{"kafka invalid spool_max_bytes", func(c *Config) {
			c.Output = OutputConfig{Type: "kafka", Kafka: KafkaOutputConfig{Brokers: []string{"b:9092"}, Topic: "t", SpoolDir: "/tmp/spool", SpoolMaxBytes: "lots", DrainInterval: time.Second}}
		}, "output.kafka.spool_max_bytes"},
		{"kafka drain_interval not positive", func(c *Config) {
			c.Output = OutputConfig{Type: "kafka", Kafka: KafkaOutputConfig{Brokers: []string{"b:9092"}, Topic: "t", SpoolDir: "/tmp/spool", SpoolMaxBytes: "100MB"}}
		}, "output.kafka.drain_interval must be positive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig(t)
			tt.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestConfigValidate_KafkaOutputValid(t *testing.T) {
	cfg := validConfig(t)
	cfg.Output = OutputConfig{
		Type: "kafka",
		Kafka: KafkaOutputConfig{
			Brokers:       []string{"broker1:9092", "broker2:9092"},
			Topic:         "palimpsest-frames",
			SpoolDir:      t.TempDir(),
			SpoolMaxBytes: "1GB",
			DrainInterval: 10 * time.Second,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected a fully-specified kafka output config to pass, got: %v", err)
	}
}

func TestResolveOutputType(t *testing.T) {
	if got := resolveOutputType(&Config{}); got != outputTypeFile {
		t.Fatalf("resolveOutputType(unset) = %q, want %q", got, outputTypeFile)
	}
	if got := resolveOutputType(&Config{Output: OutputConfig{Type: "kafka"}}); got != outputTypeKafka {
		t.Fatalf("resolveOutputType(kafka) = %q, want %q", got, outputTypeKafka)
	}
}

func TestConfigValidate_PullDisabledSkipsPullChecks(t *testing.T) {
	cfg := validConfig(t)
	cfg.Pull = PullConfig{Enabled: false} // no addr/mTLS/bearer/tls files at all
	if err := cfg.Validate(); err != nil {
		t.Fatalf("pull.enabled=false should skip all pull validation, got: %v", err)
	}
}

func TestConfigValidate_SnapshotDisabledSkipsSnapshotChecks(t *testing.T) {
	cfg := validConfig(t)
	cfg.Snapshot = SnapshotConfig{Enabled: false} // zero threshold/max/ttl
	if err := cfg.Validate(); err != nil {
		t.Fatalf("snapshot.enabled=false should skip snapshot validation, got: %v", err)
	}
}

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"100MB", 100 << 20, false},
		{"1GB", 1 << 30, false},
		{"512KB", 512 << 10, false},
		{"10B", 10, false},
		{"1TB", 1 << 40, false},
		{"  256MB  ", 256 << 20, false},
		{"100mb", 100 << 20, false}, // case-insensitive
		{"100", 0, true},            // missing unit
		{"abc", 0, true},
		{"", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseByteSize(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseByteSize(%q): expected error, got %d", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseByteSize(%q): unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("parseByteSize(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolveGoldenEvery(t *testing.T) {
	tests := []struct {
		name    string
		full    int
		golden  int
		want    int
		wantErr bool
	}{
		{"only full dict set", 10, 0, 10, false},
		{"only golden set", 0, 7, 7, false},
		{"both set and equal", 5, 5, 5, false},
		{"both set and disagree", 5, 6, 0, true},
		{"neither set", 0, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{KeyframeFullDictEvery: tt.full, GoldenKeyframeEvery: tt.golden}
			got, err := cfg.resolveGoldenEvery()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveGoldenEvery() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolveViews(t *testing.T) {
	t.Run("no views declared falls back to logical_key as view 0", func(t *testing.T) {
		cfg := &Config{LogicalKey: []string{"service.name"}}
		views := resolveViews(cfg)
		if len(views) != 1 || views[0].id != 0 || len(views[0].labels) != 1 || views[0].labels[0] != "service.name" {
			t.Fatalf("resolveViews() = %+v, want single default view over logical_key", views)
		}
	})

	t.Run("declared views are indexed in order", func(t *testing.T) {
		cfg := &Config{
			Views: []ViewConfig{
				{Name: "by_deployment", Labels: []string{"service.name", "k8s.deployment.name"}},
				{Name: "by_region_az", Labels: []string{"cloud.region", "k8s.cluster.name"}},
			},
		}
		views := resolveViews(cfg)
		if len(views) != 2 {
			t.Fatalf("resolveViews() returned %d views, want 2", len(views))
		}
		if views[0].id != 0 || views[0].name != "by_deployment" {
			t.Fatalf("views[0] = %+v, want id=0 name=by_deployment", views[0])
		}
		if views[1].id != 1 || views[1].name != "by_region_az" {
			t.Fatalf("views[1] = %+v, want id=1 name=by_region_az", views[1])
		}
	})
}

// TestMatchTier_AllTiers exercises ADR-005 tiering end to end: exact-match
// rules, the default-sketched fallback, and the hard summary/quantile
// override that no config can turn off.
func TestMatchTier_AllTiers(t *testing.T) {
	rules, err := compileTierRules([]TierConfig{
		{Match: `^billing_.*`, Tier: tierExact},
		{Match: `^slo_.*`, Tier: tierExact},
		{Match: `.*`, Tier: tierSketched},
	})
	if err != nil {
		t.Fatalf("compileTierRules: %v", err)
	}

	tests := []struct {
		metric    string
		isSummary bool
		want      string
	}{
		{"billing_requests_total", false, tierExact},
		{"slo_error_budget", false, tierExact},
		{"cpu_usage", false, tierSketched},
		{"http_requests_total", false, tierSketched},
		// A summary is forced exact even though it would otherwise match a
		// sketched rule (ADR-005: "quantiles forced to exact tier").
		{"billing_latency_summary", true, tierExact},
		{"http_latency_summary", true, tierExact},
	}
	for _, tt := range tests {
		t.Run(tt.metric, func(t *testing.T) {
			if got := matchTier(rules, tt.metric, tt.isSummary); got != tt.want {
				t.Errorf("matchTier(%q, isSummary=%v) = %q, want %q", tt.metric, tt.isSummary, got, tt.want)
			}
		})
	}
}

func TestMatchTier_NoRulesDefaultsSketched(t *testing.T) {
	if got := matchTier(nil, "anything", false); got != tierSketched {
		t.Fatalf("matchTier with no rules = %q, want %q", got, tierSketched)
	}
}

func TestCompileTierRules_InvalidRegexRejected(t *testing.T) {
	if _, err := compileTierRules([]TierConfig{{Match: "(", Tier: tierExact}}); err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

func TestCompileTierRules_InvalidTierRejected(t *testing.T) {
	if _, err := compileTierRules([]TierConfig{{Match: ".*", Tier: "sometimes"}}); err == nil {
		t.Fatal("expected error for invalid tier value")
	}
}
