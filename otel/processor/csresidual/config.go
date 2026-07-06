/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package csresidual

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/collector/confmap"
)

var _ confmap.Validator = (*Config)(nil)

// TenantKeyEntry is one entry in the tenant_keys list for key rotation
// (ADR-012 §Addendum). Version must be unique across entries in a Config.
type TenantKeyEntry struct {
	Version uint8  `mapstructure:"version"`
	EnvVar  string `mapstructure:"env_var"`
}

// ViewConfig declares one sketch-cube view (ADR-010): an independent
// (identity, dictionary, accumulator) slicing of the same incoming
// metrics, tagged on the wire by its index in Config.Views (Frame.ViewID).
type ViewConfig struct {
	Name   string   `mapstructure:"name"`
	Labels []string `mapstructure:"labels"`
}

// TierConfig is one `tiers` rule (ADR-005): the first Match regex (tested
// against the metric name) that matches wins Tier ("exact" or "sketched").
type TierConfig struct {
	Match string `mapstructure:"match"`
	Tier  string `mapstructure:"tier"`
}

// StormConfig configures ADR-004 storm detection (pre-quantization energy
// spikes fall back to FALLBACK frames rather than corrupt recovery).
type StormConfig struct {
	EnergyMultiplier float64 `mapstructure:"energy_multiplier"`
	FallbackTopK     int     `mapstructure:"fallback_topk"`
}

// SnapshotConfig configures ADR-009/ADR-012 speculative snapshot push.
type SnapshotConfig struct {
	Enabled           bool          `mapstructure:"enabled"`
	ResidualThreshold float64       `mapstructure:"residual_threshold"`
	MaxSeriesPerFlush int           `mapstructure:"max_series_per_flush"`
	BlobTTL           time.Duration `mapstructure:"blob_ttl"`
}

// RingBufferConfig configures the agent-side instance ring buffer
// (ADR-008): pod/instance labels never enter the sketch, they live here.
type RingBufferConfig struct {
	Window                 time.Duration `mapstructure:"window"`
	MaxInstancesPerLogical int           `mapstructure:"max_instances_per_logical"`
	MaxTotalBytes          string        `mapstructure:"max_total_bytes"`
}

// ChurnConfig configures the ADR-009 churn circuit breaker.
type ChurnConfig struct {
	MaxBirthsPerLogicalPerMin int `mapstructure:"max_births_per_logical_per_min"`
}

// PullConfig configures the optional ADR-012 pull endpoint (default off,
// zero listening sockets). TLSCertFile/TLSKeyFile/TLSClientCAFile are not
// in the illustrative config block this processor was speced from, but are
// required for MTLS to mean anything on the wire (a server can't do mTLS
// without its own serving cert and a CA pool to verify client certs
// against); they are only validated when Enabled && MTLS.
type PullConfig struct {
	Enabled         bool   `mapstructure:"enabled"`
	Addr            string `mapstructure:"addr"`
	MTLS            bool   `mapstructure:"mTLS"`
	BearerTokenEnv  string `mapstructure:"bearer_token_env"`
	TLSCertFile     string `mapstructure:"tls_cert_file"`
	TLSKeyFile      string `mapstructure:"tls_key_file"`
	TLSClientCAFile string `mapstructure:"tls_client_ca_file"`
}

// OutputConfig configures where flushed frames are written.
type OutputConfig struct {
	// Type selects the frame sink: "file" (the default; also implied by
	// leaving Type unset) writes one file per frame under Dir, exactly as
	// before this field existed. "kafka" instead publishes frames to a
	// Kafka topic (KafkaOutputConfig) via internal/adapters/kafka, behind a
	// bounded on-disk spool (KafkaOutputConfig.SpoolDir) that absorbs broker
	// outages and drains once the broker is reachable again.
	Type  string            `mapstructure:"type"`
	Dir   string            `mapstructure:"dir"`
	Kafka KafkaOutputConfig `mapstructure:"kafka"`
}

// KafkaOutputConfig configures the "kafka" OutputConfig.Type. Frames are
// keyed by tenant_id||shard_id (internal/adapters/kafka.PartitionKey) using
// Config.TenantID, so ordering within one shard's stream is preserved
// end-to-end regardless of how many other shards/views share the topic.
type KafkaOutputConfig struct {
	Brokers []string `mapstructure:"brokers"`
	Topic   string   `mapstructure:"topic"`

	// SpoolDir buffers frames on disk whenever a publish attempt fails
	// (broker unreachable), and is drained — oldest frame first — on every
	// subsequent publish attempt and on DrainInterval's own ticker, so a
	// quiet (shard, view) pipeline's backlog still flushes promptly after
	// the broker reconnects even without new frames of its own to flush.
	SpoolDir      string        `mapstructure:"spool_dir"`
	SpoolMaxBytes string        `mapstructure:"spool_max_bytes"`
	DrainInterval time.Duration `mapstructure:"drain_interval"`
}

// Config is the csresidual processor configuration. See README.md for the
// full annotated YAML and the package doc for which ADR each field
// implements.
type Config struct {
	TenantID     string `mapstructure:"tenant_id"`
	TenantKeyEnv string `mapstructure:"tenant_key_env"`
	// TenantKeys is the ordered list of tenant key versions for key rotation
	// (ADR-012 §Addendum). When non-empty, replaces TenantKeyEnv.
	// The key material for each version is read from the named env var.
	TenantKeys []TenantKeyEntry `mapstructure:"tenant_keys"`
	// ActiveKeyVersion is the key_version stamped on newly encoded frames.
	// Must match a version present in TenantKeys when TenantKeys is set.
	// Ignored in single-key mode (TenantKeyEnv only).
	ActiveKeyVersion uint8        `mapstructure:"active_key_version"`
	ShardBy          []string     `mapstructure:"shard_by"`
	LogicalKey       []string     `mapstructure:"logical_key"`
	InstanceKey      []string     `mapstructure:"instance_key"`
	Aggregates       []string     `mapstructure:"aggregates"`
	Views            []ViewConfig `mapstructure:"views"`

	M    int `mapstructure:"m"`
	D    int `mapstructure:"d"`
	Bits int `mapstructure:"bits"`

	FlushInterval         time.Duration `mapstructure:"flush_interval"`
	KeyframeEvery         int           `mapstructure:"keyframe_every"`
	KeyframeFullDictEvery int           `mapstructure:"keyframe_full_dict_every"`
	GoldenKeyframeEvery   int           `mapstructure:"golden_keyframe_every"`
	EpochRotate           time.Duration `mapstructure:"epoch_rotate"`
	SeriesTTL             time.Duration `mapstructure:"series_ttl"`

	Storm      StormConfig      `mapstructure:"storm"`
	Snapshot   SnapshotConfig   `mapstructure:"snapshot"`
	RingBuffer RingBufferConfig `mapstructure:"ringbuffer"`
	Churn      ChurnConfig      `mapstructure:"churn"`
	Pull       PullConfig       `mapstructure:"pull"`

	DrilldownHint bool         `mapstructure:"drilldown_hint"`
	Tiers         []TierConfig `mapstructure:"tiers"`
	Shadow        bool         `mapstructure:"shadow"`
	Output        OutputConfig `mapstructure:"output"`
}

var byteSizeRe = regexp.MustCompile(`^(\d+)\s*([KMGT]?B)$`)

var byteSizeUnits = map[string]int64{"B": 1, "KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40}

// parseByteSize parses a "100MB"-style size string (config's
// ringbuffer.max_total_bytes) into a byte count.
func parseByteSize(s string) (int64, error) {
	m := byteSizeRe.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf("invalid byte size %q (want e.g. \"100MB\")", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size %q: %w", s, err)
	}
	return n * byteSizeUnits[m[2]], nil
}

// resolveGoldenEvery reconciles the two config spellings for golden-keyframe
// cadence: docs/SPEC.md calls the field keyframe_full_dict_every, but this
// processor's own illustrative config also lists golden_keyframe_every with
// the same meaning ("full (non-KDELTA) every Nth"). Whichever one is set
// wins; if both are set they must agree.
func (cfg *Config) resolveGoldenEvery() (int, error) {
	switch {
	case cfg.KeyframeFullDictEvery == 0 && cfg.GoldenKeyframeEvery == 0:
		return 0, errors.New("one of keyframe_full_dict_every or golden_keyframe_every is required")
	case cfg.KeyframeFullDictEvery == 0:
		return cfg.GoldenKeyframeEvery, nil
	case cfg.GoldenKeyframeEvery == 0:
		return cfg.KeyframeFullDictEvery, nil
	case cfg.KeyframeFullDictEvery != cfg.GoldenKeyframeEvery:
		return 0, fmt.Errorf("keyframe_full_dict_every (%d) and golden_keyframe_every (%d) disagree; set only one, or set both equal", cfg.KeyframeFullDictEvery, cfg.GoldenKeyframeEvery)
	default:
		return cfg.KeyframeFullDictEvery, nil
	}
}

// Validate implements confmap.Validator.
func (cfg *Config) Validate() error {
	var errs []error
	requirePositive := func(v int, name string) {
		if v <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive", name))
		}
	}
	requirePositiveDur := func(v time.Duration, name string) {
		if v <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive", name))
		}
	}

	if cfg.TenantID == "" {
		errs = append(errs, errors.New("tenant_id is required (ADR-012)"))
	}
	// tenant key: either multi-key rotation list or single env-var.
	if len(cfg.TenantKeys) > 0 {
		seenKV := make(map[uint8]bool, len(cfg.TenantKeys))
		activeFound := false
		for i, e := range cfg.TenantKeys {
			if e.EnvVar == "" {
				errs = append(errs, fmt.Errorf("tenant_keys[%d]: env_var is required", i))
			} else if os.Getenv(e.EnvVar) == "" {
				errs = append(errs, fmt.Errorf("tenant_keys[%d] version=%d: env var %q is empty or unset", i, e.Version, e.EnvVar))
			}
			if seenKV[e.Version] {
				errs = append(errs, fmt.Errorf("tenant_keys: duplicate version %d", e.Version))
			}
			seenKV[e.Version] = true
			if e.Version == cfg.ActiveKeyVersion {
				activeFound = true
			}
		}
		if !activeFound {
			errs = append(errs, fmt.Errorf("active_key_version %d is not present in tenant_keys", cfg.ActiveKeyVersion))
		}
	} else {
		if cfg.TenantKeyEnv == "" {
			errs = append(errs, errors.New("tenant_key_env is required (ADR-012)"))
		} else if os.Getenv(cfg.TenantKeyEnv) == "" {
			errs = append(errs, fmt.Errorf("tenant_key_env %q names an empty/unset environment variable", cfg.TenantKeyEnv))
		}
	}
	if len(cfg.LogicalKey) == 0 {
		errs = append(errs, errors.New("logical_key must declare at least one label (ADR-008)"))
	}
	if len(cfg.Aggregates) == 0 {
		errs = append(errs, errors.New("aggregates must declare at least one of sum, count, max"))
	}
	for _, a := range cfg.Aggregates {
		switch a {
		case "sum", "count", "max":
		default:
			errs = append(errs, fmt.Errorf("aggregates: unknown aggregate %q (want sum, count, or max)", a))
		}
	}

	requirePositive(cfg.M, "m")
	requirePositive(cfg.D, "d")
	if cfg.Bits != 8 && cfg.Bits != 16 {
		errs = append(errs, fmt.Errorf("bits must be 8 or 16, got %d", cfg.Bits))
	}
	requirePositiveDur(cfg.FlushInterval, "flush_interval")
	requirePositive(cfg.KeyframeEvery, "keyframe_every")
	if golden, err := cfg.resolveGoldenEvery(); err != nil {
		errs = append(errs, err)
	} else {
		requirePositive(golden, "keyframe_full_dict_every/golden_keyframe_every")
	}
	requirePositiveDur(cfg.EpochRotate, "epoch_rotate")
	requirePositiveDur(cfg.SeriesTTL, "series_ttl")

	if cfg.Storm.EnergyMultiplier <= 1 {
		errs = append(errs, errors.New("storm.energy_multiplier must be > 1"))
	}
	requirePositive(cfg.Storm.FallbackTopK, "storm.fallback_topk")

	if cfg.Snapshot.Enabled {
		if cfg.Snapshot.ResidualThreshold <= 0 {
			errs = append(errs, errors.New("snapshot.residual_threshold must be positive when snapshot.enabled"))
		}
		requirePositive(cfg.Snapshot.MaxSeriesPerFlush, "snapshot.max_series_per_flush")
		requirePositiveDur(cfg.Snapshot.BlobTTL, "snapshot.blob_ttl")
	}

	requirePositiveDur(cfg.RingBuffer.Window, "ringbuffer.window")
	requirePositive(cfg.RingBuffer.MaxInstancesPerLogical, "ringbuffer.max_instances_per_logical")
	if _, err := parseByteSize(cfg.RingBuffer.MaxTotalBytes); err != nil {
		errs = append(errs, fmt.Errorf("ringbuffer.max_total_bytes: %w", err))
	}

	requirePositive(cfg.Churn.MaxBirthsPerLogicalPerMin, "churn.max_births_per_logical_per_min")

	if cfg.Pull.Enabled {
		if cfg.Pull.Addr == "" {
			errs = append(errs, errors.New("pull.addr is required when pull.enabled"))
		}
		if !cfg.Pull.MTLS {
			errs = append(errs, errors.New("pull.mTLS must be true when pull.enabled (ADR-012: pull endpoint requires mTLS + bearer token)"))
		} else if cfg.Pull.TLSCertFile == "" || cfg.Pull.TLSKeyFile == "" || cfg.Pull.TLSClientCAFile == "" {
			errs = append(errs, errors.New("pull.tls_cert_file, pull.tls_key_file, and pull.tls_client_ca_file are required when pull.mTLS"))
		}
		if cfg.Pull.BearerTokenEnv == "" {
			errs = append(errs, errors.New("pull.bearer_token_env is required when pull.enabled (ADR-012)"))
		} else if os.Getenv(cfg.Pull.BearerTokenEnv) == "" {
			errs = append(errs, fmt.Errorf("pull.bearer_token_env %q names an empty/unset environment variable", cfg.Pull.BearerTokenEnv))
		}
	}

	if _, err := compileTierRules(cfg.Tiers); err != nil {
		errs = append(errs, err)
	}

	seenViewNames := make(map[string]bool, len(cfg.Views))
	for i, v := range cfg.Views {
		if v.Name == "" {
			errs = append(errs, fmt.Errorf("views[%d]: name is required", i))
		} else if seenViewNames[v.Name] {
			errs = append(errs, fmt.Errorf("views[%d]: duplicate view name %q", i, v.Name))
		}
		seenViewNames[v.Name] = true
		if len(v.Labels) == 0 {
			errs = append(errs, fmt.Errorf("views[%d]: labels must declare at least one label", i))
		}
	}

	switch cfg.Output.Type {
	case "", outputTypeFile:
		if cfg.Output.Dir == "" {
			errs = append(errs, errors.New("output.dir is required when output.type is \"file\" (or unset)"))
		}
	case outputTypeKafka:
		k := cfg.Output.Kafka
		if len(k.Brokers) == 0 {
			errs = append(errs, errors.New("output.kafka.brokers is required when output.type is \"kafka\""))
		}
		if k.Topic == "" {
			errs = append(errs, errors.New("output.kafka.topic is required when output.type is \"kafka\""))
		}
		if k.SpoolDir == "" {
			errs = append(errs, errors.New("output.kafka.spool_dir is required when output.type is \"kafka\" (bounds the broker-outage backlog to disk)"))
		}
		if _, err := parseByteSize(k.SpoolMaxBytes); err != nil {
			errs = append(errs, fmt.Errorf("output.kafka.spool_max_bytes: %w", err))
		}
		requirePositiveDur(k.DrainInterval, "output.kafka.drain_interval")
	default:
		errs = append(errs, fmt.Errorf("output.type: unknown value %q (want \"file\" or \"kafka\")", cfg.Output.Type))
	}

	return errors.Join(errs...)
}

// Output.Type values.
const (
	outputTypeFile  = "file"
	outputTypeKafka = "kafka"
)

// resolveOutputType returns cfg.Output.Type, defaulting an unset value to
// "file" (the behavior before OutputConfig.Type existed).
func resolveOutputType(cfg *Config) string {
	if cfg.Output.Type == "" {
		return outputTypeFile
	}
	return cfg.Output.Type
}

// viewSpec is a resolved, indexed view: either every entry of cfg.Views (in
// declared order, tagged by its index as Frame.ViewID) or, if none are
// declared, a single implicit default view (id 0) built from logical_key.
type viewSpec struct {
	id     uint16
	name   string
	labels []string
}

func resolveViews(cfg *Config) []viewSpec {
	if len(cfg.Views) == 0 {
		return []viewSpec{{id: 0, name: "default", labels: cfg.LogicalKey}}
	}
	out := make([]viewSpec, len(cfg.Views))
	for i, v := range cfg.Views {
		out[i] = viewSpec{id: uint16(i), name: v.Name, labels: v.Labels}
	}
	return out
}
