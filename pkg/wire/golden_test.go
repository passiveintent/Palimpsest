/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package wire_test holds the cross-language golden vector tests as an
// external test package (not "package wire"): testGoldenFrame needs
// pkg/sketch (to derive series IDs the same way a real caller would), and
// pkg/sketch imports pkg/wire, so this file must live outside "wire" itself
// to avoid an import cycle in the test binary.
package wire_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strconv"
	"testing"

	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// TestGoldenVectors cross-checks pkg/wire against vectors produced by the
// Python conformance oracle (oracle/README.md, `make golden`):
// ComputeDictRoot (dictroot_vectors.json), Quantize/Dequantize
// (quant_vectors.json), the KEYFRAME codec including a full KDELTA replay
// (kdelta_sequence.json), and Marshal/Unmarshal of a complete frame with
// births, a tombstone, and a snapshot blob (frame_residual.bin).
//
// docs/adr/ADR-001: "Python feasibility harness promoted to conformance
// oracle generating golden vectors Go must match byte-exact."
func TestGoldenVectors(t *testing.T) {
	t.Run("dictroot", testGoldenDictRoot)
	t.Run("quant", testGoldenQuant)
	t.Run("kdelta", testGoldenKDelta)
	t.Run("frame", testGoldenFrame)
	t.Run("key_rotation", testGoldenKeyRotation)
}

type goldenDictRootCase struct {
	Description string   `json:"description"`
	IDs         []uint64 `json:"ids"`
	Root        uint64   `json:"root"`
}

type dictRootVectorsFile struct {
	Cases []goldenDictRootCase `json:"cases"`
}

func testGoldenDictRoot(t *testing.T) {
	var dv dictRootVectorsFile
	loadGoldenJSON(t, "dictroot_vectors.json", &dv)
	if len(dv.Cases) != 5 {
		t.Fatalf("want 5 dictroot_vectors cases, got %d", len(dv.Cases))
	}
	for _, c := range dv.Cases {
		if got := wire.ComputeDictRoot(c.IDs); got != c.Root {
			t.Errorf("%s: ComputeDictRoot = %#x, want %#x", c.Description, got, c.Root)
		}
	}
}

type goldenQuantCase struct {
	Description string    `json:"description"`
	Values      []float64 `json:"values"`
	Bits        uint8     `json:"bits"`
	Scale       float32   `json:"scale"`
	PackedHex   string    `json:"packed_hex"`
	Roundtrip   []float64 `json:"roundtrip"`
}

type quantVectorsFile struct {
	Cases []goldenQuantCase `json:"cases"`
}

func testGoldenQuant(t *testing.T) {
	var qv quantVectorsFile
	loadGoldenJSON(t, "quant_vectors.json", &qv)
	if len(qv.Cases) == 0 {
		t.Fatal("quant_vectors.json has no cases")
	}
	for _, c := range qv.Cases {
		wantPacked, err := hex.DecodeString(c.PackedHex)
		if err != nil {
			t.Fatalf("%s: bad packed_hex: %v", c.Description, err)
		}
		gotPacked, err := wire.Quantize(c.Values, c.Bits, c.Scale)
		if err != nil {
			t.Fatalf("%s: Quantize: %v", c.Description, err)
		}
		if !bytes.Equal(gotPacked, wantPacked) {
			t.Errorf("%s: Quantize = %x, want %x", c.Description, gotPacked, wantPacked)
		}

		gotRT, err := wire.Dequantize(wantPacked, uint32(len(c.Values)), c.Bits, c.Scale)
		if err != nil {
			t.Fatalf("%s: Dequantize: %v", c.Description, err)
		}
		if len(gotRT) != len(c.Roundtrip) {
			t.Fatalf("%s: Dequantize length = %d, want %d", c.Description, len(gotRT), len(c.Roundtrip))
		}
		for i := range gotRT {
			if gotRT[i] != c.Roundtrip[i] {
				t.Errorf("%s: Dequantize[%d] = %v, want %v", c.Description, i, gotRT[i], c.Roundtrip[i])
			}
		}
	}
}

type goldenKDeltaStep struct {
	Step        int                `json:"step"`
	Golden      bool               `json:"golden"`
	FlagsKDelta bool               `json:"flags_kdelta"`
	IDs         []uint64           `json:"ids"`
	Values      map[string]float64 `json:"values"`
	DictRoot    uint64             `json:"dict_root"`
	PayloadHex  string             `json:"payload_hex"`
}

type kdeltaSequenceFile struct {
	Scale float32            `json:"scale"`
	Steps []goldenKDeltaStep `json:"steps"`
}

func valuesToF32Map(t *testing.T, values map[string]float64) map[uint64]float32 {
	t.Helper()
	out := make(map[uint64]float32, len(values))
	for k, v := range values {
		id, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			t.Fatalf("bad id key %q: %v", k, err)
		}
		out[id] = float32(v)
	}
	return out
}

// testGoldenKDelta replays kdelta_sequence.json's golden keyframe followed
// by 3 KDELTA frames: each step's payload is decoded on top of the
// *previously decoded* state (never the oracle's own expected map), so this
// is exactly the "Unmarshal sequence of frames -> identical state" replay
// property ADR-011/ADR-013 require. It also re-encodes each step and checks
// the payload bytes match byte-for-byte.
func testGoldenKDelta(t *testing.T) {
	var seq kdeltaSequenceFile
	loadGoldenJSON(t, "kdelta_sequence.json", &seq)
	if len(seq.Steps) != 4 {
		t.Fatalf("want 4 kdelta_sequence steps, got %d", len(seq.Steps))
	}

	var replayed map[uint64]float32

	for _, step := range seq.Steps {
		wantPayload, err := hex.DecodeString(step.PayloadHex)
		if err != nil {
			t.Fatalf("step %d: bad payload_hex: %v", step.Step, err)
		}
		wantValues := valuesToF32Map(t, step.Values)

		if root := wire.ComputeDictRoot(step.IDs); root != step.DictRoot {
			t.Fatalf("step %d: ComputeDictRoot(ids) = %#x, want %#x", step.Step, root, step.DictRoot)
		}

		// Decode on top of the replay state accumulated from prior steps
		// (nil prev + kdelta=false for the opening golden frame).
		decoded, err := wire.DecodeKeyframe(wantPayload, step.FlagsKDelta, step.IDs, replayed, seq.Scale)
		if err != nil {
			t.Fatalf("step %d: DecodeKeyframe: %v", step.Step, err)
		}
		if !reflect.DeepEqual(decoded, wantValues) {
			t.Fatalf("step %d: decoded values = %+v, want %+v", step.Step, decoded, wantValues)
		}

		// Re-encode against the same prior state and check byte-exact
		// agreement with the oracle's payload.
		gotPayload, flags, err := wire.EncodeKeyframe(step.IDs, wantValues, replayed, step.Golden, seq.Scale)
		if err != nil {
			t.Fatalf("step %d: EncodeKeyframe: %v", step.Step, err)
		}
		wantFlags := uint8(0)
		if step.FlagsKDelta {
			wantFlags = wire.FlagKDelta
		}
		if flags != wantFlags {
			t.Fatalf("step %d: EncodeKeyframe flags = %#x, want %#x", step.Step, flags, wantFlags)
		}
		if !bytes.Equal(gotPayload, wantPayload) {
			t.Fatalf("step %d: EncodeKeyframe payload = %x, want %x", step.Step, gotPayload, wantPayload)
		}

		replayed = decoded
	}
}

// testGoldenFrame builds the exact Frame the oracle generated (same literal
// name strings / parameters as oracle/gen_golden.py's
// build_frame_residual), and checks Marshal reproduces frame_residual.bin
// byte-for-byte, and Unmarshal(frame_residual.bin) reproduces the same
// Frame. IDs, dict_root, and the quantized payload are computed here via
// Go's own implementation from shared literal inputs -- not copied numbers
// -- so this test cannot pass by transcription accident.
func testGoldenFrame(t *testing.T) {
	const (
		birth1Name    = "checkout_latency_ms|region=us-east,cluster=prod|agg=sum"
		birth2Name    = "checkout_latency_ms|region=us-west,cluster=prod|agg=sum"
		tombstoneName = "checkout_latency_ms|region=eu-central,cluster=prod|agg=sum"
	)
	birth1ID := sketch.SeriesID([]byte(birth1Name))
	birth2ID := sketch.SeriesID([]byte(birth2Name))
	tombstoneID := sketch.SeriesID([]byte(tombstoneName))

	activeIDs := []uint64{birth1ID, birth2ID}
	sort.Slice(activeIDs, func(i, j int) bool { return activeIDs[i] < activeIDs[j] })
	dictRoot := wire.ComputeDictRoot(activeIDs)

	snapshotBlob, err := wire.EncodeSnapshot([]wire.SnapshotEntry{
		{ID: birth1ID, TSMs: 1750000000000, Value: 13.1},
		{ID: birth1ID, TSMs: 1750000005000, Value: 12.9},
	}, wire.CodecNone)
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}

	const m = 64
	values := make([]float64, m)
	for i := range values {
		values[i] = float64(i-32) * 0.25
	}
	payload, err := wire.Quantize(values, 8, 0.25)
	if err != nil {
		t.Fatalf("Quantize: %v", err)
	}

	f := &wire.Frame{
		Magic:      wire.Magic,
		Version:    wire.Version,
		FrameType:  wire.FrameTypeResidual,
		Flags:      0,
		Bits:       8,
		EmitterID:  0x1122334455667788,
		ShardID:    7,
		Epoch:      3,
		Seq:        42,
		ViewID:     1,
		M:          m,
		D:          4,
		Predictor:  uint8(wire.PredictorHold),
		KeyVersion: 0,
		Codec:      wire.CodecNone,
		Energy:     1.25,
		QuantScale: 0.25,
		DictRoot:   dictRoot,
		DictDeltas: []wire.DictDelta{
			{ID: birth1ID, Flags: 0, InitValue: 12.5, Name: []byte(birth1Name)},
			{ID: birth2ID, Flags: 0, InitValue: 9.75, Name: []byte(birth2Name)},
			{ID: tombstoneID, Flags: wire.DictFlagTombstone, InitValue: 0, Name: nil},
		},
		SnapshotBlob: snapshotBlob,
		Payload:      payload,
	}

	want, err := os.ReadFile("../../testdata/golden/frame_residual.bin")
	if err != nil {
		t.Skipf("no golden vectors at testdata/golden/frame_residual.bin: %v (run `make golden`)", err)
	}

	got, err := wire.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Marshal(f) does not match frame_residual.bin byte-for-byte\ngot:  %x\nwant: %x", got, want)
	}

	decoded, err := wire.Unmarshal(want)
	if err != nil {
		t.Fatalf("Unmarshal(frame_residual.bin): %v", err)
	}
	f.CRC32C = decoded.CRC32C
	if !reflect.DeepEqual(decoded, f) {
		t.Fatalf("Unmarshal(frame_residual.bin) = %+v, want %+v", decoded, f)
	}
}

func loadGoldenJSON(t *testing.T, filename string, v interface{}) {
	t.Helper()
	const dir = "../../testdata/golden"
	path := dir + "/" + filename
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no golden vectors at %s: %v (run `make golden`)", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
}

// goldenKeyRotationFrame is one entry in key_rotation_vectors.json.
type goldenKeyRotationFrame struct {
	KeyVersion uint8     `json:"key_version"`
	TenantKey  string    `json:"tenant_key"`
	Seed       uint64    `json:"seed"`
	FrameHex   string    `json:"frame_hex"`
	Y          []float64 `json:"y"`
	QuantScale float32   `json:"quant_scale"`
}

type keyRotationVectorsFile struct {
	ShardID    uint64                   `json:"shard_id"`
	EpochIdx   uint32                   `json:"epoch_idx"`
	ViewID     uint16                   `json:"view_id"`
	Seq        uint32                   `json:"seq"`
	M          uint32                   `json:"m"`
	D          uint8                    `json:"d"`
	Bits       uint8                    `json:"bits"`
	SeriesName string                   `json:"series_name"`
	SeriesID   uint64                   `json:"series_id"`
	Frames     []goldenKeyRotationFrame `json:"frames"`
}

// testGoldenKeyRotation verifies that a key_rotation_vectors.json entry
// round-trips correctly: each frame must Unmarshal with its declared
// key_version preserved, and the re-marshaled bytes must be byte-identical
// to the oracle's bytes.
func testGoldenKeyRotation(t *testing.T) {
	var kv keyRotationVectorsFile
	loadGoldenJSON(t, "key_rotation_vectors.json", &kv)
	if len(kv.Frames) != 2 {
		t.Fatalf("key_rotation_vectors.json: want 2 frames (key_version 0 and 1), got %d", len(kv.Frames))
	}

	for _, entry := range kv.Frames {
		wantBytes, err := hex.DecodeString(entry.FrameHex)
		if err != nil {
			t.Fatalf("key_version %d: bad frame_hex: %v", entry.KeyVersion, err)
		}

		f, err := wire.Unmarshal(wantBytes)
		if err != nil {
			t.Fatalf("key_version %d: Unmarshal: %v", entry.KeyVersion, err)
		}
		if f.Version != wire.Version {
			t.Errorf("key_version %d: frame.Version = %d, want %d", entry.KeyVersion, f.Version, wire.Version)
		}
		if f.KeyVersion != entry.KeyVersion {
			t.Errorf("key_version %d: frame.KeyVersion = %d, want %d", entry.KeyVersion, f.KeyVersion, entry.KeyVersion)
		}

		// Verify seed matches oracle's derive_ephemeral_seed.
		gotSeed := sketch.DeriveEphemeralSeed([]byte(entry.TenantKey), kv.ShardID, kv.EpochIdx, kv.ViewID)
		if gotSeed != entry.Seed {
			t.Errorf("key_version %d: DeriveEphemeralSeed = %d, want %d", entry.KeyVersion, gotSeed, entry.Seed)
		}

		// Re-marshal must be byte-identical.
		gotBytes, err := wire.Marshal(f)
		if err != nil {
			t.Fatalf("key_version %d: re-Marshal: %v", entry.KeyVersion, err)
		}
		if !bytes.Equal(gotBytes, wantBytes) {
			t.Errorf("key_version %d: re-Marshal bytes differ\ngot:  %x\nwant: %x", entry.KeyVersion, gotBytes, wantBytes)
		}
	}
}
