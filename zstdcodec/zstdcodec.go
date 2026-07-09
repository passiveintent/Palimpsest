/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package zstdcodec implements wire.Compressor for wire.CodecZstd using
// github.com/klauspost/compress/zstd. It deliberately lives outside pkg/*
// (ADR-007: pkg/* stays stdlib+xxhash only) — this package is the "new leaf
// module or the service module" ADR-006 §Addendum allows for the zstd
// dependency, imported by cmd/palimpsestd and the otel/processor/csresidual
// factory, each of which calls Register at process startup.
package zstdcodec

import (
	"fmt"

	"github.com/klauspost/compress/zstd"

	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// maxDecoderMemory bounds a single Decoder's internal allocation regardless
// of what a compressed frame's header claims its decoded size will be — an
// absolute safety ceiling in front of Decompress's own maxBytes check,
// which only runs after the bytes already exist. 512 MiB is generously
// above any realistic KEYFRAME payload or snapshot blob (docs/PERF.md: a
// 50k-series golden keyframe is ~600 KB raw).
const maxDecoderMemory = 512 << 20

// Compressor implements wire.Compressor for zstd. The zero value is not
// usable; construct with New.
type Compressor struct {
	enc *zstd.Encoder
	dec *zstd.Decoder
}

// New returns a Compressor ready for concurrent Compress/Decompress calls
// (both EncodeAll and DecodeAll, which back them, are documented safe for
// concurrent use — see zstdcodec_test.go's TestCompressorConcurrent).
func New() (*Compressor, error) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, fmt.Errorf("zstdcodec: new encoder: %w", err)
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(maxDecoderMemory))
	if err != nil {
		enc.Close()
		return nil, fmt.Errorf("zstdcodec: new decoder: %w", err)
	}
	return &Compressor{enc: enc, dec: dec}, nil
}

// Compress implements wire.Compressor.
func (c *Compressor) Compress(plain []byte) ([]byte, error) {
	return c.enc.EncodeAll(plain, nil), nil
}

// Decompress implements wire.Compressor. It bounds the decoded size to
// maxBytes on top of the Decoder's own maxDecoderMemory ceiling (New).
func (c *Compressor) Decompress(compressed []byte, maxBytes int) ([]byte, error) {
	out, err := c.dec.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("zstdcodec: decode: %w", err)
	}
	if len(out) > maxBytes {
		return nil, fmt.Errorf("zstdcodec: decompresses to more than %d bytes", maxBytes)
	}
	return out, nil
}

// Register constructs a Compressor and installs it as wire's CodecZstd
// implementation (wire.Register). Call once at process startup — before
// any frame carrying CodecZstd is encoded or decoded — from cmd/palimpsestd
// and the otel csresidual processor factory.
func Register() error {
	c, err := New()
	if err != nil {
		return err
	}
	wire.Register(wire.CodecZstd, c)
	return nil
}
