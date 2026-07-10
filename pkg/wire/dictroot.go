/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import (
	"encoding/binary"
	"sort"

	"github.com/cespare/xxhash/v2"
)

// ComputeDictRoot returns the ADR-008 dict_root digest for a set of
// active dictionary IDs: xxh64(sorted ids serialized little-endian u64,
// seed=0). The result is independent of input order.
func ComputeDictRoot(ids []uint64) uint64 {
	sorted := append([]uint64(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	buf := make([]byte, len(sorted)*8)
	for i, id := range sorted {
		binary.LittleEndian.PutUint64(buf[i*8:], id)
	}
	return xxhash.Sum64(buf)
}
