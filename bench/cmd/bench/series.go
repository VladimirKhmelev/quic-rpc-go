package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
	"github.com/VladimirKhmelev/quic-rpc-go/bench/loadgen"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/scenarios"
)

// runOverhead is roughly what a run takes beyond its warm-up and
// measurement: the testbed, the certificates, the processes.
const runOverhead = 5 * time.Second

// matrix describes a series (EXP-2, EXP-5): every variant on every network
// and load, Repeats times. The runs of a repeat share its seed, so that
// every variant answers the same schedule of calls.
type matrix struct {
	Name     string        `json:"name"        yaml:"name"`
	Seed     uint64        `json:"seed"        yaml:"seed"`
	Repeats  int           `json:"repeats"     yaml:"repeats"`
	Warmup   time.Duration `json:"warmup_ns"   yaml:"warmup"`
	Duration time.Duration `json:"duration_ns" yaml:"duration"`
	Variants []variant     `json:"variants"    yaml:"variants"`
	Networks []string      `json:"networks"    yaml:"networks"`
	Loads    []string      `json:"loads"       yaml:"loads"`
	// Drift is the network of the runs, one per variant and load, that open
	// and close the series and show how much the environment drifted.
	Drift string `json:"drift" yaml:"drift"`
}

// variant is a transport with a TCP setup of the end hosts.
type variant struct {
	Name      string `json:"name"      yaml:"name"`
	Transport string `json:"transport" yaml:"transport"`
	TCP       string `json:"tcp"       yaml:"tcp"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// parseMatrix reads a matrix and checks it against the known networks and
// loads.
func parseMatrix(data []byte, networks []netns.Profile, loads []loadgen.Profile) (matrix, error) {
	var m matrix
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return matrix{}, fmt.Errorf("matrix: %w", err)
	}
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}
	check(nameRE.MatchString(m.Name), "name %q: want lowercase letters, digits and dashes", m.Name)
	check(m.Repeats >= 1, "repeats %d: want at least 1", m.Repeats)
	check(m.Duration > 0 && m.Warmup >= 0, "want a positive duration and no negative warmup")
	check(len(m.Variants) > 0 && len(m.Networks) > 0 && len(m.Loads) > 0, "want variants, networks and loads")
	seen := map[string]bool{}
	for _, v := range m.Variants {
		check(nameRE.MatchString(v.Name), "variant %q: want lowercase letters, digits and dashes", v.Name)
		check(!seen[v.Name], "variant %q is defined twice", v.Name)
		seen[v.Name] = true
		if _, err := common.ParseTransport(v.Transport); err != nil {
			errs = append(errs, fmt.Errorf("variant %s: %w", v.Name, err))
		}
		if _, err := netns.ParseTCP(v.TCP); err != nil {
			errs = append(errs, fmt.Errorf("variant %s: %w", v.Name, err))
		}
	}
	listed := map[string]bool{}
	for _, n := range m.Networks {
		check(!listed["network "+n], "network %q is listed twice", n)
		listed["network "+n] = true
		if _, err := netns.Lookup(networks, n); err != nil {
			errs = append(errs, err)
		}
	}
	if m.Drift != "" {
		if _, err := netns.Lookup(networks, m.Drift); err != nil {
			errs = append(errs, fmt.Errorf("drift: %w", err))
		}
	}
	for _, l := range m.Loads {
		check(!listed["load "+l], "load %q is listed twice", l)
		listed["load "+l] = true
		if _, err := loadgen.Lookup(loads, l); err != nil {
			errs = append(errs, err)
		}
	}
	return m, errors.Join(errs...)
}

// plannedRun is a run of a series.
type plannedRun struct {
	ID        string `json:"id"`
	Variant   string `json:"variant"`
	Transport string `json:"transport"`
	TCP       string `json:"tcp"`
	Network   string `json:"network"`
	Load      string `json:"load"`
	// Repeat counts from 1 and is the seed of the run; the drift runs have
	// repeat 0 and the seed 1.
	Repeat int    `json:"repeat"`
	Seed   uint64 `json:"seed"`
	// Role is drift-start or drift-end for the drift runs.
	Role string `json:"role,omitempty"`
}

// plan lays out the runs of m (EXP-5): the cells of network, load and repeat
// come in an order shuffled by the seed of the series, and the variants of a
// cell one after another in a shuffled order, so that a drift of the
// environment favours no variant. The drift runs open and close the series.
func plan(m matrix) []plannedRun {
	rng := rand.New(rand.NewPCG(m.Seed, 0)) //nolint:gosec // an order, not a secret
	var runs []plannedRun
	cell := func(network, load string, repeat int, role string) {
		vs := slices.Clone(m.Variants)
		rng.Shuffle(len(vs), func(i, j int) { vs[i], vs[j] = vs[j], vs[i] })
		for _, v := range vs {
			runs = append(runs, plannedRun{Variant: v.Name, Transport: v.Transport, TCP: v.TCP, Network: network,
				Load: load, Repeat: repeat, Seed: uint64(max(repeat, 1)), Role: role}) //nolint:gosec // repeat is positive
		}
	}
	drift := func(role string) {
		if m.Drift != "" {
			for _, l := range m.Loads {
				cell(m.Drift, l, 0, role)
			}
		}
	}
	type key struct {
		network, load string
		repeat        int
	}
	var keys []key
	for r := 1; r <= m.Repeats; r++ {
		for _, n := range m.Networks {
			for _, l := range m.Loads {
				keys = append(keys, key{n, l, r})
			}
		}
	}
	rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	drift("drift-start")
	for _, k := range keys {
		cell(k.network, k.load, k.repeat, "")
	}
	drift("drift-end")
	width := len(strconv.Itoa(len(runs) - 1))
	for i := range runs {
		r := &runs[i]
		tag := "r" + strconv.Itoa(r.Repeat)
		if r.Role != "" {
			tag = r.Role
		}
		r.ID = fmt.Sprintf("%0*d-%s-%s-%s-%s", width, i, r.Variant, r.Network, r.Load, tag)
	}
	return runs
}

// seriesFile is series.json: the matrix, the networks and loads it names as
// they were when the series began, and its plan.
type seriesFile struct {
	Matrix   matrix            `json:"matrix"`
	Networks []netns.Profile   `json:"networks"`
	Loads    []loadgen.Profile `json:"loads"`
	Created  time.Time         `json:"created"`
	Runs     []plannedRun      `json:"runs"`
}

// setup is what a resumed series must share with its start.
func (s seriesFile) setup() ([]byte, error) {
	return json.Marshal(struct {
		Matrix   matrix            `json:"matrix"`
		Networks []netns.Profile   `json:"networks"`
		Loads    []loadgen.Profile `json:"loads"`
	}{s.Matrix, s.Networks, s.Loads})
}

// seriesConfig is the command line of series.
type seriesConfig struct {
	matrix, out, bench, testbed string
	strict, check, dryRun       bool
}

// benchSeries runs the series of a matrix file, or resumes it (EXP-8).
func benchSeries(c seriesConfig) (int, error) {
	data, err := os.ReadFile(c.matrix)
	if err != nil {
		return 1, err
	}
	networks, err := networkProfiles("")
	if err != nil {
		return 1, err
	}
	loads, err := loadgen.ParseProfiles(scenarios.Loads)
	if err != nil {
		return 1, err
	}
	m, err := parseMatrix(data, networks, loads)
	if err != nil {
		return 1, err
	}
	s := seriesFile{Matrix: m, Created: time.Now().UTC().Truncate(time.Second), Runs: plan(m)}
	for _, n := range distinct(s.Runs, func(r plannedRun) string { return r.Network }) {
		p, _ := netns.Lookup(networks, n) // parseMatrix found it
		s.Networks = append(s.Networks, p)
	}
	for _, l := range m.Loads {
		p, _ := loadgen.Lookup(loads, l) // parseMatrix found it
		s.Loads = append(s.Loads, p)
	}
	if c.dryRun {
		printPlan(os.Stdout, s)
		return 0, nil
	}
	if c.bench == "" {
		if c.bench, err = os.Executable(); err != nil {
			return 1, err
		}
	}
	if c.testbed == "" {
		c.testbed = filepath.Join(filepath.Dir(c.bench), "testbed")
	}
	dir := c.out
	if dir == "" {
		dir = filepath.Join("out", s.Created.Format("20060102T150405Z")+"-"+m.Name)
	}
	if s, err = openSeries(dir, s); err != nil {
		return 1, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	resume := fmt.Sprintf("bench series -matrix %s -out %s", c.matrix, dir)
	if c.check {
		if err := selfCheck(ctx, c, dir, s); err != nil {
			return 1, err
		}
	}
	return runSeries(ctx, c, dir, s, resume)
}

// openSeries starts a series in dir, or takes up the one already there if
// it has the same setup.
func openSeries(dir string, s seriesFile) (seriesFile, error) {
	path := filepath.Join(dir, "series.json")
	data, err := os.ReadFile(path) //nolint:gosec // the series the user named
	if err == nil {
		var old seriesFile
		if err := json.Unmarshal(data, &old); err != nil {
			return seriesFile{}, fmt.Errorf("%s: %w", path, err)
		}
		was, err1 := old.setup()
		is, err2 := s.setup()
		if err := errors.Join(err1, err2); err != nil {
			return seriesFile{}, err
		}
		if !bytes.Equal(was, is) {
			return seriesFile{}, fmt.Errorf("the series in %s was planned with another matrix or other profiles", dir)
		}
		fmt.Fprintf(os.Stderr, "bench: resuming the series in %s\n", dir)
		return old, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return seriesFile{}, err
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return seriesFile{}, fmt.Errorf("%s holds something other than a series", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // results, not secrets
		return seriesFile{}, err
	}
	if data, err = json.MarshalIndent(s, "", "  "); err != nil {
		return seriesFile{}, err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil { //nolint:gosec // results, not secrets
		return seriesFile{}, err
	}
	fmt.Fprintf(os.Stderr, "bench: series %s, %d runs, about %v\n", dir, len(s.Runs), estimate(s))
	return s, nil
}

// selfCheck checks the testbed against every network of the series before
// its runs (NET-4); the reports go to check/ of the series.
func selfCheck(ctx context.Context, c seriesConfig, dir string, s seriesFile) error {
	checkDir := filepath.Join(dir, "check")
	if err := os.MkdirAll(checkDir, 0o755); err != nil { //nolint:gosec // results, not secrets
		return err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	for _, n := range distinct(s.Runs, func(r plannedRun) string { return r.Network }) {
		args := []string{"check", "-profile", n, "-json", filepath.Join(checkDir, stamp+"-"+n+".json")}
		if c.strict {
			args = append(args, "-strict")
		}
		fmt.Fprintf(os.Stderr, "bench: checking the testbed against %s\n", n)
		if err := seriesCommand(ctx, c.testbed, args...).Run(); err != nil {
			if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%w: build the testbed command next to bench (make bench-pilot does) or name it with -testbed", err)
			}
			return fmt.Errorf("the testbed fails its self-check against %s: %w", n, err)
		}
	}
	return nil
}

// runSeries makes the runs not done yet, in the order of the plan. A run
// that fails goes to failed/ and the series goes on; a run cut short by an
// interrupt goes there when the series resumes.
func runSeries(ctx context.Context, c seriesConfig, dir string, s seriesFile, resume string) (int, error) {
	var failed []string
	done := 0
	for i, r := range s.Runs {
		if ctx.Err() != nil {
			break
		}
		runDir := filepath.Join(dir, r.ID)
		if _, err := os.Stat(filepath.Join(runDir, "meta.json")); err == nil {
			done++
			continue
		}
		if err := setAside(dir, runDir); err != nil {
			return 1, err
		}
		fmt.Fprintf(os.Stderr, "bench: run %d of %d: %s\n", i+1, len(s.Runs), r.ID)
		if err := seriesCommand(ctx, c.bench, runArgs(s.Matrix, r, runDir, c.strict)...).Run(); err != nil {
			if ctx.Err() != nil {
				break
			}
			fmt.Fprintf(os.Stderr, "bench: run %s failed: %v\n", r.ID, err)
			failed = append(failed, r.ID)
			if err := setAside(dir, runDir); err != nil {
				return 1, err
			}
			continue
		}
		done++
	}
	fmt.Fprintf(os.Stderr, "bench: %d of %d runs of the series done\n", done, len(s.Runs))
	switch {
	case ctx.Err() != nil:
		return 1, fmt.Errorf("interrupted; resume with %s", resume)
	case len(failed) > 0:
		return 1, fmt.Errorf("%d runs failed (%s), their remains are in %s; make them again with %s",
			len(failed), strings.Join(failed, ", "), filepath.Join(dir, "failed"), resume)
	}
	return 0, nil
}

// runArgs is the command line of bench run for a run of the series.
func runArgs(m matrix, r plannedRun, dir string, strict bool) []string {
	args := []string{"run", "-transport", r.Transport, "-tcp", r.TCP, "-profile", r.Network, "-load", r.Load,
		"-seed", strconv.FormatUint(r.Seed, 10), "-warmup", m.Warmup.String(), "-duration", m.Duration.String(), "-out", dir}
	if strict {
		args = append(args, "-strict")
	}
	return args
}

// seriesCommand runs a command of the series with its output on stderr. An
// interrupt of the series stops it with SIGTERM, which bench run and
// testbed check pass on into their namespaces; they let SIGINT by, since
// Ctrl-C reaches every process of the terminal by itself.
func seriesCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // bench and testbed, as the user named them
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	return cmd
}

// setAside moves the remains of a run that did not finish into failed/, so
// that the run is made anew and the remains stay for a look.
func setAside(dir, runDir string) error {
	if _, err := os.Stat(runDir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	failed := filepath.Join(dir, "failed")
	if err := os.MkdirAll(failed, 0o755); err != nil { //nolint:gosec // results, not secrets
		return err
	}
	return os.Rename(runDir, filepath.Join(failed, filepath.Base(runDir)+"-"+time.Now().UTC().Format("20060102T150405.000Z")))
}

// distinct returns the values of f over the runs, each once, in the order
// they first come.
func distinct(runs []plannedRun, f func(plannedRun) string) []string {
	var out []string
	for _, r := range runs {
		if v := f(r); !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// estimate is roughly how long the runs of s take.
func estimate(s seriesFile) time.Duration {
	return time.Duration(len(s.Runs)) * (s.Matrix.Warmup + s.Matrix.Duration + runOverhead)
}

func printPlan(w io.Writer, s seriesFile) {
	for _, r := range s.Runs {
		_, _ = fmt.Fprintln(w, r.ID)
	}
	_, _ = fmt.Fprintf(w, "%d runs on %s, about %v\n", len(s.Runs),
		strings.Join(distinct(s.Runs, func(r plannedRun) string { return r.Network }), ", "), estimate(s))
}
