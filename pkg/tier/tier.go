/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package tier implements ADR-005 tier selection: a first-match-wins regex
// rule list that classifies metric names into one of three processing tiers
// (exact, sketched, merged). It has no external dependencies.
package tier

import (
	"fmt"
	"regexp"
)

// The three tier names (ADR-005).
const (
	Exact    = "exact"
	Sketched = "sketched"
	Merged   = "merged"
)

// Rule is one raw tier-routing entry. The first Rule whose Match regex
// matches a metric name wins (ADR-005: "first-match-wins").
type Rule struct {
	Match string
	Tier  string
}

// CompiledRules is the validated, compiled form of a []Rule.
type CompiledRules []compiledRule

type compiledRule struct {
	re   *regexp.Regexp
	tier string
}

// Compile validates and compiles raw rules. It returns an error if any Tier
// value is unrecognised or any Match pattern is an invalid regexp.
func Compile(rules []Rule) (CompiledRules, error) {
	out := make(CompiledRules, 0, len(rules))
	for i, r := range rules {
		switch r.Tier {
		case Exact, Sketched, Merged:
		default:
			return nil, fmt.Errorf("rules[%d]: tier must be %q, %q, or %q, got %q",
				i, Exact, Sketched, Merged, r.Tier)
		}
		re, err := regexp.Compile(r.Match)
		if err != nil {
			return nil, fmt.Errorf("rules[%d]: invalid match regex %q: %w", i, r.Match, err)
		}
		out = append(out, compiledRule{re: re, tier: r.Tier})
	}
	return out, nil
}

// Match resolves the processing tier for metricName (ADR-005):
//   - Summary/quantile metrics are always forced to Exact regardless of rules.
//   - Otherwise the first matching rule wins.
//   - A metric matching no rule defaults to Sketched.
func Match(rules CompiledRules, metricName string, isSummary bool) string {
	if isSummary {
		return Exact
	}
	for _, r := range rules {
		if r.re.MatchString(metricName) {
			return r.tier
		}
	}
	return Sketched
}
