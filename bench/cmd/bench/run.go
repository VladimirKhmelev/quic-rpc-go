//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
	"github.com/VladimirKhmelev/quic-rpc-go/bench/cpu"
	"github.com/VladimirKhmelev/quic-rpc-go/bench/loadgen"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/certs"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/env"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/scenarios"
)

// serverPort is the port of the server in the testbed, for UDP and TCP.
const serverPort = 4433

// runner is one run of the benchmark in the testbed.
type runner struct {
	c                      runConfig
	transport              common.Transport
	network                netns.Profile
	load                   loadgen.Profile
	tcp                    netns.TCP
	serverCPUs, clientCPUs cpu.List
	dir                    string
	logs                   []*os.File
	meta                   meta
}

// benchRun runs the server and the load generator in the testbed and
// writes calls.csv and meta.json of the run into its directory.
func benchRun(c runConfig) (int, error) {
	r := &runner{c: c}
	// What can fail without the testbed fails before it is built.
	if code, err := r.parse(); err != nil {
		return code, err
	}
	if err := netns.Enter(); err != nil {
		return 1, err
	}
	// From here on this process is root in its own namespaces. Ctrl-C
	// reaches the server and the client from the terminal by themselves.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return r.run(ctx)
}

func (r *runner) parse() (int, error) {
	var err error
	if r.transport, err = common.ParseTransport(r.c.transport); err != nil {
		return 2, fmt.Errorf("%w: %w", errUsage, err)
	}
	if r.tcp, err = netns.ParseTCP(r.c.tcp); err != nil {
		return 2, fmt.Errorf("%w: %w", errUsage, err)
	}
	networks, err := networkProfiles(r.c.profiles)
	if err != nil {
		return 1, err
	}
	if r.network, err = netns.Lookup(networks, r.c.network); err != nil {
		return 1, err
	}
	data, err := readFileOr(r.c.loads, scenarios.Loads)
	if err != nil {
		return 1, err
	}
	loads, err := loadgen.ParseProfiles(data)
	if err != nil {
		return 1, err
	}
	if r.load, err = loadgen.Lookup(loads, r.c.load); err != nil {
		return 1, err
	}
	if r.c.strict {
		if problems := env.Machine("/").Problems(); len(problems) > 0 {
			for _, p := range problems {
				fmt.Fprintf(os.Stderr, "bench: not tuned for a series: %s\n", p)
			}
			return 1, errors.New("the machine is not tuned for a series (NET-5), see testbed/env/host-setup.sh")
		}
	}
	if r.c.measure <= 0 || r.c.warmup < 0 || r.c.timeout <= 0 || r.c.gomaxprocs < 0 {
		return 2, fmt.Errorf("%w: want a positive -duration and -timeout and no negative -warmup or -gomaxprocs", errUsage)
	}
	online, err := cpu.Online("/")
	if err != nil {
		return 1, err
	}
	for _, p := range []struct {
		flag string
		list *cpu.List
		s    string
	}{{"-server-cpus", &r.serverCPUs, r.c.serverCPUs}, {"-client-cpus", &r.clientCPUs, r.c.clientCPUs}} {
		if *p.list, err = cpu.ParseList(p.s); err != nil {
			return 2, fmt.Errorf("%w: %s: %w", errUsage, p.flag, err)
		}
		for _, id := range *p.list {
			if !slices.Contains(online, id) {
				return 1, fmt.Errorf("%s %s: CPU %d is not online (online: %v); pass an empty list to run unpinned", p.flag, p.s, id, online)
			}
		}
	}
	return 0, nil
}

func (r *runner) run(ctx context.Context) (int, error) {
	id := fmt.Sprintf("%s-%s-%s-%s-s%d", time.Now().UTC().Format("20060102T150405Z"), r.transport, r.network.Name, r.load.Name, r.c.seed)
	r.dir = r.c.out
	if r.dir == "" {
		r.dir = filepath.Join("out", id)
	}
	if err := os.MkdirAll(filepath.Dir(r.dir), 0o755); err != nil { //nolint:gosec // results, not secrets
		return 1, err
	}
	// Mkdir fails if the directory exists: runs never mix.
	if err := os.Mkdir(r.dir, 0o755); err != nil { //nolint:gosec // results, not secrets
		return 1, err
	}
	defer func() {
		for _, f := range r.logs {
			_ = f.Close()
		}
	}()
	r.meta = meta{
		RunID: filepath.Base(r.dir), Transport: r.transport, Network: r.network, Load: r.load, TCP: r.tcp, Seed: r.c.seed,
		Warmup: r.c.warmup, Measure: r.c.measure, Timeout: r.c.timeout,
		Outages: outageStarts(r.network, r.c.warmup, r.c.measure), Build: readBuild(ctx),
	}

	s := &netns.Stand{TCP: r.tcp}
	if err := s.Up(ctx, r.network); err != nil {
		return 1, err
	}
	defer func() { _ = s.Down(context.Background()) }()
	e, err := env.Collect(ctx, r.tcp)
	if err != nil {
		return 1, err
	}
	r.meta.Env, r.meta.Problems = e, append([]string{}, e.Problems()...)
	for _, p := range r.meta.Problems {
		fmt.Fprintf(os.Stderr, "bench: not tuned for a series: %s\n", p)
	}
	if r.meta.Orchestrator.CPUsAllowed, err = allowed(os.Getpid()); err != nil {
		return 1, err
	}
	fmt.Fprintf(os.Stderr, "bench: run %s: %s, network %s, TCP %s, load %s, seed %d\n",
		r.dir, r.transport, r.network.Name, r.tcp, r.load.Name, r.c.seed)

	certsDir := filepath.Join(r.dir, "certs")
	b, err := certs.Issue("localhost", netns.ServerAddr.String())
	if err != nil {
		return 1, err
	}
	if err := errors.Join(os.Mkdir(certsDir, 0o700), b.Write(certsDir)); err != nil {
		return 1, err
	}
	addr := netip.AddrPortFrom(netns.ServerAddr, serverPort).String()
	srv, err := r.startServer(addr, certsDir)
	if err != nil {
		return 1, err
	}
	loadErr := r.runLoad(ctx, s, srv.cmd.Process.Pid, addr, certsDir)
	if err := errors.Join(loadErr, srv.stop()); err != nil {
		return 1, err
	}
	if err := r.summarize(); err != nil {
		return 1, err
	}
	data, err := json.MarshalIndent(r.meta, "", "  ")
	if err != nil {
		return 1, err
	}
	if err := os.WriteFile(filepath.Join(r.dir, "meta.json"), append(data, '\n'), 0o644); err != nil { //nolint:gosec // results, not secrets
		return 1, err
	}
	fmt.Fprint(os.Stderr, loadgen.FormatSummary(r.meta.Results.Kinds))
	return 0, nil
}

// outageStarts returns when the outages of p are due, since the start of
// the schedule: the first in the middle of the measurement, so that a run
// shorter than the period still has one, and the others every period after
// it until the measurement ends.
func outageStarts(p netns.Profile, warmup, measure time.Duration) []time.Duration {
	starts := []time.Duration{}
	if p.Outage.Every == 0 {
		return starts
	}
	for at := warmup + max(measure-p.Outage.Duration, 0)/2; at < warmup+measure; at += p.Outage.Every {
		starts = append(starts, at)
	}
	return starts
}

// command returns a command that runs this program with args in namespace
// ns, pinned to cpus unless there are none.
func (r *runner) command(ns string, cpus cpu.List, log string, args ...string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	name := self
	if len(cpus) > 0 {
		name, args = "taskset", append([]string{"-c", cpus.String(), self}, args...)
	}
	// Not canceled with the run: the processes stop as the run tells them.
	cmd := netns.Command(context.Background(), ns, name, args...)
	cmd.Env = append(os.Environ(), "QUIC_GO_DISABLE_GSO=true") // NET-5
	if r.c.gomaxprocs > 0 {
		cmd.Env = append(cmd.Env, "GOMAXPROCS="+strconv.Itoa(r.c.gomaxprocs))
	}
	f, err := os.Create(filepath.Join(r.dir, log)) //nolint:gosec // a file of the run
	if err != nil {
		return nil, err
	}
	r.logs = append(r.logs, f)
	cmd.Stderr = f
	return cmd, nil
}

// server is the server process of a run.
type server struct {
	cmd *exec.Cmd
	// read is closed once the server closes its standard output.
	read chan struct{}
}

// startServer starts the server and waits until it listens.
func (r *runner) startServer(addr, certsDir string) (*server, error) {
	cmd, err := r.command(netns.Server, r.serverCPUs, "server.log", "server", "-transport", string(r.transport), "-addr", addr, "-certs", certsDir)
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	srv := &server{cmd: cmd, read: make(chan struct{})}
	listening := make(chan error, 1)
	go func() {
		defer close(srv.read)
		line, err := bufio.NewReader(out).ReadString('\n')
		if err == nil && !strings.HasPrefix(line, "listening on ") {
			err = fmt.Errorf("the server said %q", line)
		}
		listening <- err
		_, _ = io.Copy(io.Discard, out)
	}()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case err = <-listening:
	case <-timer.C:
		err = errors.New("the server did not listen within 10 s")
	}
	if err == nil {
		r.meta.Server, err = r.processMeta(cmd.Process.Pid, r.serverCPUs)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("server: %w (see server.log)", err), srv.stop())
	}
	return srv, nil
}

// stop stops the server and waits for it to exit.
func (s *server) stop() error {
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	// Wait closes the pipe: it comes after the reading.
	<-s.read
	if err := s.cmd.Wait(); err != nil {
		return fmt.Errorf("server: %w (see server.log)", err)
	}
	return nil
}

func (r *runner) processMeta(pid int, cpus cpu.List) (process, error) {
	p := process{CPUs: cpus.String()}
	var err1, err2 error
	p.CPUsAllowed, err1 = allowed(pid)
	p.Env, err2 = processEnv(pid)
	return p, errors.Join(err1, err2)
}

func allowed(pid int) (string, error) {
	l, err := cpu.Allowed("/", pid)
	return l.String(), err
}

// sample is the CPU time of the server, the client and every CPU at one
// moment.
type sample struct {
	at             time.Time
	server, client cpu.Times
	cores          []cpu.Core
}

func takeSample(server, client int) (sample, error) {
	s := sample{at: time.Now()}
	var err1, err2, err3 error
	s.server, err1 = cpu.Process("/", server)
	s.client, err2 = cpu.Process("/", client)
	s.cores, err3 = cpu.Cores("/")
	return s, errors.Join(err1, err2, err3)
}

// runLoad runs the load generator to its end and follows its events: it
// starts the outages with the schedule and samples the CPU as the
// measurement begins and ends.
func (r *runner) runLoad(ctx context.Context, s *netns.Stand, server int, addr, certsDir string) error {
	args := []string{
		"load", "-transport", string(r.transport), "-addr", addr, "-certs", certsDir, "-profile", r.load.Name,
		"-seed", strconv.FormatUint(r.c.seed, 10), "-warmup", r.c.warmup.String(), "-duration", r.c.measure.String(),
		"-timeout", r.c.timeout.String(), "-out", filepath.Join(r.dir, "calls.csv"),
	}
	if r.c.loads != "" {
		loads, err := filepath.Abs(r.c.loads)
		if err != nil {
			return err
		}
		args = append(args, "-loads", loads)
	}
	client, err := r.command(netns.Client, r.clientCPUs, "client.log", args...)
	if err != nil {
		return err
	}
	out, err := client.StdoutPipe()
	if err != nil {
		return err
	}
	if err := client.Start(); err != nil {
		return err
	}
	// The client stops by itself after the run; a signal to the
	// orchestrator stops it early.
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Process.Signal(syscall.SIGTERM)
		case <-stopped:
		}
	}()
	outagesCtx, stopOutages := context.WithCancel(ctx)
	defer stopOutages()
	var outages chan error // set once the outages run

	var start time.Time
	var first, last sample
	var followErr error
	sc := bufio.NewScanner(out)
	for followErr == nil && sc.Scan() {
		var ev event
		if followErr = json.Unmarshal(sc.Bytes(), &ev); followErr != nil {
			followErr = fmt.Errorf("client: event %q: %w", sc.Text(), followErr)
			continue
		}
		switch {
		case ev.TLS != nil:
			r.meta.TLS = append(r.meta.TLS, *ev.TLS)
		case ev.Phase == loadgen.PhaseWarmup:
			start = time.Now()
			r.meta.Client, followErr = r.processMeta(client.Process.Pid, r.clientCPUs)
			if followErr == nil && len(r.meta.Outages) > 0 {
				outages = make(chan error, 1)
				go func() { outages <- s.Outages(outagesCtx, r.network, start.Add(r.meta.Outages[0])) }()
			}
		case ev.Phase == loadgen.PhaseMeasure:
			first, followErr = takeSample(server, client.Process.Pid)
		case ev.Phase == loadgen.PhaseEnd:
			last, followErr = takeSample(server, client.Process.Pid)
			stopOutages()
		}
	}
	if followErr != nil {
		_ = client.Process.Signal(syscall.SIGTERM)
	}
	// The client may still be writing: drain it before Wait.
	_, _ = io.Copy(io.Discard, out)
	stopOutages()
	err = followErr
	if outages != nil {
		err = errors.Join(err, <-outages)
	}
	waitErr := client.Wait()
	if ctx.Err() != nil {
		return errors.New("interrupted")
	}
	if waitErr != nil {
		err = errors.Join(err, fmt.Errorf("client: %w (see client.log)", waitErr))
	}
	if err != nil {
		return err
	}
	if last.at.IsZero() || first.at.IsZero() {
		return errors.New("client: the measurement did not end (see client.log)")
	}
	// UTC, as the environment; the duration by the monotonic clock.
	r.meta.Window = window{Start: start.UTC(), Measure: first.at.UTC(), End: last.at.UTC(), Duration: last.at.Sub(first.at)}
	r.meta.Server.CPU, r.meta.Client.CPU = last.server.Sub(first.server), last.client.Sub(first.client)
	if len(first.cores) != len(last.cores) {
		return errors.New("the CPUs changed during the run")
	}
	for i, c := range last.cores {
		if c.ID != first.cores[i].ID {
			return errors.New("the CPUs changed during the run")
		}
		r.meta.Cores = append(r.meta.Cores, c.Sub(first.cores[i]))
	}
	return nil
}

// summarize reads the calls back and sums them up into the results.
func (r *runner) summarize() error {
	f, err := os.Open(filepath.Join(r.dir, "calls.csv")) //nolint:gosec // a file of the run
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	records, err := loadgen.ReadCSV(f)
	if err != nil {
		return err
	}
	res := &r.meta.Results
	res.Kinds = loadgen.Summarize(records)
	res.SentPerSecond, res.ReceivedPerSecond = loadgen.Goodput(records, r.c.measure)
	res.InFlightMean, res.InFlightMax = loadgen.InFlight(records, r.c.warmup, r.c.measure)
	return nil
}
