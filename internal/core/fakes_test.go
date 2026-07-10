/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package core

import (
	"context"
	"sync"
	"time"

	"github.com/passiveintent/Palimpsest/internal/ports"
)

// fakeAnomalySink is the in-memory ports.AnomalySink fake used by every
// test in this package.
type fakeAnomalySink struct {
	mu     sync.Mutex
	events []ports.AnomalyEvent
}

func (f *fakeAnomalySink) EmitAnomaly(_ context.Context, ev ports.AnomalyEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return nil
}

func (f *fakeAnomalySink) Events() []ports.AnomalyEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ports.AnomalyEvent(nil), f.events...)
}

// fakeSeriesSink is the in-memory ports.SeriesSink fake used by every test
// in this package.
type fakeSeriesSink struct {
	mu      sync.Mutex
	samples []ports.Sample
}

func (f *fakeSeriesSink) WriteSeries(_ context.Context, samples []ports.Sample) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.samples = append(f.samples, samples...)
	return nil
}

func (f *fakeSeriesSink) Samples() []ports.Sample {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ports.Sample(nil), f.samples...)
}

// fakeClock is a manually-advanced time source, swapped in for Engine.now
// so watermark/repair-horizon/clock-skew tests are deterministic.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}
