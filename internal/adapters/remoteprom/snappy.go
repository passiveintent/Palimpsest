package remoteprom

import "encoding/binary"

// snappyEncodeBlock encodes src as a Snappy "block format" stream
// (https://github.com/google/snappy/blob/main/format_description.txt),
// which is what Prometheus remote-write requires as its Content-Encoding.
//
// This is intentionally an all-literal encoder: it never emits Snappy's
// LZ77-style back-reference ("copy") elements, only the varint
// uncompressed-length preamble followed by one literal chunk. That is a
// fully spec-compliant Snappy stream — back-references are an optional
// space optimization, not a format requirement — any conformant decoder
// (including Prometheus's remote-write receiver) decodes it correctly. The
// tradeoff is compression ratio: since remoteprom's payloads are already
// quantized/small keyframe batches, and the guardrail here is "snappy +
// prompb only in this adapter, not core" (i.e. keep this self-contained
// and dependency-free) rather than "minimize bytes on the wire", that
// tradeoff is the right one for this adapter.
func snappyEncodeBlock(src []byte) []byte {
	dst := appendUvarint(nil, uint64(len(src)))
	if len(src) > 0 {
		dst = appendLiteralChunk(dst, src)
	}
	return dst
}

func appendUvarint(dst []byte, v uint64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	return append(dst, tmp[:n]...)
}

// appendLiteralChunk appends one Snappy literal element encoding all of
// lit: a tag byte (length-1 in the top 6 bits when it fits, else a marker
// plus 1-4 little-endian length bytes), followed by the raw bytes.
func appendLiteralChunk(dst, lit []byte) []byte {
	n := len(lit) - 1
	if n < 60 {
		dst = append(dst, byte(n<<2))
	} else {
		var extra []byte
		v := uint64(n)
		for v > 0 {
			extra = append(extra, byte(v))
			v >>= 8
		}
		dst = append(dst, byte((59+len(extra))<<2))
		dst = append(dst, extra...)
	}
	return append(dst, lit...)
}
