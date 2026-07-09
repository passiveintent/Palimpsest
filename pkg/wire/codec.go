/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Marshal encodes f as a wire frame: fixed header, dict deltas, optional
// snapshot blob, payload, and a trailing CRC32C computed over every
// preceding byte. All multi-byte fields are little-endian.
func Marshal(f *Frame) ([]byte, error) {
	if f.Magic != Magic {
		return nil, fmt.Errorf("%w: bad magic %x", ErrInvalidFrame, f.Magic)
	}
	if f.Version != Version {
		return nil, fmt.Errorf("%w: Marshal requires version %d, got %d", ErrInvalidFrame, Version, f.Version)
	}
	for i, dd := range f.DictDeltas {
		if dd.IsTombstone() && (dd.InitValue != 0 || len(dd.Name) != 0) {
			return nil, fmt.Errorf("%w: dict_delta[%d] id=%d: tombstone must have zero init_value and empty name", ErrInvalidFrame, i, dd.ID)
		}
		if len(dd.Name) > math.MaxUint16 {
			return nil, fmt.Errorf("%w: dict_delta[%d] id=%d: name too long (%d bytes)", ErrInvalidFrame, i, dd.ID, len(dd.Name))
		}
	}
	if uint64(len(f.DictDeltas)) > math.MaxUint32 {
		return nil, fmt.Errorf("%w: too many dict_deltas (%d)", ErrInvalidFrame, len(f.DictDeltas))
	}
	if uint64(len(f.SnapshotBlob)) > math.MaxUint32 {
		return nil, fmt.Errorf("%w: snapshot_blob too large (%d bytes)", ErrInvalidFrame, len(f.SnapshotBlob))
	}
	if uint64(len(f.Payload)) > math.MaxUint32 {
		return nil, fmt.Errorf("%w: payload too large (%d bytes)", ErrInvalidFrame, len(f.Payload))
	}

	buf := make([]byte, 0, 96+len(f.Payload)+len(f.SnapshotBlob))
	buf = append(buf, f.Magic[:]...)
	buf = appendU8(buf, f.Version)
	buf = appendU8(buf, f.FrameType)
	buf = appendU8(buf, f.Flags)
	buf = appendU8(buf, f.Bits)
	buf = appendU64(buf, f.EmitterID)
	buf = appendU64(buf, f.ShardID)
	buf = appendU64(buf, f.Epoch)
	buf = appendU32(buf, f.Seq)
	buf = appendU16(buf, f.ViewID)
	buf = appendU32(buf, f.M)
	buf = appendU8(buf, f.D)
	buf = appendU8(buf, f.Predictor)
	buf = appendU8(buf, f.KeyVersion)
	buf = appendU8(buf, uint8(f.Codec))
	buf = appendF32(buf, f.Energy)
	buf = appendF32(buf, f.QuantScale)
	buf = appendU64(buf, f.DictRoot)

	buf = appendU32(buf, uint32(len(f.DictDeltas)))
	for _, dd := range f.DictDeltas {
		buf = appendU64(buf, dd.ID)
		buf = appendU8(buf, dd.Flags)
		buf = appendF32(buf, dd.InitValue)
		buf = appendU16(buf, uint16(len(dd.Name)))
		buf = append(buf, dd.Name...)
	}

	buf = appendU32(buf, uint32(len(f.SnapshotBlob)))
	buf = append(buf, f.SnapshotBlob...)

	buf = appendU32(buf, uint32(len(f.Payload)))
	buf = append(buf, f.Payload...)

	crc := crc32.Checksum(buf, crcTable)
	buf = appendU32(buf, crc)
	return buf, nil
}

// Unmarshal decodes a wire frame produced by Marshal. It never panics on
// malformed or truncated input; every field read is bounds-checked and
// errors are wrapped in ErrInvalidFrame / ErrShortBuffer / ErrCRCMismatch.
func Unmarshal(b []byte) (*Frame, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("%w: frame too short (%d bytes)", ErrShortBuffer, len(b))
	}
	body := b[:len(b)-4]
	crcWant := binary.LittleEndian.Uint32(b[len(b)-4:])
	crcGot := crc32.Checksum(body, crcTable)
	if crcWant != crcGot {
		return nil, fmt.Errorf("%w: want %08x got %08x", ErrCRCMismatch, crcWant, crcGot)
	}

	r := &reader{b: body}
	f := &Frame{}

	magic, err := r.bytes(4)
	if err != nil {
		return nil, err
	}
	copy(f.Magic[:], magic)
	if f.Magic != Magic {
		return nil, fmt.Errorf("%w: bad magic %x", ErrInvalidFrame, f.Magic)
	}

	if f.Version, err = r.u8(); err != nil {
		return nil, err
	}
	if f.Version < VersionMin || f.Version > Version {
		if f.Version > Version {
			return nil, fmt.Errorf("%w: got %d, max supported %d", ErrVersion, f.Version, Version)
		}
		return nil, fmt.Errorf("%w: unsupported version %d", ErrInvalidFrame, f.Version)
	}
	if f.FrameType, err = r.u8(); err != nil {
		return nil, err
	}
	if f.Flags, err = r.u8(); err != nil {
		return nil, err
	}
	if f.Bits, err = r.u8(); err != nil {
		return nil, err
	}
	if f.EmitterID, err = r.u64(); err != nil {
		return nil, err
	}
	if f.ShardID, err = r.u64(); err != nil {
		return nil, err
	}
	if f.Epoch, err = r.u64(); err != nil {
		return nil, err
	}
	if f.Seq, err = r.u32(); err != nil {
		return nil, err
	}
	if f.ViewID, err = r.u16(); err != nil {
		return nil, err
	}
	if f.M, err = r.u32(); err != nil {
		return nil, err
	}
	if f.D, err = r.u8(); err != nil {
		return nil, err
	}
	if f.Predictor, err = r.u8(); err != nil {
		return nil, err
	}
	// v2 adds key_version and codec where v1 had a single reserved byte.
	if f.Version == 1 {
		// Consume the v1 reserved byte; derive key_version=0 and codec from
		// flags.bit0 (FlagGzip) per SPEC.md §"Version compatibility".
		if _, err = r.u8(); err != nil {
			return nil, err
		}
		f.KeyVersion = 0
		if f.Flags&FlagGzip != 0 {
			f.Codec = CodecGzip
		} else {
			f.Codec = CodecNone
		}
	} else {
		if f.KeyVersion, err = r.u8(); err != nil {
			return nil, err
		}
		codecByte, err2 := r.u8()
		if err2 != nil {
			return nil, err2
		}
		f.Codec = Codec(codecByte)
	}
	if f.Energy, err = r.f32(); err != nil {
		return nil, err
	}
	if f.QuantScale, err = r.f32(); err != nil {
		return nil, err
	}
	if f.DictRoot, err = r.u64(); err != nil {
		return nil, err
	}

	ddCount, err := r.u32()
	if err != nil {
		return nil, err
	}
	// Bound the preallocation by remaining bytes (min dict_delta size is
	// 8+1+4+2=15 bytes) so a corrupt/malicious count can't force a huge
	// allocation before we've validated there's data to back it.
	f.DictDeltas = make([]DictDelta, 0, min(int(ddCount), r.remaining()/15+1))
	for i := uint32(0); i < ddCount; i++ {
		var dd DictDelta
		if dd.ID, err = r.u64(); err != nil {
			return nil, err
		}
		if dd.Flags, err = r.u8(); err != nil {
			return nil, err
		}
		if dd.InitValue, err = r.f32(); err != nil {
			return nil, err
		}
		nameLen, err := r.u16()
		if err != nil {
			return nil, err
		}
		name, err := r.bytes(int(nameLen))
		if err != nil {
			return nil, err
		}
		if len(name) > 0 {
			dd.Name = name
		}
		if dd.IsTombstone() && (dd.InitValue != 0 || len(dd.Name) != 0) {
			return nil, fmt.Errorf("%w: dict_delta[%d] id=%d: tombstone must have zero init_value and empty name", ErrInvalidFrame, i, dd.ID)
		}
		f.DictDeltas = append(f.DictDeltas, dd)
	}

	blobLen, err := r.u32()
	if err != nil {
		return nil, err
	}
	blob, err := r.bytes(int(blobLen))
	if err != nil {
		return nil, err
	}
	if len(blob) > 0 {
		f.SnapshotBlob = blob
	}

	payloadLen, err := r.u32()
	if err != nil {
		return nil, err
	}
	payload, err := r.bytes(int(payloadLen))
	if err != nil {
		return nil, err
	}
	if len(payload) > 0 {
		f.Payload = payload
	}

	if r.remaining() != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrInvalidFrame, r.remaining())
	}

	f.CRC32C = crcWant
	return f, nil
}

// reader is a bounds-checked cursor over an in-memory byte slice. Every
// method returns ErrShortBuffer instead of panicking when the requested
// field would run past the end of the buffer, so Unmarshal can safely
// process arbitrary/truncated/fuzzed input.
type reader struct {
	b   []byte
	pos int
}

func (r *reader) remaining() int { return len(r.b) - r.pos }

func (r *reader) need(n int) error {
	if n < 0 || r.remaining() < n {
		return fmt.Errorf("%w: need %d bytes, have %d", ErrShortBuffer, n, r.remaining())
	}
	return nil
}

// bytes returns a copy of the next n bytes so the returned Frame never
// aliases the caller's input buffer.
func (r *reader) bytes(n int) ([]byte, error) {
	if err := r.need(n); err != nil {
		return nil, err
	}
	out := append([]byte(nil), r.b[r.pos:r.pos+n]...)
	r.pos += n
	return out, nil
}

func (r *reader) u8() (uint8, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	v := r.b[r.pos]
	r.pos++
	return v, nil
}

func (r *reader) u16() (uint16, error) {
	if err := r.need(2); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint16(r.b[r.pos:])
	r.pos += 2
	return v, nil
}

func (r *reader) u32() (uint32, error) {
	if err := r.need(4); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint32(r.b[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *reader) u64() (uint64, error) {
	if err := r.need(8); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint64(r.b[r.pos:])
	r.pos += 8
	return v, nil
}

func (r *reader) f32() (float32, error) {
	v, err := r.u32()
	if err != nil {
		return 0, err
	}
	return math.Float32frombits(v), nil
}

func (r *reader) f64() (float64, error) {
	v, err := r.u64()
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(v), nil
}

func appendU8(b []byte, v uint8) []byte { return append(b, v) }

func appendU16(b []byte, v uint16) []byte {
	var tmp [2]byte
	binary.LittleEndian.PutUint16(tmp[:], v)
	return append(b, tmp[:]...)
}

func appendU32(b []byte, v uint32) []byte {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], v)
	return append(b, tmp[:]...)
}

func appendU64(b []byte, v uint64) []byte {
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], v)
	return append(b, tmp[:]...)
}

func appendF32(b []byte, v float32) []byte { return appendU32(b, math.Float32bits(v)) }

func appendF64(b []byte, v float64) []byte { return appendU64(b, math.Float64bits(v)) }
