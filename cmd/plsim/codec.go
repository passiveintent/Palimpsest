/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package main

import (
	"fmt"

	"github.com/passiveintent/Palimpsest/pkg/wire"
	"github.com/passiveintent/Palimpsest/zstdcodec"
)

// init registers zstdcodec's Compressor as wire.CodecZstd's implementation
// (ADR-006 §Addendum, ADR-007: pkg/wire has no zstd dependency of its own),
// mirroring otel/processor/csresidual's identical registration so --codec
// zstd works regardless of which encoder produced the run's frames.
func init() {
	if err := zstdcodec.Register(); err != nil {
		panic(fmt.Sprintf("plsim: registering zstd codec: %v", err))
	}
}

// resolveCodec maps --codec's string value to its wire.Codec.
func resolveCodec(name string) (wire.Codec, error) {
	switch name {
	case "", "none":
		return wire.CodecNone, nil
	case "gzip":
		return wire.CodecGzip, nil
	case "zstd":
		return wire.CodecZstd, nil
	default:
		return 0, fmt.Errorf("--codec: unknown value %q (want none, gzip, or zstd)", name)
	}
}
