package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/certs"
)

// TestMain runs main instead of the tests when the test binary is started
// as the command.
func TestMain(m *testing.M) {
	if os.Getenv("BENCH_AS_COMMAND") == "1" {
		main()
	}
	_ = os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	os.Exit(m.Run())
}

func command(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], args...) //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), "BENCH_AS_COMMAND=1")
	cmd.Stderr = os.Stderr
	return cmd
}

// longCommand is a command that outlives the test's context, which ends
// before the cleanups run; its cleanup stops it.
func longCommand(args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...) //nolint:gosec // the test binary itself, stopped by its cleanup
	cmd.Env = append(os.Environ(), "BENCH_AS_COMMAND=1")
	cmd.Stderr = os.Stderr
	return cmd
}

// startServer issues certificates into a new directory and starts the
// server of the command on it; it returns the directory and the address.
func startServer(t *testing.T, tr common.Transport) (dir, addr string) {
	t.Helper()
	dir = t.TempDir()
	if err := command(t, "certs", "-dir", dir).Run(); err != nil {
		t.Fatalf("certs: %v", err)
	}
	srv := longCommand("server", "-transport", string(tr), "-addr", "127.0.0.1:0", "-certs", dir)
	out, err := srv.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = srv.Process.Signal(syscall.SIGTERM)
		if err := srv.Wait(); err != nil {
			t.Errorf("server after SIGTERM: %v", err)
		}
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	addr, ok := strings.CutPrefix(strings.TrimSpace(line), "listening on ")
	if err != nil || !ok {
		t.Fatalf("server said %q, %v", line, err)
	}
	return dir, addr
}

// TestServer makes a call to the server of the command with a client that
// trusts the certificates of the command.
func TestServer(t *testing.T) {
	for _, tr := range common.Transports {
		t.Run(string(tr), func(t *testing.T) {
			dir, addr := startServer(t, tr)
			b, err := certs.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			p, err := b.Pair()
			if err != nil {
				t.Fatal(err)
			}
			c, err := common.Dial(tr, addr, p.Client)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if n, err := c.Call(ctx, []byte("ping"), 100); err != nil || n != 100 {
				t.Fatalf("Call: %d, %v", n, err)
			}
		})
	}
}

// loads are short profiles of every kind for TestLoad.
const loads = `loads:
  - {name: small-heavy, small: {rate: 200, min_size: 100, max_size: 1000}, heavy: {size: 65536, every: 100ms}}
  - {name: bidi, echo: {rate: 100, size: 200}}
  - {name: reconnect, small: {min_size: 100, max_size: 1000}, reconnect: {every: 200ms, calls: 3}}
`

// TestLoad runs every kind of load against the server of the command and
// checks the CSV it writes.
func TestLoad(t *testing.T) {
	file := filepath.Join(t.TempDir(), "loads.yaml")
	if err := os.WriteFile(file, []byte(loads), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tr := range common.Transports {
		t.Run(string(tr), func(t *testing.T) {
			dir, addr := startServer(t, tr)
			for _, profile := range []string{"small-heavy", "bidi", "reconnect"} {
				out := filepath.Join(t.TempDir(), "calls.csv")
				if err := command(t, "load", "-transport", string(tr), "-addr", addr, "-certs", dir, "-loads", file,
					"-profile", profile, "-warmup", "200ms", "-duration", "800ms", "-out", out).Run(); err != nil {
					t.Fatalf("load %s: %v", profile, err)
				}
				f, err := os.Open(out) //nolint:gosec // the test's own file
				if err != nil {
					t.Fatal(err)
				}
				rows, err := csv.NewReader(f).ReadAll()
				_ = f.Close()
				if err != nil || len(rows) < 10 {
					t.Fatalf("%s: %d rows, %v", profile, len(rows), err)
				}
				if got := strings.Join(rows[0], ","); got != "seq,kind,scheduled_ns,latency_ns,code,req_bytes,resp_bytes,conn,phase" {
					t.Fatalf("header %q", got)
				}
				kinds := map[string]int{}
				for _, row := range rows[1:] {
					if row[4] != "0" {
						t.Errorf("%s: failed call %v", profile, row)
					}
					kinds[row[1]]++
				}
				t.Logf("%s: %v", profile, kinds)
			}
		})
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"fly"},
		{"certs"},
		{"server", "-addr", "127.0.0.1:0", "-certs", t.TempDir()},
		{"server", "-transport", "h3", "-addr", "127.0.0.1:0", "-certs", t.TempDir()},
	} {
		err := command(t, args...).Run()
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 { //nolint:errorlint // the command's own error
			t.Errorf("bench %v: %v, want exit code 2", args, err)
		}
	}
}
