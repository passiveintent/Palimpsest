/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package remoteprom implements ports.SeriesSink as a Prometheus
// remote-write client (ADR-010's Layer 1: keyframes/rehydrated series
// persisted as an exact, forever-queryable PromQL layer; ADR-011's
// placement-arbitrage economics depend on this landing in commodity
// storage). The protobuf WriteRequest and Snappy block-format encodings it
// needs are hand-rolled in prompb.go/snappy.go rather than pulled in as
// dependencies — see those files' doc comments.
/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package remoteprom

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/passiveintent/Palimpsest/internal/ports"
)

const defaultTimeout = 10 * time.Second

// Sink POSTs ports.Samples to a Prometheus remote-write endpoint.
type Sink struct {
	URL    string
	Client *http.Client
}

// New returns a Sink writing to url (a Prometheus remote-write endpoint,
// e.g. "http://localhost:9090/api/v1/write").
func New(url string) *Sink {
	return &Sink{URL: url, Client: &http.Client{Timeout: defaultTimeout}}
}

// WriteSeries implements ports.SeriesSink: each Sample becomes its own
// single-sample TimeSeries (palimpsestd doesn't batch multiple timestamps
// per series per call).
func (s *Sink) WriteSeries(ctx context.Context, samples []ports.Sample) error {
	if len(samples) == 0 {
		return nil
	}

	series := make([][]byte, 0, len(samples))
	for _, smp := range samples {
		labels := buildLabels(smp)
		series = append(series, encodeTimeSeries(labels, []promSample{{Value: smp.Value, TimestampMs: smp.TimestampMs}}))
	}
	body := encodeWriteRequest(series)
	compressed := snappyEncodeBlock(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("remoteprom: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")

	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("remoteprom: posting to %s: %w", s.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("remoteprom: %s returned %s", s.URL, resp.Status)
	}
	return nil
}

// buildLabels derives a Prometheus label set for smp, sorted by name (a
// remote-write requirement). Palimpsest logical series names contain
// characters ("|", "=", ",") that aren't valid in a Prometheus metric name,
// so the raw name (if any) rides as a separate label and __name__ is
// sanitized.
func buildLabels(smp ports.Sample) []promLabel {
	name := smp.Name
	if name == "" {
		name = "palimpsest_series"
	}
	labels := []promLabel{
		{Name: "__name__", Value: sanitizeMetricName(name)},
		{Name: "palimpsest_series_id", Value: strconv.FormatUint(smp.SeriesID, 10)},
		{Name: "palimpsest_shard_id", Value: strconv.FormatUint(smp.ShardID, 10)},
		{Name: "palimpsest_view_id", Value: strconv.FormatUint(uint64(smp.ViewID), 10)},
	}
	if smp.Name != "" {
		labels = append(labels, promLabel{Name: "palimpsest_logical_name", Value: smp.Name})
	}
	sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })
	return labels
}

// sanitizeMetricName maps s onto Prometheus's metric-name charset
// ([a-zA-Z_:][a-zA-Z0-9_:]*), replacing every other rune with '_' and
// prefixing an otherwise-leading digit.
func sanitizeMetricName(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_', c == ':', c >= '0' && c <= '9':
			b[i] = c
		default:
			b[i] = '_'
		}
	}
	// A digit is valid everywhere except the leading position; prepend
	// rather than mangle it there so e.g. "1xyz" and "2xyz" don't collide
	// on the same sanitized name.
	if len(b) > 0 && b[0] >= '0' && b[0] <= '9' {
		return "_" + string(b)
	}
	return string(b)
}
