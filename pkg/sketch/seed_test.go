/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package sketch

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// TestHKDFRFC5869Vectors cross-checks the unexported hkdfExtract/hkdfExpand
// primitives against RFC 5869 Appendix A's SHA-256 known-answer vectors,
// independent of DeriveEphemeralSeed's own fixed info/salt shape.
func TestHKDFRFC5869Vectors(t *testing.T) {
	mustHex := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatalf("bad test hex %q: %v", s, err)
		}
		return b
	}

	cases := []struct {
		name      string
		ikm, salt []byte
		info      []byte
		l         int
		wantPRK   []byte
		wantOKM   []byte
	}{
		{
			name:    "RFC 5869 test case 1",
			ikm:     mustHex("0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b"),
			salt:    mustHex("000102030405060708090a0b0c"),
			info:    mustHex("f0f1f2f3f4f5f6f7f8f9"),
			l:       42,
			wantPRK: mustHex("077709362c2e32df0ddc3f0dc47bba6390b6c73bb50f9c3122ec844ad7c2b3e5"),
			wantOKM: mustHex("3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865"),
		},
		{
			name:    "RFC 5869 test case 3 (zero-length salt/info)",
			ikm:     mustHex("0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b"),
			salt:    nil,
			info:    nil,
			l:       42,
			wantPRK: mustHex("19ef24a32c717b167f33a91d6f648bdf96596776afdb6377ac434c1c293ccb04"),
			wantOKM: mustHex("8da4e775a563c18f715f802a063c5a31b8a11f5c5ee1879ec3454e5f3c738d2d9d201395faa4b61a96c8"),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prk := hkdfExtract(c.salt, c.ikm)
			if !bytes.Equal(prk, c.wantPRK) {
				t.Fatalf("PRK = %x, want %x", prk, c.wantPRK)
			}
			okm := hkdfExpand(prk, c.info, c.l)
			if !bytes.Equal(okm, c.wantOKM) {
				t.Fatalf("OKM = %x, want %x", okm, c.wantOKM)
			}
		})
	}
}

func TestDeriveEphemeralSeedDeterministic(t *testing.T) {
	tenantKey := []byte("tenant-secret-key-material")
	a := DeriveEphemeralSeed(tenantKey, 7, 42, 3)
	b := DeriveEphemeralSeed(tenantKey, 7, 42, 3)
	if a != b {
		t.Fatalf("DeriveEphemeralSeed not deterministic: %x != %x", a, b)
	}
}

func TestDeriveEphemeralSeedVariesWithInputs(t *testing.T) {
	base := DeriveEphemeralSeed([]byte("tenant-a"), 1, 1, 1)

	variants := []uint64{
		DeriveEphemeralSeed([]byte("tenant-b"), 1, 1, 1),
		DeriveEphemeralSeed([]byte("tenant-a"), 2, 1, 1),
		DeriveEphemeralSeed([]byte("tenant-a"), 1, 2, 1),
		DeriveEphemeralSeed([]byte("tenant-a"), 1, 1, 2),
	}
	for i, v := range variants {
		if v == base {
			t.Fatalf("variant %d matched base seed %x; expected a different seed when an input changes", i, base)
		}
	}
}
