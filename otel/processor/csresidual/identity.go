package csresidual

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

// Tier names (ADR-005): billing meters and quantiles are forced to exact;
// everything else defaults to sketched unless a tiers rule says otherwise.
const (
	tierExact    = "exact"
	tierSketched = "sketched"
)

// kv is one resolved (label key, string value) pair used to build a
// logical series identity (docs/SPEC.md "Identity").
type kv struct {
	key string
	val string
}

// attrString looks up key in dp (datapoint attributes), falling back to res
// (resource attributes) if absent from dp. A key present on neither
// resolves to "" so identity construction stays total/deterministic rather
// than erroring on sparse or optional labels.
func attrString(dp, res pcommon.Map, key string) string {
	if v, ok := dp.Get(key); ok {
		return v.AsString()
	}
	if v, ok := res.Get(key); ok {
		return v.AsString()
	}
	return ""
}

// resolvePairs resolves each of keys against dp/res attributes, preserving
// configured order (the order the logical name is built in).
func resolvePairs(keys []string, dp, res pcommon.Map) []kv {
	pairs := make([]kv, len(keys))
	for i, k := range keys {
		pairs[i] = kv{key: k, val: attrString(dp, res, k)}
	}
	return pairs
}

// joinKV renders pairs as "k1=v1,k2=v2" (docs/SPEC.md: "join(logical_key
// values, \",\")"). Also used standalone for instance keys (ring buffer /
// fold identity), which never carry a metric-name prefix.
func joinKV(pairs []kv) string {
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(p.key)
		b.WriteByte('=')
		b.WriteString(p.val)
	}
	return b.String()
}

// groupKey builds the pre-aggregate logical identity for metric under
// pairs: "metric_name|k1=v1,k2=v2" (docs/SPEC.md "Identity", minus the
// "|agg=" suffix logicalName appends). aggregate.go folds instances on this
// key; ringbuffer.go stores per-instance samples under it.
func groupKey(metric string, pairs []kv) string {
	return metric + "|" + joinKV(pairs)
}

// logicalName appends the "|agg=" suffix (docs/SPEC.md) that turns a group
// key into the wire-level logical series name actually sketched: each
// configured aggregate (sum/count/max) fans one group out into its own
// dictionary entry (ADR-008).
func logicalName(group, agg string) string {
	return group + "|agg=" + agg
}

// leKV returns the synthetic "le" bucket-bound pair a histogram bucket's
// group key is built with ("Histograms: each bucket = separate logical
// series (name|le=bound|...|agg=sum)"). +Inf marks the implicit final
// (overflow) bucket of an OTel explicit-bounds histogram.
func leKV(bound float64) kv {
	if math.IsInf(bound, 1) {
		return kv{key: "le", val: "+Inf"}
	}
	return kv{key: "le", val: strconv.FormatFloat(bound, 'g', -1, 64)}
}

// tierRule is one compiled entry of the config's `tiers` list (ADR-005):
// the first rule whose regex matches a metric name wins.
type tierRule struct {
	re   *regexp.Regexp
	tier string
}

// compileTierRules compiles raw (match, tier) pairs, validating the tier
// value and regex syntax up front so bad config fails at Validate() time
// rather than on the first matching metric.
func compileTierRules(raw []TierConfig) ([]tierRule, error) {
	rules := make([]tierRule, 0, len(raw))
	for i, r := range raw {
		switch r.Tier {
		case tierExact, tierSketched:
		default:
			return nil, fmt.Errorf("tiers[%d]: tier must be %q or %q, got %q", i, tierExact, tierSketched, r.Tier)
		}
		re, err := regexp.Compile(r.Match)
		if err != nil {
			return nil, fmt.Errorf("tiers[%d]: invalid match regex %q: %w", i, r.Match, err)
		}
		rules = append(rules, tierRule{re: re, tier: r.Tier})
	}
	return rules, nil
}

// matchTier resolves metricName's tier (ADR-005): summary/quantile metrics
// are always forced exact regardless of config; otherwise the first
// matching rule wins; a metric matching no rule defaults to sketched.
func matchTier(rules []tierRule, metricName string, isSummary bool) string {
	if isSummary {
		return tierExact
	}
	for _, r := range rules {
		if r.re.MatchString(metricName) {
			return r.tier
		}
	}
	return tierSketched
}
