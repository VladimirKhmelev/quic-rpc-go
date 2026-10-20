package loadgen

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// csvHeader names the columns of WriteCSV (GEN-4); data/schema.md
// describes them.
var csvHeader = []string{"seq", "kind", "scheduled_ns", "latency_ns", "code", "req_bytes", "resp_bytes", "conn", "phase"}

// WriteCSV writes one row per record, the raw data of a run.
func WriteCSV(w io.Writer, records []Record) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return err
	}
	for _, r := range records {
		phase := "measure"
		if r.Warmup {
			phase = "warmup"
		}
		if err := cw.Write([]string{
			strconv.Itoa(r.Seq), string(r.Kind),
			strconv.FormatInt(int64(r.Scheduled), 10), strconv.FormatInt(int64(r.Latency), 10),
			strconv.FormatUint(uint64(r.Code), 10), strconv.Itoa(r.ReqBytes), strconv.Itoa(r.RespBytes),
			strconv.Itoa(r.Conn), phase,
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// KindSummary sums up the measured records of one kind.
type KindSummary struct {
	Kind   Kind
	Count  int
	Errors int
	// Percentiles of the latency of the answered calls.
	P50, P99 time.Duration
}

// Summarize sums up the records outside the warm-up, by kind, in the order
// small, heavy, echo, connect.
func Summarize(records []Record) []KindSummary {
	byKind := map[Kind][]time.Duration{}
	sums := map[Kind]*KindSummary{}
	for _, r := range records {
		if r.Warmup {
			continue
		}
		s := sums[r.Kind]
		if s == nil {
			s = &KindSummary{Kind: r.Kind}
			sums[r.Kind] = s
		}
		s.Count++
		if r.Code != codeOK {
			s.Errors++
			continue
		}
		byKind[r.Kind] = append(byKind[r.Kind], r.Latency)
	}
	var out []KindSummary
	for _, k := range []Kind{KindSmall, KindHeavy, KindEcho, KindConnect} {
		s := sums[k]
		if s == nil {
			continue
		}
		s.P50, s.P99 = Percentile(byKind[k], 0.5), Percentile(byKind[k], 0.99)
		out = append(out, *s)
	}
	return out
}

// Percentile returns the q-quantile of ds by the nearest-rank method, or 0
// for none.
func Percentile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	rank := int(math.Ceil(q*float64(len(s)))) - 1
	return s[min(max(rank, 0), len(s)-1)]
}

// FormatSummary renders summaries as a table.
func FormatSummary(sums []KindSummary) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "kind\tcalls\terrors\tp50\tp99\t")
	for _, s := range sums {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%v\t%v\t\n", s.Kind, s.Count, s.Errors, s.P50.Round(time.Microsecond), s.P99.Round(time.Microsecond))
	}
	_ = w.Flush()
	return b.String()
}
