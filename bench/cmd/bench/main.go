// Command bench runs the parts of a benchmark (GEN-1).
//
// Usage:
//
//	bench certs -dir DIR [-hosts HOST,...]
//	bench server -transport quic|grpc-h2|grpc-h2-pool|grpc-h3|connect-h2 -addr HOST:PORT -certs DIR
//	bench load -transport quic|grpc-h2|grpc-h2-pool|grpc-h3|connect-h2 -addr HOST:PORT -certs DIR -profile NAME -out FILE
//	           [-loads FILE] [-seed N] [-warmup D] [-duration D] [-timeout D] [-closed N] [-pool-size N]
//	bench run -transport quic|grpc-h2|grpc-h2-pool|grpc-h3|connect-h2 -load NAME [-profile NAME] [-tcp SETUP] [-seed N]
//	          [-warmup D] [-duration D] [-timeout D] [-server-cpus LIST] [-client-cpus LIST]
//	          [-gomaxprocs N] [-profiles FILE] [-loads FILE] [-strict] [-pool-size N] [-out DIR]
//	bench series -matrix FILE [-out DIR] [-strict] [-check=false] [-dry-run]
//	             [-bench BIN] [-testbed BIN]
//
// certs issues a throwaway CA and a server certificate into DIR, for the
// server and its clients to share. server answers the calls of the
// benchmark service on addr with the certificate from DIR; it prints
// "listening on ADDR" once it accepts connections and stops on SIGINT or
// SIGTERM.
//
// load runs the load profile NAME of testbed/scenarios/load.yaml (or of
// -loads FILE) against the server at addr: -warmup of warm-up, then
// -duration of measurement, with the seed -seed. It writes a row per call
// to the CSV file -out (see data/schema.md) and prints a summary; failed
// calls are rows like any other, so it fails only if it cannot run.
// -closed N makes the calls closed-loop on N workers, for comparison. On
// its standard output it prints a line of JSON as each phase of the run
// begins, {"phase":"warmup"}, "measure" and "end", and for the TLS setup of
// the first connection and of any that differs:
// {"tls":{"version":"TLS 1.3","cipher_suite":"...","group":"..."}}.
//
// run makes one run of the benchmark in the network testbed (Linux only,
// no root; see docs/testbed.md): it builds the testbed with the network
// profile -profile of testbed/scenarios/profiles.yaml or points.yaml (or of
// -profiles FILE) and the TCP setup -tcp, starts the server in namespace server on
// CPUs -server-cpus and the load generator with load profile -load in
// namespace client on CPUs -client-cpus, both with GOMAXPROCS=-gomaxprocs
// and GSO of quic-go off (NET-5). The outages of the network profile start
// in the middle of the measurement. It writes calls.csv, meta.json (the
// setup, the environment, the CPU time of the measurement and a summary),
// the logs and the certificates into -out, by default
// out/TIME-TRANSPORT-NETWORK-LOAD-sSEED; data/schema.md describes the
// files. With -strict it refuses to run on a machine not tuned for a
// series. An empty CPU list leaves a process unpinned.
//
// series makes the runs of the matrix FILE, such as
// testbed/scenarios/pilot.yaml (EXP-2, EXP-5): every variant on every
// network and load, in an order shuffled by the seed of the matrix, each
// with bench run into a directory of the series, by default
// out/TIME-NAME. It first checks the testbed against every network of the
// series with testbed check (NET-4), unless -check=false; the testbed
// command is BIN of -testbed, by default testbed next to bench. A run that
// fails is set aside in failed/ and the series goes on. Run again with -out
// DIR, a series resumes: it skips the runs done and makes the others
// (EXP-8). series.json in DIR holds the plan. -strict goes to the checks and
// the runs; -dry-run prints the plan and stops.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
	"github.com/VladimirKhmelev/quic-rpc-go/bench/loadgen"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/certs"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/scenarios"
)

const usage = `usage:
  bench certs -dir DIR [-hosts HOST,...]
  bench server -transport quic|grpc-h2|grpc-h2-pool|grpc-h3|connect-h2 -addr HOST:PORT -certs DIR
  bench load -transport quic|grpc-h2|grpc-h2-pool|grpc-h3|connect-h2 -addr HOST:PORT -certs DIR -profile NAME -out FILE
             [-loads FILE] [-seed N] [-warmup D] [-duration D] [-timeout D] [-closed N] [-pool-size N]
  bench run -transport quic|grpc-h2|grpc-h2-pool|grpc-h3|connect-h2 -load NAME [-profile NAME] [-tcp SETUP] [-seed N]
            [-warmup D] [-duration D] [-timeout D] [-server-cpus LIST] [-client-cpus LIST]
            [-gomaxprocs N] [-profiles FILE] [-loads FILE] [-strict] [-pool-size N] [-out DIR]
  bench series -matrix FILE [-out DIR] [-strict] [-check=false] [-dry-run]
               [-bench BIN] [-testbed BIN]
`

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

var errUsage = errors.New("bad arguments")

func run(args []string) (int, error) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2, nil
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	switch args[0] {
	case "certs":
		dir := fs.String("dir", "", "write the certificates to `DIR`")
		hosts := fs.String("hosts", "localhost,"+netns.ServerAddr.String(), "names and addresses the server certificate is valid for")
		if err := fs.Parse(args[1:]); err != nil {
			return 2, nil // flag has printed the error
		}
		if *dir == "" || fs.NArg() != 0 {
			return 2, fmt.Errorf("%w: certs needs -dir", errUsage)
		}
		b, err := certs.Issue(strings.Split(*hosts, ",")...)
		if err != nil {
			return 1, err
		}
		return 0, b.Write(*dir)
	case "server":
		transport := fs.String("transport", "", "serve with `TRANSPORT`: quic, grpc-h2, grpc-h2-pool, grpc-h3 or connect-h2")
		addr := fs.String("addr", "", "listen on `HOST:PORT`")
		certsDir := fs.String("certs", "", "read the certificates from `DIR`")
		if err := fs.Parse(args[1:]); err != nil {
			return 2, nil // flag has printed the error
		}
		if *addr == "" || *certsDir == "" || fs.NArg() != 0 {
			return 2, fmt.Errorf("%w: server needs -transport, -addr and -certs", errUsage)
		}
		t, err := common.ParseTransport(*transport)
		if err != nil {
			return 2, fmt.Errorf("%w: %w", errUsage, err)
		}
		return serve(t, *addr, *certsDir)
	case "load":
		var c loadConfig
		fs.StringVar(&c.transport, "transport", "", "call with `TRANSPORT`: quic, grpc-h2, grpc-h2-pool, grpc-h3 or connect-h2")
		fs.StringVar(&c.addr, "addr", "", "call the server at `HOST:PORT`")
		fs.StringVar(&c.certs, "certs", "", "read the certificates from `DIR`")
		fs.StringVar(&c.profile, "profile", "", "run load profile `NAME`")
		fs.StringVar(&c.loads, "loads", "", "read load profiles from `FILE` instead of the bundled ones")
		fs.StringVar(&c.out, "out", "", "write the calls to the CSV `FILE`")
		fs.Uint64Var(&c.cfg.Seed, "seed", 1, "seed of the schedule")
		fs.DurationVar(&c.cfg.Warmup, "warmup", 5*time.Second, "warm-up before the measurement")
		fs.DurationVar(&c.cfg.Measure, "duration", 30*time.Second, "length of the measurement")
		fs.DurationVar(&c.cfg.Timeout, "timeout", loadgen.DefaultTimeout, "deadline of every call")
		fs.IntVar(&c.cfg.Closed, "closed", 0, "run closed-loop on `N` workers instead of open-loop")
		fs.IntVar(&c.poolSize, "pool-size", 0, "number of connections for grpc-h2-pool (default 4)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2, nil // flag has printed the error
		}
		if c.addr == "" || c.certs == "" || c.profile == "" || c.out == "" || fs.NArg() != 0 {
			return 2, fmt.Errorf("%w: load needs -transport, -addr, -certs, -profile and -out", errUsage)
		}
		if err := checkPoolFlag(fs); err != nil {
			return 2, err
		}
		return load(c)
	case "run":
		var c runConfig
		fs.StringVar(&c.transport, "transport", "", "serve and call with `TRANSPORT`: quic, grpc-h2, grpc-h2-pool, grpc-h3 or connect-h2")
		fs.StringVar(&c.load, "load", "", "run load profile `NAME`")
		fs.StringVar(&c.network, "profile", netns.None.Name, "apply network profile `NAME`")
		fs.StringVar(&c.tcp, "tcp", string(netns.TCPAligned), "TCP setup of the end hosts: aligned or default")
		fs.Uint64Var(&c.seed, "seed", 1, "seed of the schedule")
		fs.DurationVar(&c.warmup, "warmup", 5*time.Second, "warm-up before the measurement")
		fs.DurationVar(&c.measure, "duration", 30*time.Second, "length of the measurement")
		fs.DurationVar(&c.timeout, "timeout", loadgen.DefaultTimeout, "deadline of every call")
		fs.StringVar(&c.serverCPUs, "server-cpus", "2-3", "pin the server to `LIST` of CPUs")
		fs.StringVar(&c.clientCPUs, "client-cpus", "4-5", "pin the load generator to `LIST` of CPUs")
		fs.IntVar(&c.gomaxprocs, "gomaxprocs", 2, "GOMAXPROCS of the server and the load generator; 0 leaves Go's default")
		fs.StringVar(&c.profiles, "profiles", "", "read network profiles from `FILE` instead of the bundled ones")
		fs.StringVar(&c.loads, "loads", "", "read load profiles from `FILE` instead of the bundled ones")
		fs.BoolVar(&c.strict, "strict", false, "refuse to run on a machine not tuned for a series (NET-5)")
		fs.IntVar(&c.poolSize, "pool-size", 0, "number of connections for grpc-h2-pool (default 4)")
		fs.StringVar(&c.out, "out", "", "write the run into `DIR`, which must not exist")
		if err := fs.Parse(args[1:]); err != nil {
			return 2, nil // flag has printed the error
		}
		if c.load == "" || fs.NArg() != 0 {
			return 2, fmt.Errorf("%w: run needs -transport and -load", errUsage)
		}
		if err := checkPoolFlag(fs); err != nil {
			return 2, err
		}
		return benchRun(c)
	case "series":
		var c seriesConfig
		fs.StringVar(&c.matrix, "matrix", "", "run the series of `FILE`")
		fs.StringVar(&c.out, "out", "", "keep the series in `DIR`; a series already there resumes")
		fs.BoolVar(&c.strict, "strict", false, "refuse to run on a machine not tuned for a series (NET-5)")
		fs.BoolVar(&c.check, "check", true, "check the testbed against every network of the series first (NET-4)")
		fs.BoolVar(&c.dryRun, "dry-run", false, "print the plan of the series and stop")
		fs.StringVar(&c.bench, "bench", "", "make the runs with the bench command `BIN`, this one by default")
		fs.StringVar(&c.testbed, "testbed", "", "check with the testbed command `BIN`, testbed next to bench by default")
		if err := fs.Parse(args[1:]); err != nil {
			return 2, nil // flag has printed the error
		}
		if c.matrix == "" || fs.NArg() != 0 {
			return 2, fmt.Errorf("%w: series needs -matrix", errUsage)
		}
		return benchSeries(c)
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2, nil
	}
}

// runConfig is the command line of run.
type runConfig struct {
	transport, load, network, tcp, profiles, loads, out string
	serverCPUs, clientCPUs                              string
	seed                                                uint64
	warmup, measure, timeout                            time.Duration
	gomaxprocs                                          int
	strict                                              bool
	poolSize                                            int
}

// loadConfig is the command line of load.
type loadConfig struct {
	transport, addr, certs, profile, loads, out string
	cfg                                         loadgen.Config
	poolSize                                    int
}

// Omission selects the transport default; an explicit size must be positive.
func checkPoolFlag(fs *flag.FlagSet) error {
	var err error
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "pool-size" && f.Value.(flag.Getter).Get().(int) <= 0 {
			err = fmt.Errorf("%w: -pool-size must be positive", errUsage)
		}
	})
	return err
}

func load(c loadConfig) (int, error) {
	t, err := common.ParseTransport(c.transport)
	if err != nil {
		return 2, fmt.Errorf("%w: %w", errUsage, err)
	}
	options, err := (common.ClientOptions{PoolSize: c.poolSize}).Resolve(t)
	if err != nil {
		return 2, fmt.Errorf("%w: %w", errUsage, err)
	}
	data, err := readFileOr(c.loads, scenarios.Loads)
	if err != nil {
		return 1, err
	}
	profiles, err := loadgen.ParseProfiles(data)
	if err != nil {
		return 1, err
	}
	if c.cfg.Profile, err = loadgen.Lookup(profiles, c.profile); err != nil {
		return 1, err
	}
	b, err := certs.Load(c.certs)
	if err != nil {
		return 1, err
	}
	p, err := b.Pair()
	if err != nil {
		return 1, err
	}
	ev := newEvents(os.Stdout)
	tlsConf := p.Client.Clone()
	tlsConf.VerifyConnection = ev.verify
	c.cfg.Dial = func() (common.Client, error) { return common.Dial(t, c.addr, tlsConf, options) }
	c.cfg.OnPhase = ev.phase
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	records, runErr := loadgen.Run(ctx, c.cfg)
	if records == nil {
		return 1, runErr
	}
	f, err := os.Create(c.out)
	if err != nil {
		return 1, err
	}
	if err := errors.Join(loadgen.WriteCSV(f, records), f.Close()); err != nil {
		return 1, err
	}
	fmt.Fprint(os.Stderr, loadgen.FormatSummary(loadgen.Summarize(records)))
	return 0, runErr
}

func serve(t common.Transport, addr, certsDir string) (int, error) {
	b, err := certs.Load(certsDir)
	if err != nil {
		return 1, err
	}
	p, err := b.Pair()
	if err != nil {
		return 1, err
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	srv, err := common.Listen(t, addr, p.Server)
	if err != nil {
		return 1, err
	}
	fmt.Println("listening on", srv.Addr())
	<-stop
	srv.Stop()
	return 0, nil
}

// networkProfiles reads the network profiles of file, or the bundled ones:
// the profiles and the points of block 1.
func networkProfiles(file string) ([]netns.Profile, error) {
	if file == "" {
		return netns.ParseProfiles(scenarios.Profiles, scenarios.Points)
	}
	data, err := os.ReadFile(file) //nolint:gosec // the file the user named
	if err != nil {
		return nil, err
	}
	return netns.ParseProfiles(data)
}

func readFileOr(file string, bundled []byte) ([]byte, error) {
	if file == "" {
		return bundled, nil
	}
	return os.ReadFile(file) //nolint:gosec // the file the user named
}
