// Package webhook implements ports.AnomalySink as an HTTP POST of the
// anomaly's JSON encoding to a configured URL.
/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/passiveintent/Palimpsest/internal/ports"
)

// defaultTimeout bounds one webhook POST so a hung downstream endpoint
// can't block the engine indefinitely.
const defaultTimeout = 10 * time.Second

// Sink POSTs each AnomalyEvent, JSON-encoded, to URL.
type Sink struct {
	URL    string
	Client *http.Client
}

// New returns a Sink posting to url with a default client/timeout.
func New(url string) *Sink {
	return &Sink{URL: url, Client: &http.Client{Timeout: defaultTimeout}}
}

// EmitAnomaly implements ports.AnomalySink.
func (s *Sink) EmitAnomaly(ctx context.Context, ev ports.AnomalyEvent) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("webhook: marshaling anomaly: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: posting to %s: %w", s.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("webhook: %s returned %s", s.URL, resp.Status)
	}
	return nil
}
