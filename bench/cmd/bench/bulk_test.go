//go:build linux

package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
	"github.com/VladimirKhmelev/quic-rpc-go/bench/loadgen"
)

func TestRunBulk(t *testing.T) {
	for _, tr := range common.Transports {
		t.Run(string(tr), func(t *testing.T) {
			dir, m := runBench(t, "-transport", string(tr), "-load", "bulk", "-warmup", "0s", "-duration", "1ns", "-server-cpus", "", "-client-cpus", "")
			calls := readCalls(t, dir)
			if len(calls) != 1 || calls[0].Kind != "bulk" || calls[0].Code != 0 || calls[0].RespBytes != 100<<20 || calls[0].Chunks != 1600 || calls[0].Warmup {
				t.Fatalf("bulk calls: %+v", calls)
			}
			b := m.Results.Bulk
			if b == nil || !b.Completed || b.ReceivedBytes != 100<<20 || b.Chunks != 1600 || b.Duration != calls[0].Latency || m.Measure != calls[0].Latency || m.Warmup != calls[0].Scheduled {
				t.Fatalf("bulk metadata: %+v, calls %+v", b, calls)
			}
			if m.Window.Duration < m.Measure || m.ConfiguredMeasure != time.Nanosecond || m.Timeout != time.Minute || m.ConnectTimeout != loadgen.DefaultTimeout {
				t.Fatalf("measurement: %+v", m)
			}
			want := float64(100<<20) / m.Measure.Seconds()
			if math.Abs(m.Results.ReceivedPerSecond/want-1) > 1e-12 {
				t.Fatalf("goodput %v, want %v", m.Results.ReceivedPerSecond, want)
			}
		})
	}
}

func TestBulkSeriesBudget(t *testing.T) {
	s := seriesFile{Matrix: matrix{Warmup: time.Second, Duration: 30 * time.Second},
		Loads: []loadgen.Profile{{Name: "bulk", Bulk: &loadgen.Bulk{TotalBytes: 100 << 20, ChunkBytes: 64 << 10}}},
		Runs:  []plannedRun{{Load: "bulk"}}}
	if got := estimate(s); got != time.Second+2*time.Minute+runOverhead {
		t.Fatalf("bulk time budget %v", got)
	}
}

func TestRunBulkDeadline(t *testing.T) {
	loads := filepath.Join(t.TempDir(), "bulk.yaml")
	if err := os.WriteFile(loads, []byte("loads:\n - name: bulk-short\n   bulk: {total_bytes: 104857600, chunk_bytes: 65536, timeout: 1ns}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tr := range common.Transports {
		t.Run(string(tr), func(t *testing.T) {
			dir, m := runBench(t, "-transport", string(tr), "-loads", loads, "-load", "bulk-short", "-warmup", "0s", "-duration", "1ns", "-server-cpus", "", "-client-cpus", "")
			recs := readCalls(t, dir)
			if len(recs) != 1 || recs[0].Code != 4 || m.Results.Bulk == nil || m.Results.Bulk.Completed || m.Results.Bulk.Code != 4 || m.Results.ReceivedPerSecond != 0 {
				t.Fatalf("deadline: records=%+v meta=%+v", recs, m.Results.Bulk)
			}
		})
	}
}
