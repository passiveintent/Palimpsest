/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package recover

import (
	"bytes"

	"github.com/cespare/xxhash/v2"
)

// Grouper assigns a group ID to a series for the group-LASSO proximal
// operator (ADR-014). All series sharing a group ID are shrunk together
// via block-soft-threshold, concentrating sparsity at the group level
// rather than the series level.
//
// A well-designed Grouper maps series from the same infrastructure failure
// domain (e.g. AZ, deployment, cluster) to the same group ID, so that a
// correlated incident — "all series in az-east-1 are anomalous" — appears
// 1-sparse at the group level even though it is dense at the series level.
//
// Grouper is called once per active series before RecoverGroup's FISTA loop;
// it must be deterministic and side-effect-free.
type Grouper func(seriesID uint64, dict *Dictionary) (groupID uint64)

// DefaultGrouper returns a Grouper that derives the group ID from the
// label segment of the series name — the portion between the first and
// last "|" delimiter in Palimpsest's logical-name format
// "metric|label1=v1,label2=v2|agg=sum". All series that share the same
// label set (i.e. the same deployment/AZ combination) are assigned to the
// same group, making a correlated AZ outage 1-sparse at the group level.
//
// Series whose name is absent from the dictionary, or whose name has fewer
// than two "|" delimiters (no label segment), fall back to a group ID
// derived from the series ID alone (each such series forms a singleton group).
func DefaultGrouper() Grouper {
	return func(seriesID uint64, dict *Dictionary) uint64 {
		entry, ok := dict.active[seriesID]
		if !ok {
			return seriesID
		}
		name := entry.name
		first := bytes.IndexByte(name, '|')
		if first < 0 {
			return seriesID
		}
		last := bytes.LastIndexByte(name, '|')
		if last <= first {
			return seriesID
		}
		return xxhash.Sum64(name[first+1 : last])
	}
}
