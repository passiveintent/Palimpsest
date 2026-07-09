/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package remoteprom

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/passiveintent/Palimpsest/internal/ports"
)

// The functions below are a minimal, test-only decoder for the same
// literal-only Snappy block format and protobuf subset snappy.go/prompb.go
// encode, so this file can verify round-trip correctness without pulling
// in any decoding dependency either.

type pbReader struct {
	b   []byte
	pos int
}

func (r *pbReader) done() bool { return r.pos >= len(r.b) }

func (r *pbReader) readVarint() (uint64, error) {
	var x uint64
	var s uint
	for {
		if r.pos >= len(r.b) {
			return 0, io.ErrUnexpectedEOF
		}
		b := r.b[r.pos]
		r.pos++
		if b < 0x80 {
			return x | uint64(b)<<s, nil
		}
		x |= uint64(b&0x7f) << s
		s += 7
	}
}

func (r *pbReader) readTag() (fieldNum, wireType int, err error) {
	v, err := r.readVarint()
	if err != nil {
		return 0, 0, err
	}
	return int(v >> 3), int(v & 0x7), nil
}

func (r *pbReader) readBytesField() ([]byte, error) {
	n, err := r.readVarint()
	if err != nil {
		return nil, err
	}
	if r.pos+int(n) > len(r.b) {
		return nil, io.ErrUnexpectedEOF
	}
	out := r.b[r.pos : r.pos+int(n)]
	r.pos += int(n)
	return out, nil
}

func (r *pbReader) readFixed64() (uint64, error) {
	if r.pos+8 > len(r.b) {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.LittleEndian.Uint64(r.b[r.pos:])
	r.pos += 8
	return v, nil
}

func decodeLabel(b []byte) (promLabel, error) {
	r := &pbReader{b: b}
	var l promLabel
	for !r.done() {
		fn, _, err := r.readTag()
		if err != nil {
			return l, err
		}
		v, err := r.readBytesField()
		if err != nil {
			return l, err
		}
		switch fn {
		case 1:
			l.Name = string(v)
		case 2:
			l.Value = string(v)
		}
	}
	return l, nil
}

func decodeSample(b []byte) (promSample, error) {
	r := &pbReader{b: b}
	var s promSample
	for !r.done() {
		fn, _, err := r.readTag()
		if err != nil {
			return s, err
		}
		switch fn {
		case 1:
			v, err := r.readFixed64()
			if err != nil {
				return s, err
			}
			s.Value = math.Float64frombits(v)
		case 2:
			v, err := r.readVarint()
			if err != nil {
				return s, err
			}
			s.TimestampMs = int64(v)
		}
	}
	return s, nil
}

func decodeTimeSeries(b []byte) ([]promLabel, []promSample, error) {
	r := &pbReader{b: b}
	var labels []promLabel
	var samples []promSample
	for !r.done() {
		fn, _, err := r.readTag()
		if err != nil {
			return nil, nil, err
		}
		v, err := r.readBytesField()
		if err != nil {
			return nil, nil, err
		}
		switch fn {
		case 1:
			l, err := decodeLabel(v)
			if err != nil {
				return nil, nil, err
			}
			labels = append(labels, l)
		case 2:
			s, err := decodeSample(v)
			if err != nil {
				return nil, nil, err
			}
			samples = append(samples, s)
		}
	}
	return labels, samples, nil
}

func decodeWriteRequest(b []byte) ([][]byte, error) {
	r := &pbReader{b: b}
	var series [][]byte
	for !r.done() {
		fn, _, err := r.readTag()
		if err != nil {
			return nil, err
		}
		v, err := r.readBytesField()
		if err != nil {
			return nil, err
		}
		if fn == 1 {
			series = append(series, v)
		}
	}
	return series, nil
}

// decodeSnappyBlock decodes a block produced by snappyEncodeBlock. It
// rejects any non-literal (copy) element, since that encoder never emits
// one.
func decodeSnappyBlock(b []byte) ([]byte, error) {
	r := &pbReader{b: b}
	n, err := r.readVarint()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, n)
	for r.pos < len(b) {
		tag := b[r.pos]
		r.pos++
		if tag&0x3 != 0 {
			return nil, fmt.Errorf("unexpected non-literal snappy tag %#x", tag)
		}
		lenMinus1 := int(tag >> 2)
		if lenMinus1 >= 60 {
			nExtra := lenMinus1 - 59
			if r.pos+nExtra > len(b) {
				return nil, io.ErrUnexpectedEOF
			}
			var v int
			for i := 0; i < nExtra; i++ {
				v |= int(b[r.pos+i]) << (8 * i)
			}
			r.pos += nExtra
			lenMinus1 = v
		}
		length := lenMinus1 + 1
		if r.pos+length > len(b) {
			return nil, io.ErrUnexpectedEOF
		}
		out = append(out, b[r.pos:r.pos+length]...)
		r.pos += length
	}
	if uint64(len(out)) != n {
		return nil, fmt.Errorf("decoded length %d != preamble %d", len(out), n)
	}
	return out, nil
}

func TestSnappyEncodeBlock_RoundTrip(t *testing.T) {
	for _, src := range [][]byte{
		nil,
		{},
		[]byte("x"),
		[]byte("hello, world"),
		make([]byte, 1000),  // all zero, exercises the >=60-byte literal path
		make([]byte, 70000), // exercises the 2-extra-byte length path
	} {
		got, err := decodeSnappyBlock(snappyEncodeBlock(src))
		if err != nil {
			t.Fatalf("len(src)=%d: decode: %v", len(src), err)
		}
		if len(got) != len(src) {
			t.Fatalf("len(src)=%d: round-trip length = %d", len(src), len(got))
		}
	}
}

func TestEncodeTimeSeries_RoundTrip(t *testing.T) {
	labels := []promLabel{{Name: "__name__", Value: "test_metric"}, {Name: "shard", Value: "1"}}
	samples := []promSample{{Value: 3.5, TimestampMs: 123456}}

	tsBytes := encodeTimeSeries(labels, samples)
	gotLabels, gotSamples, err := decodeTimeSeries(tsBytes)
	if err != nil {
		t.Fatalf("decodeTimeSeries: %v", err)
	}
	if len(gotLabels) != 2 || gotLabels[0] != labels[0] || gotLabels[1] != labels[1] {
		t.Fatalf("labels = %+v, want %+v", gotLabels, labels)
	}
	if len(gotSamples) != 1 || gotSamples[0] != samples[0] {
		t.Fatalf("samples = %+v, want %+v", gotSamples, samples)
	}
}

func TestEncodeWriteRequest_RoundTrip(t *testing.T) {
	ts1 := encodeTimeSeries([]promLabel{{Name: "__name__", Value: "a"}}, []promSample{{Value: 1, TimestampMs: 1}})
	ts2 := encodeTimeSeries([]promLabel{{Name: "__name__", Value: "b"}}, []promSample{{Value: 2, TimestampMs: 2}})

	wr := encodeWriteRequest([][]byte{ts1, ts2})
	series, err := decodeWriteRequest(wr)
	if err != nil {
		t.Fatalf("decodeWriteRequest: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("len(series) = %d, want 2", len(series))
	}
	labels, samples, err := decodeTimeSeries(series[1])
	if err != nil {
		t.Fatalf("decodeTimeSeries: %v", err)
	}
	if labels[0].Value != "b" || samples[0].Value != 2 {
		t.Fatalf("series[1] decoded wrong: labels=%+v samples=%+v", labels, samples)
	}
}

func TestSink_WriteSeries_EndToEnd(t *testing.T) {
	var gotBody []byte
	var gotContentType, gotEncoding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotEncoding = r.Header.Get("Content-Encoding")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := New(srv.URL)
	samples := []ports.Sample{
		{SeriesID: 42, Name: "cpu|host=a|agg=sum", ShardID: 1, ViewID: 0, Value: 9.5, TimestampMs: 1000},
	}
	if err := sink.WriteSeries(context.Background(), samples); err != nil {
		t.Fatalf("WriteSeries: %v", err)
	}

	if gotContentType != "application/x-protobuf" {
		t.Fatalf("Content-Type = %q, want application/x-protobuf", gotContentType)
	}
	if gotEncoding != "snappy" {
		t.Fatalf("Content-Encoding = %q, want snappy", gotEncoding)
	}

	body, err := decodeSnappyBlock(gotBody)
	if err != nil {
		t.Fatalf("decodeSnappyBlock: %v", err)
	}
	series, err := decodeWriteRequest(body)
	if err != nil {
		t.Fatalf("decodeWriteRequest: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("len(series) = %d, want 1", len(series))
	}
	labels, sampleList, err := decodeTimeSeries(series[0])
	if err != nil {
		t.Fatalf("decodeTimeSeries: %v", err)
	}
	if len(sampleList) != 1 || sampleList[0].Value != 9.5 || sampleList[0].TimestampMs != 1000 {
		t.Fatalf("samples = %+v, want one {9.5, 1000}", sampleList)
	}
	if !sort.SliceIsSorted(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name }) {
		t.Fatalf("labels not sorted by name: %+v", labels)
	}
	found := map[string]string{}
	for _, l := range labels {
		found[l.Name] = l.Value
	}
	if found["__name__"] != sanitizeMetricName("cpu|host=a|agg=sum") {
		t.Fatalf("__name__ = %q, want sanitized %q", found["__name__"], sanitizeMetricName("cpu|host=a|agg=sum"))
	}
	if found["palimpsest_series_id"] != "42" {
		t.Fatalf("palimpsest_series_id = %q, want 42", found["palimpsest_series_id"])
	}
}

func TestSanitizeMetricName(t *testing.T) {
	cases := map[string]string{
		"cpu|host=a|agg=sum": "cpu_host_a_agg_sum",
		"1abc":               "_1abc",
		"already_valid:name": "already_valid:name",
	}
	for in, want := range cases {
		if got := sanitizeMetricName(in); got != want {
			t.Fatalf("sanitizeMetricName(%q) = %q, want %q", in, got, want)
		}
	}
}
