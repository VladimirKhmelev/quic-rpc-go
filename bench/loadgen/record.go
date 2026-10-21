package loadgen

import (
	"cmp"
	"encoding/csv"
	"errors"
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

// ReadCSV reads the records that WriteCSV wrote.
func ReadCSV(r io.Reader) ([]Record, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("loadgen: %w", err)
	}
	if len(rows) == 0 || !slices.Equal(rows[0], csvHeader) {
		return nil, errors.New("loadgen: not a CSV of calls")
	}
	records := make([]Record, 0, len(rows)-1)
	for i, row := range rows[1:] {
		r, err := parseRow(row)
		if err != nil {
			return nil, fmt.Errorf("loadgen: line %d: %w", i+2, err)
		}
		records = append(records, r)
	}
	return records, nil
}

func parseRow(row []string) (Record, error) {
	var ints [6]int64
	for i, col := range []int{0, 2, 3, 5, 6, 7} {
		n, err := strconv.ParseInt(row[col], 10, 64)
		if err != nil || n < 0 {
			return Record{}, fmt.Errorf("%s %q", csvHeader[col], row[col])
		}
		ints[i] = n
	}
	code, err := strconv.ParseUint(row[4], 10, 32)
	if err != nil {
		return Record{}, fmt.Errorf("code %q", row[4])
	}
	kind := Kind(row[1])
	if !slices.Contains([]Kind{KindSmall, KindHeavy, KindEcho, KindConnect}, kind) {
		return Record{}, fmt.Errorf("kind %q", row[1])
	}
	if row[8] != "warmup" && row[8] != "measure" {
		return Record{}, fmt.Errorf("phase %q", row[8])
	}
	return Record{
		Seq: int(ints[0]), Kind: kind, Scheduled: time.Duration(ints[1]), Latency: time.Duration(ints[2]),
		Code: uint32(code), ReqBytes: int(ints[3]), RespBytes: int(ints[4]), Conn: int(ints[5]),
		Warmup: row[8] == "warmup",
	}, nil
}

// Goodput returns the payload bytes per second that the answered calls
// and messages of the measurement carried: sent in requests and received
// in responses.
func Goodput(records []Record, measure time.Duration) (sent, received float64) {
	for _, r := range records {
		if !r.Warmup && r.Code == codeOK {
			sent += float64(r.ReqBytes)
			received += float64(r.RespBytes)
		}
	}
	return sent / measure.Seconds(), received / measure.Seconds()
}

// InFlight returns how many calls and messages were in flight during the
// measurement, from warmup to warmup + measure: on average and at most. One
// is in flight from the time it was due to its answer or failure.
func InFlight(records []Record, warmup, measure time.Duration) (mean float64, peak int) {
	from, to := warmup, warmup+measure
	type edge struct {
		at    time.Duration
		delta int
	}
	var edges []edge
	var busy time.Duration
	for _, r := range records {
		lo, hi := max(r.Scheduled, from), min(r.Scheduled+r.Latency, to)
		if r.Kind == KindConnect || lo >= hi {
			continue
		}
		busy += hi - lo
		edges = append(edges, edge{lo, 1}, edge{hi, -1})
	}
	// An answer at the time the next call is due comes first: the two do
	// not overlap.
	slices.SortFunc(edges, func(a, b edge) int { return cmp.Or(cmp.Compare(a.at, b.at), cmp.Compare(a.delta, b.delta)) })
	n := 0
	for _, e := range edges {
		n += e.delta
		peak = max(peak, n)
	}
	return float64(busy) / float64(measure), peak
}

// KindSummary sums up the measured records of one kind.
type KindSummary struct {
	Kind   Kind `json:"kind"`
	Count  int  `json:"count"`
	Errors int  `json:"errors"`
	// Percentiles of the latency of the answered calls.
	P50 time.Duration `json:"p50_ns"`
	P99 time.Duration `json:"p99_ns"`
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
