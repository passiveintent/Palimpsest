package sketch

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// DeriveEphemeralSeed derives the per-(tenant, shard, epoch, view) sketch
// seed via HKDF-SHA256 (RFC 5869), truncated to a uint64 (docs/SPEC.md
// "Hashing & Phi"; ADR-012: "Seeds derive via HKDF-SHA256(tenant_key, …),
// not public convention").
//
// tenantKey is the HKDF input keying material (secret; never derived from
// public/predictable data per ADR-012); shardID, epochIdx, and viewID are
// mixed in as HKDF "info" context, so the same tenant key yields
// independent, non-comparable seeds across shards/epochs/views — sketches
// from different (shard, epoch, view) coordinate systems never
// accidentally align. The result is deterministic: identical inputs always
// yield the identical seed, which is required for an encoder and its
// decoder(s) to agree on Phi without exchanging it out of band.
func DeriveEphemeralSeed(tenantKey []byte, shardID uint64, epochIdx uint32, viewID uint16) uint64 {
	var info [8 + 4 + 2]byte
	binary.BigEndian.PutUint64(info[0:8], shardID)
	binary.BigEndian.PutUint32(info[8:12], epochIdx)
	binary.BigEndian.PutUint16(info[12:14], viewID)

	prk := hkdfExtract(nil, tenantKey)
	okm := hkdfExpand(prk, info[:], 8)
	return binary.BigEndian.Uint64(okm)
}

// hkdfExtract implements the RFC 5869 "extract" step: PRK = HMAC-Hash(salt,
// IKM). A nil/empty salt is replaced with HashLen zero bytes per the RFC.
func hkdfExtract(salt, ikm []byte) []byte {
	if len(salt) == 0 {
		salt = make([]byte, sha256.Size)
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write(ikm)
	return mac.Sum(nil)
}

// hkdfExpand implements the RFC 5869 "expand" step: OKM = T(1) || T(2) ||
// … truncated to length bytes, where T(i) = HMAC-Hash(PRK, T(i-1) || info
// || i). length must not exceed 255*sha256.Size (unused here since we only
// ever request 8 bytes).
func hkdfExpand(prk, info []byte, length int) []byte {
	okm := make([]byte, 0, length)
	var t []byte
	for counter := byte(1); len(okm) < length; counter++ {
		mac := hmac.New(sha256.New, prk)
		mac.Write(t)
		mac.Write(info)
		mac.Write([]byte{counter})
		t = mac.Sum(nil)
		okm = append(okm, t...)
	}
	return okm[:length]
}
