package loadgen

import (
	"bytes"
	"reflect"
	"testing"
	"time"
)

func TestWriteCSV(t *testing.T) {
	var b bytes.Buffer
	err := WriteCSV(&b, []Record{
		{Seq: 0, Kind: KindSmall, Scheduled: 5 * time.Millisecond, Latency: 1500 * time.Microsecond, ReqBytes: 100, RespBytes: 100, Warmup: true},
		{Seq: 1, Kind: KindConnect, Scheduled: 6 * time.Second, Latency: 2 * time.Millisecond, Code: 14, Conn: 2},
	})
	want := "seq,kind,scheduled_ns,latency_ns,code,req_bytes,resp_bytes,conn,phase\n" +
		"0,small,5000000,1500000,0,100,100,0,warmup\n" +
		"1,connect,6000000000,2000000,14,0,0,2,measure\n"
	if err != nil || b.String() != want {
		t.Fatalf("got %q, %v\nwant %q", b.String(), err, want)
	}
}

func TestPercentile(t *testing.T) {
	var ds []time.Duration
	for i := 100; i >= 1; i-- {
		ds = append(ds, time.Duration(i))
	}
	for q, want := range map[float64]time.Duration{0.5: 50, 0.99: 99, 1: 100, 0.001: 1} {
		if got := Percentile(ds, q); got != want {
			t.Errorf("Percentile(%v) = %v, want %v", q, got, want)
		}
	}
	if Percentile(nil, 0.5) != 0 {
		t.Error("Percentile of nothing")
	}
	sums := Summarize([]Record{
		{Kind: KindSmall, Latency: 1, Warmup: true},
		{Kind: KindSmall, Latency: 2},
		{Kind: KindSmall, Latency: 4, Code: 4},
		{Kind: KindConnect, Latency: 3},
	})
	want := []KindSummary{{Kind: KindSmall, Count: 2, Errors: 1, P50: 2, P99: 2}, {Kind: KindConnect, Count: 1, P50: 3, P99: 3}}
	if !reflect.DeepEqual(sums, want) {
		t.Errorf("Summarize = %+v, want %+v", sums, want)
	}
}
