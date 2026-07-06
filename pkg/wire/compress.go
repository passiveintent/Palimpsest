/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrUnregisteredCodec indicates a frame named a Codec (Frame.Codec) that
// has no Compressor registered for it (Register) — most commonly CodecZstd
// before its Compressor has been Register-ed by a caller outside pkg/*
// (ADR-007: pkg/* stays stdlib+xxhash only, so zstd is never registered by
// pkg/wire itself). Callers decoding a frame should treat this the same as
// any other decode failure: gate the stream at low confidence and count it,
// never silently drop it (see ADR-006 §Addendum, ADR-012 §Addendum's
// identical policy for an unknown key_version).
var ErrUnregisteredCodec = errors.New("wire: unregistered codec")

// Compressor implements one Codec's compress/decompress pair. Register
// installs a Compressor for a Codec value; CompressPayload/DecompressPayload
// (and EncodeSnapshot/DecodeSnapshot, and any caller compressing a KEYFRAME
// payload) look it up by Frame.Codec.
//
// Implementations must be safe for concurrent use: the registry is a single
// process-wide map and encode/decode paths call through it from multiple
// goroutines (one per shard/pipeline) without additional synchronization.
type Compressor interface {
	// Compress returns plain, compressed. It must not retain a reference
	// to plain beyond the call.
	Compress(plain []byte) ([]byte, error)
	// Decompress reverses Compress. Implementations must bound the
	// decompressed size to maxBytes (inclusive) and return an error
	// rather than allocate without bound — compressed is untrusted input
	// (a frame read off the wire), so a small malicious input must never
	// be able to force an unbounded allocation ("zip bomb").
	Decompress(compressed []byte, maxBytes int) ([]byte, error)
}

var (
	registryMu sync.RWMutex
	registry   = map[Codec]Compressor{
		CodecNone: noneCompressor{},
		CodecGzip: gzipCompressor{},
	}
)

// Register installs comp as the Compressor for c, replacing any previously
// registered Compressor for the same value. Intended to be called once at
// process startup (e.g. palimpsestd's main, the otel csresidual processor
// factory) before any frame is encoded or decoded — CodecNone and CodecGzip
// are pre-registered by pkg/wire itself; CodecZstd (and any operator-defined
// codec) must be Register-ed by the caller (ADR-007: pkg/wire has no zstd
// dependency of its own).
//
// Safe for concurrent use, but registering the same Codec from multiple
// goroutines without external coordination leaves last-write-wins
// semantics — callers should register during startup, before any encode/
// decode traffic begins.
func Register(c Codec, comp Compressor) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[c] = comp
}

// lookup returns the Compressor registered for c, or (nil, false) if none
// is registered.
func lookup(c Codec) (Compressor, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	comp, ok := registry[c]
	return comp, ok
}

// CompressPayload compresses plain using the Compressor registered for
// codec, returning ErrUnregisteredCodec if none is registered.
func CompressPayload(codec Codec, plain []byte) ([]byte, error) {
	comp, ok := lookup(codec)
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrUnregisteredCodec, codec)
	}
	out, err := comp.Compress(plain)
	if err != nil {
		return nil, fmt.Errorf("wire: compress (codec %d): %w", codec, err)
	}
	return out, nil
}

// DecompressPayload reverses CompressPayload, bounding the decompressed
// size to maxBytes. Returns ErrUnregisteredCodec if codec has no
// registered Compressor.
func DecompressPayload(codec Codec, compressed []byte, maxBytes int) ([]byte, error) {
	comp, ok := lookup(codec)
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrUnregisteredCodec, codec)
	}
	out, err := comp.Decompress(compressed, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: decompress (codec %d): %v", ErrInvalidFrame, codec, err)
	}
	return out, nil
}

// noneCompressor implements CodecNone: an identity pass-through.
type noneCompressor struct{}

func (noneCompressor) Compress(plain []byte) ([]byte, error) { return plain, nil }

func (noneCompressor) Decompress(compressed []byte, maxBytes int) ([]byte, error) {
	if len(compressed) > maxBytes {
		return nil, fmt.Errorf("%d bytes exceeds max %d", len(compressed), maxBytes)
	}
	return compressed, nil
}

// gzipCompressor implements CodecGzip via compress/gzip (stdlib).
type gzipCompressor struct{}

func (gzipCompressor) Compress(plain []byte) ([]byte, error) {
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	if _, err := gw.Write(plain); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (gzipCompressor) Decompress(compressed []byte, maxBytes int) ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	decoded, err := io.ReadAll(io.LimitReader(gr, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(decoded) > maxBytes {
		return nil, fmt.Errorf("decompresses to more than %d bytes", maxBytes)
	}
	return decoded, nil
}
