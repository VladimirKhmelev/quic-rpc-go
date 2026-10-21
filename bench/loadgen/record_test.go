package loadgen

import (
	"bytes"
	"math"
	"reflect"
	"strings"
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

func TestReadCSV(t *testing.T) {
	records := []Record{
		{Seq: 0, Kind: KindSmall, Scheduled: 5, Latency: 1500, ReqBytes: 100, RespBytes: 100, Warmup: true},
		{Seq: 1, Kind: KindConnect, Scheduled: 6e9, Latency: 2e6, Code: 14, Conn: 2},
		{Seq: 2, Kind: KindEcho, Scheduled: 7e9, Latency: 10e9, Code: 4, ReqBytes: 200},
	}
	var b bytes.Buffer
	if err := WriteCSV(&b, records); err != nil {
		t.Fatal(err)
	}
	got, err := ReadCSV(&b)
	if err != nil || !reflect.DeepEqual(got, records) {
		t.Fatalf("ReadCSV = %+v, %v\nwant %+v", got, err, records)
	}
	const header = "seq,kind,scheduled_ns,latency_ns,code,req_bytes,resp_bytes,conn,phase\n"
	for _, in := range []string{
		"",
		"seq,kind\n0,small\n",
		header + "0,bulk,0,0,0,0,0,0,measure\n",
		header + "0,small,-1,0,0,0,0,0,measure\n",
		header + "0,small,0,0,0,0,0,0,cooldown\n",
		header + "0,small,0,0,0,0,0\n",
	} {
		if _, err := ReadCSV(strings.NewReader(in)); err == nil {
			t.Errorf("ReadCSV(%q) succeeded", in)
		}
	}
}

func TestGoodputAndInFlight(t *testing.T) {
	ms := time.Millisecond
	records := []Record{
		// In flight from 500 to 1500 ms, half of it in the measurement.
		{Kind: KindSmall, Scheduled: 500 * ms, Latency: 1000 * ms, ReqBytes: 1000, RespBytes: 1000, Warmup: true},
		{Kind: KindSmall, Scheduled: 1200 * ms, Latency: 300 * ms, ReqBytes: 100, RespBytes: 200},
		// Due as the two above are answered.
		{Kind: KindSmall, Scheduled: 1500 * ms, Latency: 1000 * ms, ReqBytes: 100, RespBytes: 100},
		// Failed, in flight past the end of the measurement.
		{Kind: KindHeavy, Scheduled: 2900 * ms, Latency: 500 * ms, Code: 4},
		{Kind: KindConnect, Scheduled: 2000 * ms, Latency: 100 * ms},
	}
	if sent, received := Goodput(records, 2*time.Second); sent != 100 || received != 150 {
		t.Errorf("goodput %v sent, %v received, want 100 and 150 B/s", sent, received)
	}
	// 500 + 300 + 1000 + 100 ms in flight over 2 s; two at once at most.
	if mean, peak := InFlight(records, time.Second, 2*time.Second); math.Abs(mean-0.95) > 1e-9 || peak != 2 {
		t.Errorf("in flight %v on average, %d at most, want 0.95 and 2", mean, peak)
	}
}
