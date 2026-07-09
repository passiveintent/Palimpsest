/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package csresidual

import (
	"fmt"

	"github.com/passiveintent/Palimpsest/pkg/wire"
	"github.com/passiveintent/Palimpsest/zstdcodec"
)

// init registers zstdcodec's Compressor as wire.CodecZstd's implementation
// once for this process (ADR-006 §Addendum, ADR-007: pkg/wire has no zstd
// dependency of its own, so any binary that wants to encode/decode
// CodecZstd frames must Register one at startup — this processor always
// does, unconditionally, regardless of whether compression.codec is
// actually set to "zstd" in any given config, so decoding a CodecZstd frame
// from a peer never fails here just because the local config chose gzip).
func init() {
	if err := zstdcodec.Register(); err != nil {
		// New() only fails on an internal klauspost/compress construction
		// error (never on operator input) — a condition worth failing
		// loudly for rather than silently leaving CodecZstd unregistered.
		panic(fmt.Sprintf("csresidual: registering zstd codec: %v", err))
	}
}

// resolveCodec maps a CompressionConfig.Codec string (already validated by
// Config.Validate) to its wire.Codec value.
func resolveCodec(name string) (wire.Codec, error) {
	switch name {
	case "", "none":
		return wire.CodecNone, nil
	case "gzip":
		return wire.CodecGzip, nil
	case "zstd":
		return wire.CodecZstd, nil
	default:
		return 0, fmt.Errorf("csresidual: unknown compression.codec %q", name)
	}
}
