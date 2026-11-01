//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
	"github.com/VladimirKhmelev/quic-rpc-go/bench/cpu"
	"github.com/VladimirKhmelev/quic-rpc-go/bench/loadgen"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/env"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
)

// runBench makes a run of 200 ms of warm-up and 1 s of measurement into a
// new directory and returns the directory and meta.json. Where the testbed
// cannot run, it skips the test, unless QRPC_TESTBED_REQUIRED is set.
//
// The environment names a proxy, as on a machine behind one: the calls must
// not take it.
func runBench(t *testing.T, args ...string) (string, meta) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "run")
	cmd := command(t, append([]string{"run", "-out", dir, "-warmup", "200ms", "-duration", "1s"}, args...)...)
	cmd.Env = append(cmd.Env, "HTTPS_PROXY=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9", "NO_PROXY=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), netns.ErrUnavailable.Error()) && os.Getenv("QRPC_TESTBED_REQUIRED") == "" {
			t.Skip(netns.ErrUnavailable)
		}
		t.Fatalf("run: %v\n%s", err, &stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "meta.json")) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	var m meta
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return dir, m
}

func readCalls(t *testing.T, dir string) []loadgen.Record {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "calls.csv")) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	records, err := loadgen.ReadCSV(f)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

// TestRun makes a run of small calls on each transport and checks what it
// records.
func TestRun(t *testing.T) {
	online, err := cpu.Online("/")
	if err != nil {
		t.Fatal(err)
	}
	// Pinned as in a series, where the machine has the CPUs.
	serverCPUs, clientCPUs := "", ""
	if len(online) >= 3 {
		serverCPUs, clientCPUs = "1", "2"
	}
	for _, tr := range common.Transports {
		t.Run(string(tr), func(t *testing.T) {
			t.Parallel()
			dir, m := runBench(t, "-transport", string(tr), "-load", "small", "-server-cpus", serverCPUs, "-client-cpus", clientCPUs)
			options, err := (common.ClientOptions{}).Resolve(tr)
			if err != nil || m.ClientOptions != options {
				t.Fatalf("connection metadata: %+v, want %+v", m.ClientOptions, options)
			}
			for _, f := range []string{"calls.csv", "server.log", "client.log", "certs/ca.pem"} {
				if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
					t.Error(err)
				}
			}
			if m.RunID != "run" || m.Transport != tr || m.Network.Name != netns.None.Name || m.Load.Name != "small" ||
				m.TCP != netns.TCPAligned || m.Seed != 1 || m.Warmup != 200*time.Millisecond || m.Measure != time.Second {
				t.Errorf("parameters %+v", m)
			}
			// go test stamps no commit: the run asks git, if this is a checkout.
			commit, source := "", ""
			if out, err := exec.CommandContext(t.Context(), "git", "rev-parse", "HEAD").Output(); err == nil {
				commit, source = strings.TrimSpace(string(out)), "git"
			}
			if b := m.Build; b.Go != runtime.Version() || b.Commit != commit || b.CommitSource != source ||
				b.Modules["github.com/quic-go/quic-go"] == "" {
				t.Errorf("build %+v, want commit %q from %q", b, commit, source)
			}
			if m.Env == nil || m.Env.Testbed == nil || m.Env.Testbed.TCP != string(netns.TCPAligned) || m.Problems == nil {
				t.Errorf("environment %+v, problems %v", m.Env, m.Problems)
			}
			for _, p := range []struct {
				name string
				p    process
				cpus string
			}{{"server", m.Server, serverCPUs}, {"client", m.Client, clientCPUs}} {
				if p.p.CPUs != p.cpus || (p.cpus != "" && p.p.CPUsAllowed != p.cpus) || !slices.Contains(p.p.Env, "GOMAXPROCS=2") ||
					!slices.Contains(p.p.Env, "QUIC_GO_DISABLE_GSO=true") || p.p.CPU.User+p.p.CPU.System <= 0 {
					t.Errorf("%s %+v, want pinned to %q", p.name, p.p, p.cpus)
				}
			}
			if len(m.TLS) != 1 || m.TLS[0].Version != "TLS 1.3" {
				t.Errorf("TLS %+v", m.TLS)
			}
			// The window is the measurement, 1 s after 200 ms of warm-up. The
			// duration is by the monotonic clock, the times by the wall clock.
			if w := m.Window; (w.Duration-time.Second).Abs() > 100*time.Millisecond ||
				(w.End.Sub(w.Measure)-w.Duration).Abs() > time.Millisecond ||
				(w.Measure.Sub(w.Start)-200*time.Millisecond).Abs() > 100*time.Millisecond {
				t.Errorf("window %+v", m.Window)
			}
			// Every CPU spends the whole window in one state or another.
			if len(m.Cores) != len(online) {
				t.Errorf("%d CPUs, %d online", len(m.Cores), len(online))
			}
			for _, c := range m.Cores {
				total := c.User + c.Nice + c.System + c.Idle + c.IOWait + c.IRQ + c.SoftIRQ + c.Steal
				if diff := total - m.Window.Duration; diff < -150*time.Millisecond || diff > 150*time.Millisecond {
					t.Errorf("CPU %d spent %v in a window of %v", c.ID, total, m.Window.Duration)
				}
			}
			res := m.Results
			if len(res.Kinds) != 1 || res.Kinds[0].Kind != loadgen.KindSmall || res.Kinds[0].Count < 150 || res.Kinds[0].Errors != 0 ||
				res.SentPerSecond <= 0 || res.ReceivedPerSecond != res.SentPerSecond || res.InFlightMean <= 0 || res.InFlightMax < 1 {
				t.Errorf("results %+v", res)
			}
			if n := len(readCalls(t, dir)); n < res.Kinds[0].Count {
				t.Errorf("%d calls in calls.csv, %d measured", n, res.Kinds[0].Count)
			}
		})
	}
}

// A reconnect event creates a whole pool. The command must forward its size
// and record all ten calls under the new logical pool ID.
func TestRunPoolReconnect(t *testing.T) {
	file := filepath.Join(t.TempDir(), "loads.yaml")
	profile := "loads:\n  - name: reconnect-fast\n    small: {min_size: 100, max_size: 100}\n    reconnect: {every: 200ms, calls: 10}\n"
	if err := os.WriteFile(file, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, m := runBench(t, "-transport", "grpc-h2-pool", "-pool-size", "2", "-load", "reconnect-fast", "-loads", file, "-server-cpus", "", "-client-cpus", "")
	if m.PoolSize != 2 || m.PoolPolicy != "round-robin" {
		t.Fatalf("pool metadata: %+v", m.ClientOptions)
	}
	groups := map[int]int{}
	connects := map[int]bool{}
	for _, r := range readCalls(t, dir) {
		if r.Code != 0 {
			t.Fatalf("failed pooled call: %+v", r)
		}
		switch r.Kind {
		case loadgen.KindConnect:
			connects[r.Conn] = true
		case loadgen.KindSmall:
			groups[r.Conn]++
		}
	}
	if len(connects) < 4 {
		t.Fatalf("only %d pool creations", len(connects))
	}
	for conn := range connects {
		if groups[conn] != 10 {
			t.Fatalf("pool %d has %d calls", conn, groups[conn])
		}
	}
}

// cut is a network profile whose outage of 300 ms falls in the middle of
// the measurement of runBench, from 550 to 850 ms.
const cut = `profiles:
  - {name: cut, description: Обрыв, rtt: 2ms, outage: {duration: 300ms, every: 10s}}
`

// TestRunOutage: calls due well inside the outage are answered after it.
func TestRunOutage(t *testing.T) {
	file := filepath.Join(t.TempDir(), "profiles.yaml")
	if err := os.WriteFile(file, []byte(cut), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tr := range common.Transports {
		t.Run(string(tr), func(t *testing.T) {
			t.Parallel()
			dir, m := runBench(t, "-transport", string(tr), "-load", "small", "-profile", "cut", "-profiles", file,
				"-server-cpus", "", "-client-cpus", "")
			ms := time.Millisecond
			if !slices.Equal(m.Outages, []time.Duration{550 * ms}) {
				t.Fatalf("outages at %v, want at 550ms", m.Outages)
			}
			inside := 0
			for _, r := range readCalls(t, dir) {
				if r.Code != 0 {
					t.Errorf("failed call %+v", r)
				}
				if r.Scheduled >= 600*ms && r.Scheduled < 800*ms {
					inside++
					if r.Scheduled+r.Latency < 850*ms {
						t.Errorf("call due at %v answered at %v, inside the outage", r.Scheduled, r.Scheduled+r.Latency)
					}
				}
			}
			// 40 expected at 200/s, with a standard deviation of 6.
			if inside < 15 {
				t.Errorf("%d calls due inside the outage", inside)
			}
		})
	}
}

// TestRunStrict: with -strict, a machine that is not tuned refuses to run,
// before anything is made.
func TestRunStrict(t *testing.T) {
	if len(env.Machine("/").Problems()) == 0 {
		t.Skip("this machine is tuned for a series")
	}
	dir := filepath.Join(t.TempDir(), "run")
	cmd := command(t, "run", "-transport", "quic", "-load", "small", "-strict", "-server-cpus", "", "-client-cpus", "", "-out", dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 || !strings.Contains(stderr.String(), "not tuned for a series (NET-5)") { //nolint:errorlint // the command's own error
		t.Fatalf("run -strict: %v\n%s", err, &stderr)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("run -strict made %s: %v", dir, err)
	}
}

// TestRunCPUs: a CPU that is not online is an error before the testbed is
// built.
func TestRunCPUs(t *testing.T) {
	cmd := command(t, "run", "-transport", "quic", "-load", "small", "-server-cpus", "4095", "-out", filepath.Join(t.TempDir(), "run"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil || !strings.Contains(stderr.String(), "CPU 4095 is not online") {
		t.Fatalf("run on CPU 4095: %v\n%s", err, &stderr)
	}
}
