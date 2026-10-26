package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/loadgen"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/scenarios"
)

// The environment of fakeCommand: the log of its command lines, the part of
// the name of a run to fail, the part of the name of a run to wait for a
// signal in, and whether the self-check fails.
const (
	fakeLog       = "BENCH_FAKE_LOG"
	fakeFail      = "BENCH_FAKE_FAIL"
	fakeHang      = "BENCH_FAKE_HANG"
	fakeCheckFail = "BENCH_FAKE_CHECK_FAIL"
)

// fakeCommand stands for bench run and testbed check in the tests of
// series: it logs its command line, and as bench run it leaves calls.csv in
// the directory of the run and, unless the run is to fail, meta.json. A run
// to hang leaves started in its directory and, like bench run, lets SIGINT
// by: it ends at once on SIGTERM, without meta.json, or after 10 s.
func fakeCommand() {
	args := os.Args[1:]
	f, err := os.OpenFile(os.Getenv(fakeLog), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		panic(err)
	}
	_, _ = fmt.Fprintln(f, strings.Join(args, " "))
	_ = f.Close()
	code := 0
	switch args[0] {
	case "check":
		if os.Getenv(fakeCheckFail) != "" {
			code = 1
		}
	case "run":
		dir := args[slices.Index(args, "-out")+1]
		_ = os.Mkdir(dir, 0o750)
		_ = os.WriteFile(filepath.Join(dir, "calls.csv"), nil, 0o600)
		if hang := os.Getenv(fakeHang); hang != "" && strings.Contains(filepath.Base(dir), hang) {
			signal.Ignore(syscall.SIGINT)
			term := make(chan os.Signal, 1)
			signal.Notify(term, syscall.SIGTERM)
			_ = os.WriteFile(filepath.Join(dir, "started"), nil, 0o600)
			select {
			case <-term:
				os.Exit(1)
			case <-time.After(10 * time.Second):
			}
		}
		if fail := os.Getenv(fakeFail); fail != "" && strings.Contains(filepath.Base(dir), fail) {
			code = 1
		} else {
			_ = os.WriteFile(filepath.Join(dir, "meta.json"), []byte("{}\n"), 0o600)
		}
	}
	os.Exit(code)
}

// wait bounds what a test of series waits for.
const wait = 10 * time.Second

const testMatrix = `name: test
seed: 7
repeats: 2
warmup: 1s
duration: 2s
variants:
  - {name: quic, transport: quic, tcp: aligned}
  - {name: grpc, transport: grpc-h2, tcp: default}
networks: [base, loss-1]
loads: [small]
drift: base
`

// testSeries runs the series of a matrix in dir with the fake commands and
// returns the exit code, the error and the command lines run.
type testSeries struct {
	t      *testing.T
	dir    string
	matrix string
	log    string
}

func newTestSeries(t *testing.T) *testSeries {
	t.Helper()
	tmp := t.TempDir()
	s := &testSeries{t: t, dir: filepath.Join(tmp, "series"), matrix: filepath.Join(tmp, "matrix.yaml"), log: filepath.Join(tmp, "log")}
	s.setMatrix(testMatrix)
	t.Setenv(fakeLog, s.log)
	// A binary built with -race sleeps a second at exit, once per run.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	return s
}

func (s *testSeries) setMatrix(text string) {
	s.t.Helper()
	if err := os.WriteFile(s.matrix, []byte(text), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// run runs the series and returns its exit code, the command lines it ran
// and its error.
func (s *testSeries) run(c seriesConfig) (int, []string, error) {
	s.t.Helper()
	_ = os.Remove(s.log)
	c.matrix, c.out, c.bench, c.testbed = s.matrix, s.dir, os.Args[0], os.Args[0]
	code, err := benchSeries(c)
	data, readErr := os.ReadFile(s.log)
	if readErr != nil && !os.IsNotExist(readErr) {
		s.t.Fatal(readErr)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(data) == 0 {
		lines = nil
	}
	return code, lines, err
}

func (s *testSeries) plan() []plannedRun {
	s.t.Helper()
	data, err := os.ReadFile(filepath.Join(s.dir, "series.json"))
	if err != nil {
		s.t.Fatal(err)
	}
	var f seriesFile
	if err := json.Unmarshal(data, &f); err != nil {
		s.t.Fatal(err)
	}
	return f.Runs
}

// runs returns the IDs of the runs among the command lines.
func runs(lines []string) []string {
	var ids []string
	for _, l := range lines {
		if f := strings.Fields(l); f[0] == "run" {
			ids = append(ids, filepath.Base(f[slices.Index(f, "-out")+1]))
		}
	}
	return ids
}

func ids(runs []plannedRun) []string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = r.ID
	}
	return out
}

func TestSeries(t *testing.T) {
	s := newTestSeries(t)
	code, lines, err := s.run(seriesConfig{check: true})
	if code != 0 || err != nil {
		t.Fatalf("series = %d, %v", code, err)
	}
	p := s.plan()
	if len(p) != 12 {
		t.Fatalf("%d runs planned, want 2 drift runs, 2 networks × 2 repeats × 2 variants and 2 drift runs", len(p))
	}
	if len(lines) != 2+len(p) || !strings.HasPrefix(lines[0], "check -profile base -json "+filepath.Join(s.dir, "check")) ||
		!strings.HasPrefix(lines[1], "check -profile loss-1 ") {
		t.Fatalf("commands:\n%s", strings.Join(lines, "\n"))
	}
	if got := runs(lines); !slices.Equal(got, ids(p)) {
		t.Errorf("runs %v, want the plan %v", got, ids(p))
	}
	for i, r := range p {
		want := fmt.Sprintf("run -transport %s -tcp %s -profile %s -load small -seed %d -warmup 1s -duration 2s -out %s",
			r.Transport, r.TCP, r.Network, r.Seed, filepath.Join(s.dir, r.ID))
		if lines[2+i] != want {
			t.Errorf("run %d: %q, want %q", i, lines[2+i], want)
		}
		if _, err := os.Stat(filepath.Join(s.dir, r.ID, "meta.json")); err != nil {
			t.Error(err)
		}
	}
	if strings.Contains(strings.Join(lines, "\n"), "-strict") {
		t.Error("-strict without -strict")
	}
}

func TestSeriesStrict(t *testing.T) {
	s := newTestSeries(t)
	_, lines, err := s.run(seriesConfig{check: true, strict: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if !strings.HasSuffix(l, " -strict") {
			t.Errorf("%q without -strict", l)
		}
	}
}

// TestSeriesFailedRun: a failed run is set aside and the series goes on;
// run again, the series makes only the failed runs.
func TestSeriesFailedRun(t *testing.T) {
	s := newTestSeries(t)
	t.Setenv(fakeFail, "loss-1-small-r2")
	code, lines, err := s.run(seriesConfig{})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "2 runs failed") || !strings.Contains(err.Error(), "-out "+s.dir) {
		t.Fatalf("series = %d, %v", code, err)
	}
	if len(runs(lines)) != 12 {
		t.Errorf("the series stopped at the failure: %v", runs(lines))
	}
	failed, err := os.ReadDir(filepath.Join(s.dir, "failed"))
	if err != nil || len(failed) != 2 {
		t.Fatalf("failed/ holds %v, %v", failed, err)
	}

	t.Setenv(fakeFail, "")
	code, lines, err = s.run(seriesConfig{})
	var want []string
	for _, id := range ids(s.plan()) {
		if strings.Contains(id, "loss-1-small-r2") {
			want = append(want, id)
		}
	}
	if code != 0 || err != nil || !slices.Equal(runs(lines), want) {
		t.Errorf("resumed series = %d, %v, ran %v, want %v", code, err, runs(lines), want)
	}
}

// TestSeriesResumesCutRun: a run cut short, with no meta.json, is made
// anew, and its remains go to failed/.
func TestSeriesResumesCutRun(t *testing.T) {
	s := newTestSeries(t)
	if _, _, err := s.run(seriesConfig{}); err != nil {
		t.Fatal(err)
	}
	cut := s.plan()[5].ID
	if err := os.Remove(filepath.Join(s.dir, cut, "meta.json")); err != nil {
		t.Fatal(err)
	}
	_, lines, err := s.run(seriesConfig{})
	if err != nil || !slices.Equal(runs(lines), []string{cut}) {
		t.Errorf("resumed series: %v, ran %v, want %s", err, runs(lines), cut)
	}
	failed, _ := os.ReadDir(filepath.Join(s.dir, "failed"))
	if len(failed) != 1 || !strings.HasPrefix(failed[0].Name(), cut+"-") {
		t.Errorf("failed/ holds %v", failed)
	}
}

// TestSeriesInterrupt: an interrupt stops the run under way, which leaves
// no meta.json, and the series; resumed, the series makes that run anew.
func TestSeriesInterrupt(t *testing.T) {
	s := newTestSeries(t)
	t.Setenv(fakeHang, "02-")
	done := make(chan error, 1)
	go func() {
		_, _, err := s.run(seriesConfig{})
		done <- err
	}()
	var cut string
	deadline := time.Now().Add(wait)
	for cut == "" {
		if time.Now().After(deadline) {
			t.Fatal("the run to interrupt did not start")
		}
		matches, _ := filepath.Glob(filepath.Join(s.dir, "02-*", "started"))
		if len(matches) == 1 {
			cut = filepath.Base(filepath.Dir(matches[0]))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("interrupted series: %v", err)
		}
	case <-time.After(wait):
		t.Fatal("the interrupt did not stop the series")
	}
	if _, err := os.Stat(filepath.Join(s.dir, cut, "meta.json")); !os.IsNotExist(err) {
		t.Fatalf("the interrupted run %s finished: %v", cut, err)
	}

	t.Setenv(fakeHang, "")
	_, lines, err := s.run(seriesConfig{})
	if p := ids(s.plan()); err != nil || !slices.Equal(runs(lines), p[2:]) {
		t.Errorf("resumed series: %v, ran %v, want %v", err, runs(lines), p[2:])
	}
	if failed, _ := os.ReadDir(filepath.Join(s.dir, "failed")); len(failed) != 1 || !strings.HasPrefix(failed[0].Name(), cut+"-") {
		t.Errorf("failed/ holds %v", failed)
	}
}

func TestSeriesRefusesAnotherMatrix(t *testing.T) {
	s := newTestSeries(t)
	if _, _, err := s.run(seriesConfig{}); err != nil {
		t.Fatal(err)
	}
	s.setMatrix(strings.Replace(testMatrix, "repeats: 2", "repeats: 3", 1))
	if _, lines, err := s.run(seriesConfig{}); err == nil || !strings.Contains(err.Error(), "another matrix") || len(lines) != 0 {
		t.Errorf("series with another matrix: %v, ran %v", err, lines)
	}
}

func TestSeriesCheckFails(t *testing.T) {
	s := newTestSeries(t)
	t.Setenv(fakeCheckFail, "1")
	code, lines, err := s.run(seriesConfig{check: true})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "self-check against base") || len(runs(lines)) != 0 {
		t.Errorf("series = %d, %v, ran %v", code, err, lines)
	}
}

func TestSeriesWithoutCheck(t *testing.T) {
	s := newTestSeries(t)
	_, lines, err := s.run(seriesConfig{})
	if err != nil || len(lines) != 12 || len(runs(lines)) != 12 {
		t.Errorf("series without the check: %v, %v", err, lines)
	}
}

func TestSeriesMissingTestbed(t *testing.T) {
	s := newTestSeries(t)
	c := seriesConfig{matrix: s.matrix, out: s.dir, bench: os.Args[0], testbed: filepath.Join(t.TempDir(), "testbed"), check: true}
	if _, err := benchSeries(c); err == nil || !strings.Contains(err.Error(), "-testbed") {
		t.Errorf("series without the testbed command: %v", err)
	}
}

func TestSeriesForeignDir(t *testing.T) {
	s := newTestSeries(t)
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "notes"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, lines, err := s.run(seriesConfig{}); err == nil || !strings.Contains(err.Error(), "something other than a series") || len(lines) != 0 {
		t.Errorf("series in a foreign directory: %v, ran %v", err, lines)
	}
}

func TestSeriesDryRun(t *testing.T) {
	s := newTestSeries(t)
	var stdout bytes.Buffer
	cmd := command(t, "series", "-matrix", s.matrix, "-out", s.dir, "-dry-run")
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 13 || !strings.HasPrefix(lines[0], "00-") || !strings.HasPrefix(lines[12], "12 runs on base, loss-1, about ") {
		t.Errorf("plan:\n%s", stdout.String())
	}
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Errorf("a dry run made %s", s.dir)
	}
}

func TestPlan(t *testing.T) {
	networks, err := networkProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	loads, err := loadgen.ParseProfiles(scenarios.Loads)
	if err != nil {
		t.Fatal(err)
	}
	m, err := parseMatrix([]byte(testMatrix), networks, loads)
	if err != nil {
		t.Fatal(err)
	}
	p := plan(m)
	if again := plan(m); !slices.Equal(ids(again), ids(p)) {
		t.Error("the same seed gave another plan")
	}
	m.Seed++
	if other := plan(m); slices.Equal(ids(other), ids(p)) {
		t.Error("another seed gave the same plan")
	}
	// Every variant runs once per cell, with the seed of the repeat, and
	// the variants of a cell come together.
	cells := map[string][]string{}
	var order []string
	for i, r := range p {
		if !strings.HasPrefix(r.ID, fmt.Sprintf("%02d-%s-%s-%s-", i, r.Variant, r.Network, r.Load)) {
			t.Errorf("run %d: %q", i, r.ID)
		}
		key := fmt.Sprintf("%s %s %d %s", r.Network, r.Load, r.Repeat, r.Role)
		if len(order) == 0 || order[len(order)-1] != key {
			if _, seen := cells[key]; seen {
				t.Errorf("the variants of %s are apart", key)
			}
			order = append(order, key)
		}
		cells[key] = append(cells[key], r.Variant)
		if want := uint64(max(r.Repeat, 1)); r.Seed != want { //nolint:gosec // positive
			t.Errorf("run %s has seed %d, want %d", r.ID, r.Seed, want)
		}
	}
	if len(cells) != 2*2+2 {
		t.Errorf("cells %v", order)
	}
	for key, vs := range cells {
		if !slices.Equal(slices.Sorted(slices.Values(vs)), []string{"grpc", "quic"}) {
			t.Errorf("cell %s runs %v", key, vs)
		}
	}
	if order[0] != "base small 0 drift-start" || order[len(order)-1] != "base small 0 drift-end" {
		t.Errorf("the drift runs do not open and close the series: %v", order)
	}
	if nested := []string{"base small 1 ", "loss-1 small 1 ", "base small 2 ", "loss-1 small 2 "}; slices.Equal(order[1:len(order)-1], nested) {
		t.Errorf("the cells come in the order of the matrix: %v", order)
	}
	// The variants of the cells come in more than one order.
	firsts := map[string]bool{}
	for _, key := range order {
		firsts[cells[key][0]] = true
	}
	if len(firsts) != 2 {
		t.Errorf("the variants always come in the same order: %v", cells)
	}
}

func TestParseMatrix(t *testing.T) {
	networks, err := networkProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	loads, err := loadgen.ParseProfiles(scenarios.Loads)
	if err != nil {
		t.Fatal(err)
	}
	pilot, err := os.ReadFile("../../../testbed/scenarios/pilot.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if m, err := parseMatrix(pilot, networks, loads); err != nil || len(plan(m)) != 3*6*2*10+3*2*2 {
		t.Errorf("pilot.yaml: %v", err)
	}
	for _, tc := range []struct{ old, new, err string }{
		{"name: test", "name: Test", "name"},
		{"repeats: 2", "repeats: 0", "repeats"},
		{"duration: 2s", "duration: 0s", "duration"},
		{"transport: quic", "transport: h3", "transport"},
		{"tcp: default", "tcp: tuned", "TCP setup"},
		{"name: grpc,", "name: quic,", "twice"},
		{"[base, loss-1]", "[base, loss-9]", "loss-9"},
		{"[base, loss-1]", "[base, base]", "listed twice"},
		{"[small]", "[large]", "large"},
		{"[small]", "[small, small]", "listed twice"},
		{"drift: base", "drift: moon", "drift"},
		{"drift: base", "drift: base\nextra: 1", "extra"},
		{"networks: [base, loss-1]\n", "", "want variants, networks and loads"},
	} {
		text := strings.Replace(testMatrix, tc.old, tc.new, 1)
		if text == testMatrix {
			t.Fatalf("%q is not in the matrix", tc.old)
		}
		if _, err := parseMatrix([]byte(text), networks, loads); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%q → %q: %v, want an error about %s", tc.old, tc.new, err, tc.err)
		}
	}
}
