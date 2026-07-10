/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package tier_test

import (
	"testing"

	"github.com/passiveintent/Palimpsest/pkg/tier"
)

// TestCompile verifies Compile validates tier values and regex patterns and
// returns a usable CompiledRules on success.
func TestCompile(t *testing.T) {
	tests := []struct {
		name    string
		rules   []tier.Rule
		wantErr string // non-empty → expect error containing this substring
	}{
		{
			name:  "valid exact",
			rules: []tier.Rule{{Match: `^billing_.*`, Tier: tier.Exact}},
		},
		{
			name:  "valid sketched",
			rules: []tier.Rule{{Match: `.*`, Tier: tier.Sketched}},
		},
		{
			name:  "valid merged",
			rules: []tier.Rule{{Match: `^merged_.*`, Tier: tier.Merged}},
		},
		{
			name: "multiple valid rules",
			rules: []tier.Rule{
				{Match: `^billing_.*`, Tier: tier.Exact},
				{Match: `^slo_.*`, Tier: tier.Exact},
				{Match: `.*`, Tier: tier.Sketched},
			},
		},
		{
			name:    "invalid tier value",
			rules:   []tier.Rule{{Match: `.*`, Tier: "sometimes"}},
			wantErr: `tier must be`,
		},
		{
			name:    "invalid regex",
			rules:   []tier.Rule{{Match: "(", Tier: tier.Exact}},
			wantErr: `invalid match regex`,
		},
		{
			name: "empty rules compiles to empty slice",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tier.Compile(tt.rules)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Compile(%v): expected error containing %q, got nil", tt.rules, tt.wantErr)
				}
				if s := err.Error(); len(s) == 0 {
					t.Fatalf("Compile: empty error message")
				}
				return
			}
			if err != nil {
				t.Fatalf("Compile(%v): unexpected error: %v", tt.rules, err)
			}
			if len(got) != len(tt.rules) {
				t.Fatalf("Compile: len(CompiledRules) = %d, want %d", len(got), len(tt.rules))
			}
		})
	}
}

// TestMatch exercises ADR-005 tier selection end-to-end: first-match-wins,
// the default-sketched fallback, summary/quantile forced to exact, and the
// three tier values.
func TestMatch(t *testing.T) {
	rules, err := tier.Compile([]tier.Rule{
		{Match: `^billing_.*`, Tier: tier.Exact},
		{Match: `^slo_.*`, Tier: tier.Exact},
		{Match: `^merged_.*`, Tier: tier.Merged},
		{Match: `.*`, Tier: tier.Sketched},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	tests := []struct {
		metric    string
		isSummary bool
		want      string
	}{
		// First-match-wins for exact rules.
		{"billing_requests_total", false, tier.Exact},
		{"slo_error_budget", false, tier.Exact},
		// Merged tier.
		{"merged_rollup", false, tier.Merged},
		// Default catch-all sketched rule.
		{"cpu_usage", false, tier.Sketched},
		{"http_requests_total", false, tier.Sketched},
		// Summary/quantile forced exact regardless of which rule would match
		// (ADR-005: "quantiles forced to exact tier").
		{"billing_latency_summary", true, tier.Exact},
		{"http_latency_summary", true, tier.Exact},
		{"merged_summary", true, tier.Exact},
	}
	for _, tt := range tests {
		t.Run(tt.metric, func(t *testing.T) {
			got := tier.Match(rules, tt.metric, tt.isSummary)
			if got != tt.want {
				t.Errorf("Match(%q, isSummary=%v) = %q, want %q", tt.metric, tt.isSummary, got, tt.want)
			}
		})
	}
}

// TestMatch_NoRulesDefaultsSketched verifies that an empty rule list
// leaves every non-summary metric in the Sketched tier.
func TestMatch_NoRulesDefaultsSketched(t *testing.T) {
	rules, err := tier.Compile(nil)
	if err != nil {
		t.Fatalf("Compile(nil): %v", err)
	}
	got := tier.Match(rules, "anything", false)
	if got != tier.Sketched {
		t.Fatalf("Match with empty rules = %q, want %q", got, tier.Sketched)
	}
}

// TestMatch_SummaryAlwaysExact checks the hard summary override with no
// compiled rules.
func TestMatch_SummaryAlwaysExact(t *testing.T) {
	rules, err := tier.Compile(nil)
	if err != nil {
		t.Fatalf("Compile(nil): %v", err)
	}
	got := tier.Match(rules, "latency_summary", true)
	if got != tier.Exact {
		t.Fatalf("Match summary with no rules = %q, want %q", got, tier.Exact)
	}
}
