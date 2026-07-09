/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package wire

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// frameFieldWireNames maps every wire.Frame Go field to the exact byte-table
// token docs/rfc/palimpsest-wire-v2.md's "Frame struct field cross-reference"
// table must contain for it. A new Frame field with no entry here fails this
// test loudly instead of letting the RFC -- the frozen protocol document --
// silently fall out of sync with the code.
var frameFieldWireNames = map[string]string{
	"Magic":        "`Magic`",
	"Version":      "`Version`",
	"FrameType":    "`FrameType`",
	"Flags":        "`Flags`",
	"Bits":         "`Bits`",
	"EmitterID":    "`EmitterID`",
	"ShardID":      "`ShardID`",
	"Epoch":        "`Epoch`",
	"Seq":          "`Seq`",
	"ViewID":       "`ViewID`",
	"M":            "`M`",
	"D":            "`D`",
	"Predictor":    "`Predictor`",
	"KeyVersion":   "`KeyVersion`",
	"Codec":        "`Codec`",
	"Energy":       "`Energy`",
	"QuantScale":   "`QuantScale`",
	"DictRoot":     "`DictRoot`",
	"DictDeltas":   "`DictDeltas`",
	"SnapshotBlob": "`SnapshotBlob`",
	"Payload":      "`Payload`",
	"CRC32C":       "`CRC32C`",
}

// TestRFCFrameTableInSync fails if a Frame struct field is ever added,
// removed, or renamed without updating docs/rfc/palimpsest-wire-v2.md's
// byte table to match (PROMPT 16 acceptance criterion): every field in
// wire.Frame must appear in frameFieldWireNames (else the RFC has nothing
// documenting it), and its mapped token must actually appear in the RFC's
// "Frame struct field cross-reference" table.
func TestRFCFrameTableInSync(t *testing.T) {
	rfcPath := filepath.Join("..", "..", "docs", "rfc", "palimpsest-wire-v2.md")
	b, err := os.ReadFile(rfcPath)
	if err != nil {
		t.Fatalf("reading RFC %s: %v", rfcPath, err)
	}
	rfc := string(b)

	typ := reflect.TypeOf(Frame{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		token, ok := frameFieldWireNames[name]
		if !ok {
			t.Fatalf("wire.Frame field %q has no entry in frameFieldWireNames (rfc_sync_test.go); "+
				"add one and document the field in %s's byte table", name, rfcPath)
		}
		if !strings.Contains(rfc, token) {
			t.Fatalf("wire.Frame field %q (expected token %s) is missing from %s's "+
				"Frame struct field cross-reference table", name, token, rfcPath)
		}
	}

	// Catch the opposite drift too: an entry in the map for a field that no
	// longer exists on Frame (renamed/removed) would otherwise silently
	// stop being checked at all.
	fields := make(map[string]bool, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		fields[typ.Field(i).Name] = true
	}
	for name := range frameFieldWireNames {
		if !fields[name] {
			t.Fatalf("frameFieldWireNames has an entry for %q, which is no longer a field of wire.Frame; remove it", name)
		}
	}
}
