package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
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

// TestServer issues certificates with the command, starts its server and
// makes a call with a client that trusts those certificates.
func TestServer(t *testing.T) {
	dir := t.TempDir()
	if err := command(t, "certs", "-dir", dir).Run(); err != nil {
		t.Fatalf("certs: %v", err)
	}
	b, err := certs.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := b.Pair()
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range common.Transports {
		t.Run(string(tr), func(t *testing.T) {
			srv := command(t, "server", "-transport", string(tr), "-addr", "127.0.0.1:0", "-certs", dir)
			out, err := srv.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := srv.Start(); err != nil {
				t.Fatal(err)
			}
			line, err := bufio.NewReader(out).ReadString('\n')
			addr, ok := strings.CutPrefix(strings.TrimSpace(line), "listening on ")
			if err != nil || !ok {
				t.Fatalf("server said %q, %v", line, err)
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
			if err := srv.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			if err := srv.Wait(); err != nil {
				t.Fatalf("server after SIGTERM: %v", err)
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
