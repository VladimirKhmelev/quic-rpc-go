//go:build linux

package netns

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// envState tells a process where it runs: unset outside a testbed,
// stateInit in the process Enter started, stateReady in commands run
// inside a testbed.
const (
	envState   = "QRPC_TESTBED"
	stateInit  = "init"
	stateReady = "ready"
)

// exitUnavailable is the exit code of a re-executed process that could not
// set up its namespaces.
const exitUnavailable = 125

// ErrUnavailable means that this machine does not let an unprivileged user
// create user namespaces.
var ErrUnavailable = errors.New("netns: user namespaces are unavailable " +
	"(on Ubuntu: sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0)")

// Inside reports whether the process runs inside a testbed.
func Inside() bool { return os.Getenv(envState) != "" }

// Enter moves the program into namespaces of its own, where it is root and
// can build a testbed without privileges on the host. Everything it builds
// there disappears when it exits.
//
// Called outside such namespaces, Enter starts the program again in new
// user, mount and network namespaces with the same arguments, waits for it
// and exits with its exit code. It returns only if the namespaces cannot be
// created, with ErrUnavailable. Called in the restarted program, Enter
// gives it a private /run/netns for ip netns and returns nil.
func Enter() error {
	if os.Getenv(envState) == stateInit {
		if err := prepare(); err != nil {
			fmt.Fprintf(os.Stderr, "netns: %v\n", err)
			os.Exit(exitUnavailable)
		}
		// Commands started from here on run inside a ready testbed; a
		// nested Enter in one of them starts a nested testbed.
		return os.Setenv(envState, stateReady)
	}
	code, err := reexec()
	if err != nil {
		return err
	}
	os.Exit(code)
	return nil
}

func reexec() (int, error) {
	// The path is /proc/self/exe, the running program itself.
	cmd := exec.Command("/proc/self/exe", os.Args[1:]...) //nolint:gosec // re-executes this very program
	cmd.Args[0] = os.Args[0]
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), envState+"="+stateInit)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWNET,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	stop := relaySignals(cmd.Process)
	defer stop()
	code, err := exitCode(cmd.Wait())
	if code == exitUnavailable {
		return 0, ErrUnavailable
	}
	return code, err
}

// Run runs cmd to completion as a shell does: this process outlives Ctrl-C,
// which reaches cmd from the terminal, and passes SIGTERM on to cmd. It
// returns the exit code of cmd, 128+n if signal n killed it, and 127 if
// cmd could not start.
func Run(cmd *exec.Cmd) (int, error) {
	if err := cmd.Start(); err != nil {
		return 127, err
	}
	stop := relaySignals(cmd.Process)
	defer stop()
	return exitCode(cmd.Wait())
}

// relaySignals keeps this process alive until child ends: Ctrl-C reaches
// child from the terminal by itself, and SIGTERM sent to this process is
// passed on. Handled signals, unlike ignored ones, are reset to the default
// in processes started later.
func relaySignals(child *os.Process) (stop func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				if s == syscall.SIGTERM {
					_ = child.Signal(s)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(sigs)
		close(done)
	}
}

// exitCode returns the exit code of a finished command, 128+n for a command
// killed by signal n, as shells do.
func exitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return 0, err
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return ee.ExitCode(), nil
}

// netnsDir is where ip netns keeps its namespaces.
const netnsDir = "/run/netns"

// prepare makes the mounts of this process private and covers /run/netns
// with an empty tmpfs, so that ip netns sees only this testbed. Without
// /run/netns on the host, it covers /run instead.
func prepare() error {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}
	target := netnsDir
	if _, err := os.Stat(netnsDir); err != nil {
		target = filepath.Dir(netnsDir)
	}
	if err := syscall.Mount("tmpfs", target, "tmpfs", 0, "mode=0755"); err != nil {
		return fmt.Errorf("mount tmpfs on %s: %w", target, err)
	}
	return os.MkdirAll(netnsDir, 0o755) //nolint:gosec // like the /run/netns made by ip
}

// Stand runs the commands that build and change the testbed. It must be
// used inside a testbed (see Enter).
type Stand struct {
	// TCP is the TCP setup of the end hosts; empty means TCPAligned.
	TCP TCP
	// Log, if set, receives every command before it runs.
	Log io.Writer
}

// Up builds the topology and applies p.
func (s *Stand) Up(ctx context.Context, p Profile) error {
	tcp := s.TCP
	if tcp == "" {
		tcp = TCPAligned
	}
	if _, err := ParseTCP(string(tcp)); err != nil {
		return err
	}
	return s.run(ctx, UpCommands(p, tcp))
}

// Apply replaces the impairments with those of p.
func (s *Stand) Apply(ctx context.Context, p Profile) error { return s.run(ctx, ApplyCommands(p)) }

// Outages cuts the link as p.Outage says until ctx ends: an outage of
// p.Outage.Duration starts at first and then every p.Outage.Every. If ctx
// ends during an outage, the impairments of p are brought back. Without an
// outage in p it returns at once.
func (s *Stand) Outages(ctx context.Context, p Profile, first time.Time) error {
	o := p.Outage
	if o.Every == 0 {
		return nil
	}
	sleep := func(until time.Time) bool {
		t := time.NewTimer(time.Until(until))
		defer t.Stop()
		select {
		case <-t.C:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for next := first; ; next = next.Add(o.Every) {
		if !sleep(next) {
			return nil
		}
		if err := s.run(ctx, OutageCommands(p, true)); err != nil {
			return s.endOutage(p, err)
		}
		if !sleep(next.Add(o.Duration)) {
			return s.endOutage(p, nil)
		}
		if err := s.run(ctx, OutageCommands(p, false)); err != nil {
			return s.endOutage(p, err)
		}
	}
}

// endOutage brings the impairments of p back, even after ctx has ended.
func (s *Stand) endOutage(p Profile, err error) error {
	return errors.Join(err, s.run(context.Background(), OutageCommands(p, false)))
}

// Down removes the testbed. It is optional: the testbed also disappears
// when the process that built it exits.
func (s *Stand) Down(ctx context.Context) error { return s.run(ctx, DownCommands()) }

func (s *Stand) run(ctx context.Context, cmds []Cmd) error {
	if !Inside() {
		return errors.New("netns: not inside a testbed")
	}
	for _, c := range cmds {
		if s.Log != nil {
			_, _ = fmt.Fprintf(s.Log, "+ %v\n", c)
		}
		if out, err := Command(ctx, c.NS, c.Args[0], c.Args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %w: %s", c, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// Do calls fn on an OS thread switched to namespace ns. Sockets that fn
// opens stay in ns and can be used from any goroutine afterwards; files
// under /proc/thread-self/net show ns.
func Do(ns string, fn func() error) error {
	target, err := os.Open(filepath.Join(netnsDir, ns)) //nolint:gosec // a namespace in the directory of ip netns
	if err != nil {
		return fmt.Errorf("netns: %w", err)
	}
	defer func() { _ = target.Close() }()
	done := make(chan error, 1)
	// A fresh goroutine: if the thread cannot switch back, it stays locked
	// and ends with this goroutine instead of running other code in ns.
	go func() {
		runtime.LockOSThread()
		self, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			runtime.UnlockOSThread()
			done <- fmt.Errorf("netns: %w", err)
			return
		}
		defer func() { _ = self.Close() }()
		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			done <- fmt.Errorf("netns: enter %s: %w", ns, err)
			return
		}
		fnErr := fn()
		if err := unix.Setns(int(self.Fd()), unix.CLONE_NEWNET); err != nil {
			done <- errors.Join(fnErr, fmt.Errorf("netns: leave %s: %w", ns, err))
			return
		}
		runtime.UnlockOSThread()
		done <- fnErr
	}()
	return <-done
}

// Command returns a command that runs in namespace ns, or outside the
// namespaces if ns is empty.
func Command(ctx context.Context, ns, name string, args ...string) *exec.Cmd {
	if ns != "" {
		args = append([]string{"netns", "exec", ns, name}, args...)
		name = "ip"
	}
	return exec.CommandContext(ctx, name, args...) //nolint:gosec // the testbed's own commands
}
