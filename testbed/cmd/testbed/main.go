//go:build linux

// Command testbed runs programs in the network testbed: three network
// namespaces, client — router — server, with the impairments of a profile
// on the router (see docs/testbed.md). It needs no root.
//
// Usage:
//
//	testbed run [-profile NAME] [-tcp SETUP] [-v] -- COMMAND [ARG...]
//	testbed shell [-profile NAME] [-tcp SETUP] [-v]
//	testbed check [-profile NAME | -all] [-tcp SETUP] [-strict] [-json FILE] [-v]
//	testbed env [-tcp SETUP]
//	testbed apply [-v] NAME
//	testbed show NAME
//	testbed profiles
//
// run builds the testbed, runs COMMAND in it and removes the testbed when
// COMMAND exits; inside, commands reach a namespace with "ip netns exec
// client ...". If the profile has outages, run cuts the link on schedule
// while COMMAND runs. shell does the same with an interactive shell.
//
// check builds the testbed and measures it against each profile (NET-4),
// without the outages; it exits with 1 if a value is out of tolerance or a
// measurement is unreliable, and with -strict also if the machine is not
// tuned for a series (NET-5, see testbed/env/host-setup.sh). env prints the
// environment of a run as JSON: the machine, its tuning and the testbed.
//
// apply changes the profile of the testbed it is run in. show prints the
// commands that apply a profile, and profiles lists the profiles.
//
// SETUP is the TCP setup of the end hosts, aligned (the default) or
// default (NET-6). Every command takes -profiles FILE to read profiles from
// FILE instead of the bundled testbed/scenarios/profiles.yaml; "none" means
// no impairments.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"text/tabwriter"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/testbed/env"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/scenarios"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/selfcheck"
)

const usage = `usage:
  testbed run [-profile NAME] [-tcp SETUP] [-v] -- COMMAND [ARG...]
  testbed shell [-profile NAME] [-tcp SETUP] [-v]
  testbed check [-profile NAME | -all] [-tcp SETUP] [-strict] [-json FILE] [-v]
  testbed env [-tcp SETUP]
  testbed apply [-v] NAME
  testbed show NAME
  testbed profiles
`

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "testbed:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// errUsage reports a malformed command line.
var errUsage = errors.New("bad arguments")

// flags are the options of all commands; each command defines its own.
type flags struct {
	profile, tcp, jsonFile *string
	verbose, all, strict   *bool
}

func run(args []string) (int, error) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2, nil
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	profilesFile := fs.String("profiles", "", "read profiles from `FILE` instead of the bundled ones")
	var f flags
	tcpFlag := func() {
		f.tcp = fs.String("tcp", string(netns.TCPAligned), "TCP setup of the end hosts: aligned or default")
	}
	switch args[0] {
	case "run", "shell":
		f.profile = fs.String("profile", netns.None.Name, "apply profile `NAME`")
		tcpFlag()
		f.verbose = fs.Bool("v", false, "print the commands that build the testbed")
	case "check":
		f.profile = fs.String("profile", "", "check profile `NAME`")
		f.all = fs.Bool("all", false, "check every profile")
		tcpFlag()
		f.strict = fs.Bool("strict", false, "also fail if the machine is not tuned for a series (NET-5)")
		f.jsonFile = fs.String("json", "", "also write the environment and the reports to `FILE` as JSON")
		f.verbose = fs.Bool("v", false, "print the progress")
	case "env":
		tcpFlag()
	case "apply":
		f.verbose = fs.Bool("v", false, "print the commands that change the testbed")
	case "show", "profiles":
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2, nil
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2, nil // flag has printed the error
	}
	profiles, err := loadProfiles(*profilesFile)
	if err != nil {
		return 1, err
	}
	var tcp netns.TCP
	if f.tcp != nil {
		if tcp, err = netns.ParseTCP(*f.tcp); err != nil {
			return 2, fmt.Errorf("%w: %w", errUsage, err)
		}
	}
	ctx := context.Background()

	switch args[0] {
	case "run", "shell":
		command := fs.Args()
		if args[0] == "shell" {
			if len(command) != 0 {
				return 2, fmt.Errorf("%w: shell takes no command", errUsage)
			}
			command = []string{shell()}
		} else if len(command) == 0 {
			return 2, fmt.Errorf("%w: run needs a command", errUsage)
		}
		p, err := netns.Lookup(profiles, *f.profile)
		if err != nil {
			return 1, err
		}
		return runInTestbed(ctx, p, tcp, *f.verbose, command)
	case "check":
		var targets []netns.Profile
		switch {
		case *f.all && *f.profile == "" && fs.NArg() == 0:
			targets = profiles
		case !*f.all && *f.profile != "" && fs.NArg() == 0:
			p, err := netns.Lookup(profiles, *f.profile)
			if err != nil {
				return 1, err
			}
			targets = []netns.Profile{p}
		default:
			return 2, fmt.Errorf("%w: check needs -profile NAME or -all", errUsage)
		}
		return check(ctx, targets, tcp, *f.strict, *f.jsonFile, *f.verbose)
	case "env":
		if fs.NArg() != 0 {
			return 2, fmt.Errorf("%w: env takes no arguments", errUsage)
		}
		return printEnv(ctx, tcp)
	case "apply":
		p, err := oneProfile(fs, profiles)
		if err != nil {
			return 2, err
		}
		if !netns.Inside() {
			return 1, errors.New("apply works inside a testbed: start one with testbed run or testbed shell")
		}
		if p.Outage != (netns.Outage{}) {
			fmt.Fprintln(os.Stderr, "testbed: apply sets the impairments only; outages run with testbed run -profile", p.Name)
		}
		return 0, stand(netns.TCPAligned, *f.verbose).Apply(ctx, p)
	case "show":
		p, err := oneProfile(fs, profiles)
		if err != nil {
			return 2, err
		}
		for _, c := range netns.ApplyCommands(p) {
			fmt.Println(c)
		}
		return 0, nil
	default: // profiles
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, p := range append([]netns.Profile{netns.None}, profiles...) {
			_, _ = fmt.Fprintf(w, "%s\t%s\n", p.Name, p.Description) // a failed write shows in Flush
		}
		return 0, w.Flush()
	}
}

func runInTestbed(ctx context.Context, p netns.Profile, tcp netns.TCP, verbose bool, command []string) (int, error) {
	if err := netns.Enter(); err != nil {
		return 1, err
	}
	// From here on this process is root in its own namespaces.
	s := stand(tcp, verbose)
	if err := s.Up(ctx, p); err != nil {
		return 1, err
	}
	fmt.Fprintf(os.Stderr, "testbed: profile %s, TCP %s; namespaces %s, %s, %s; server at %v\n",
		p.Name, tcp, netns.Client, netns.Router, netns.Server, netns.ServerAddr)
	outagesCtx, stopOutages := context.WithCancel(ctx)
	outages := make(chan error, 1)
	if o := p.Outage; o != (netns.Outage{}) {
		fmt.Fprintf(os.Stderr, "testbed: the link is cut for %v every %v\n", o.Duration, o.Every)
	}
	go func() { outages <- s.Outages(outagesCtx, p) }()

	cmd := exec.CommandContext(ctx, command[0], command[1:]...) //nolint:gosec // running the user's command is the point
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	code, err := netns.Run(cmd)
	stopOutages()
	return code, errors.Join(err, <-outages, s.Down(ctx))
}

// report is what check writes to its JSON file.
type report struct {
	Env     *env.Env            `json:"env"`
	Reports []*selfcheck.Report `json:"reports"`
}

// check runs the self-check (NET-4) of each profile and fails if any is out
// of tolerance or could not be measured reliably, or, if strict, if the
// machine is not tuned (NET-5).
func check(ctx context.Context, targets []netns.Profile, tcp netns.TCP, strict bool, jsonFile string, verbose bool) (int, error) {
	if err := netns.Enter(); err != nil {
		return 1, err
	}
	s := stand(tcp, false)
	if err := s.Up(ctx, netns.None); err != nil {
		return 1, err
	}
	e, err := collectEnv(ctx, tcp)
	if err != nil {
		return 1, err
	}
	code := 0
	if problems := e.Problems(); len(problems) > 0 {
		verdict := "warning"
		if strict {
			verdict, code = "not tuned for a series", 1
		}
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, "testbed: %s: %s\n", verdict, p)
		}
	}
	opts := selfcheck.Options{}
	if verbose {
		opts.Log = os.Stderr
	}
	out := report{Env: e}
	for _, p := range targets {
		if err := s.Apply(ctx, p); err != nil {
			return 1, err
		}
		r, err := selfcheck.Run(ctx, p, opts)
		if err != nil {
			return 1, fmt.Errorf("profile %s: %w", p.Name, err)
		}
		fmt.Print(r)
		if !r.OK() {
			code = 1
		}
		out.Reports = append(out.Reports, r)
	}
	if jsonFile != "" {
		if err := writeJSON(jsonFile, out); err != nil {
			return 1, err
		}
	}
	return code, s.Down(ctx)
}

// printEnv builds the testbed and prints the environment.
func printEnv(ctx context.Context, tcp netns.TCP) (int, error) {
	if err := netns.Enter(); err != nil {
		return 1, err
	}
	s := stand(tcp, false)
	if err := s.Up(ctx, netns.None); err != nil {
		return 1, err
	}
	e, err := collectEnv(ctx, tcp)
	if err != nil {
		return 1, err
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return 1, err
	}
	fmt.Println(string(data))
	return 0, s.Down(ctx)
}

func collectEnv(ctx context.Context, tcp netns.TCP) (*env.Env, error) {
	e := env.Machine("/")
	e.AddTools(ctx)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return e, e.AddTestbed(ctx, tcp)
}

func writeJSON(file string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, append(data, '\n'), 0o644) //nolint:gosec // a report, not a secret
}

func stand(tcp netns.TCP, verbose bool) *netns.Stand {
	s := &netns.Stand{TCP: tcp}
	if verbose {
		s.Log = os.Stderr
	}
	return s
}

func oneProfile(fs *flag.FlagSet, profiles []netns.Profile) (netns.Profile, error) {
	if fs.NArg() != 1 {
		return netns.Profile{}, fmt.Errorf("%w: %s needs one profile name", errUsage, fs.Name())
	}
	return netns.Lookup(profiles, fs.Arg(0))
}

func loadProfiles(file string) ([]netns.Profile, error) {
	data := scenarios.Profiles
	if file != "" {
		f, err := os.Open(file) //nolint:gosec // the file the user named
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		if data, err = io.ReadAll(f); err != nil {
			return nil, err
		}
	}
	return netns.ParseProfiles(data)
}

func shell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}
