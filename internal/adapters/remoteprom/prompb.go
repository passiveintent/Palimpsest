/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package remoteprom

import (
	"encoding/binary"
	"math"
)

// This file hand-encodes the small slice of the Prometheus remote-write
// protobuf schema (prompb.WriteRequest) this adapter needs, using the
// protobuf wire format directly (varint tags/lengths, no generated code,
// no dependency): see the "snappy + prompb only in service module, kept
// self-contained" guardrail on internal/adapters/remoteprom's parent
// package (internal/core stays stdlib+xxhash only; this adapter is
// stdlib-only too, just implementing the wire format by hand instead of
// pulling in google.golang.org/protobuf + prometheus/prometheus).
//
//	message WriteRequest { repeated TimeSeries timeseries = 1; }
//	message TimeSeries   { repeated Label labels = 1; repeated Sample samples = 2; }
//	message Label        { string name = 1; string value = 2; }
//	message Sample       { double value = 1; int64 timestamp = 2; }

const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
)

// pbBuf accumulates a protobuf message's encoded bytes.
type pbBuf struct{ b []byte }

func (w *pbBuf) tag(fieldNum, wireType int) {
	w.varint(uint64(fieldNum)<<3 | uint64(wireType))
}

func (w *pbBuf) varint(v uint64) {
	for v >= 0x80 {
		w.b = append(w.b, byte(v)|0x80)
		v >>= 7
	}
	w.b = append(w.b, byte(v))
}

func (w *pbBuf) bytesField(fieldNum int, b []byte) {
	w.tag(fieldNum, wireBytes)
	w.varint(uint64(len(b)))
	w.b = append(w.b, b...)
}

func (w *pbBuf) stringField(fieldNum int, s string) { w.bytesField(fieldNum, []byte(s)) }

func (w *pbBuf) doubleField(fieldNum int, v float64) {
	w.tag(fieldNum, wireFixed64)
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], math.Float64bits(v))
	w.b = append(w.b, tmp[:]...)
}

func (w *pbBuf) int64Field(fieldNum int, v int64) {
	w.tag(fieldNum, wireVarint)
	w.varint(uint64(v))
}

// promLabel and promSample are the plain-Go inputs this file's encode*
// functions turn into protobuf bytes.
type promLabel struct{ Name, Value string }
type promSample struct {
	Value       float64
	TimestampMs int64
}

func encodeLabel(l promLabel) []byte {
	w := &pbBuf{}
	w.stringField(1, l.Name)
	w.stringField(2, l.Value)
	return w.b
}

func encodeSample(s promSample) []byte {
	w := &pbBuf{}
	w.doubleField(1, s.Value)
	w.int64Field(2, s.TimestampMs)
	return w.b
}

// encodeTimeSeries encodes one TimeSeries message. labels must already be
// sorted by Name (Prometheus remote-write requires lexicographically
// sorted labels per series).
func encodeTimeSeries(labels []promLabel, samples []promSample) []byte {
	w := &pbBuf{}
	for _, l := range labels {
		w.bytesField(1, encodeLabel(l))
	}
	for _, s := range samples {
		w.bytesField(2, encodeSample(s))
	}
	return w.b
}

// encodeWriteRequest encodes a WriteRequest from pre-encoded TimeSeries
// messages.
func encodeWriteRequest(series [][]byte) []byte {
	w := &pbBuf{}
	for _, ts := range series {
		w.bytesField(1, ts)
	}
	return w.b
}
