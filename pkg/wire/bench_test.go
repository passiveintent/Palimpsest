package wire

import "testing"

func benchFrame() *Frame {
	f := baseFrame()
	f.DictDeltas = []DictDelta{
		{ID: 1, InitValue: 1.5, Name: []byte("svc.requests.count")},
		{ID: 2, InitValue: 2.5, Name: []byte("svc.latency.p99")},
	}
	f.Payload = make([]byte, 4096)
	for i := range f.Payload {
		f.Payload[i] = byte(i)
	}
	return f
}

func BenchmarkMarshal(b *testing.B) {
	f := benchFrame()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Marshal(f); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUnmarshal(b *testing.B) {
	f := benchFrame()
	buf, err := Marshal(f)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Unmarshal(buf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQuantize(b *testing.B) {
	values := make([]float64, 2048)
	for i := range values {
		values[i] = float64(i%200) - 100
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Quantize(values, 16, 0.01); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDequantize(b *testing.B) {
	values := make([]float64, 2048)
	for i := range values {
		values[i] = float64(i%200) - 100
	}
	payload, err := Quantize(values, 16, 0.01)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Dequantize(payload, uint32(len(values)), 16, 0.01); err != nil {
			b.Fatal(err)
		}
	}
}
