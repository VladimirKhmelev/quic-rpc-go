//go:build linux

// Command testbed runs programs in the network testbed: three network
// namespaces, client — router — server, with the impairments of a profile
// on the router (see docs/testbed.md). It needs no root.
//
// Usage:
//
//	testbed run [-profile NAME] [-v] -- COMMAND [ARG...]
//	testbed shell [-profile NAME] [-v]
//	testbed check [-profile NAME | -all] [-json FILE] [-v]
//	testbed apply [-v] NAME
//	testbed show NAME
//	testbed profiles
//
// run builds the testbed, runs COMMAND in it and removes the testbed when
// COMMAND exits; inside, commands reach a namespace with "ip netns exec
// client ...". shell does the same with an interactive shell. check builds
// the testbed and measures it against each profile (NET-4); it exits with
// 1 if a value is out of tolerance or a measurement is unreliable. apply
// changes the profile of the testbed it is run in. show prints the commands
// that apply a profile, and profiles lists the profiles. Every command takes
// -profiles FILE to read profiles from FILE instead of the bundled
// testbed/scenarios/profiles.yaml; "none" means no impairments.
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

	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/scenarios"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/selfcheck"
)

const usage = `usage:
  testbed run [-profile NAME] [-v] -- COMMAND [ARG...]
  testbed shell [-profile NAME] [-v]
  testbed check [-profile NAME | -all] [-json FILE] [-v]
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

func run(args []string) (int, error) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2, nil
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	profilesFile := fs.String("profiles", "", "read profiles from `FILE` instead of the bundled ones")
	var (
		profile, jsonFile *string
		verbose, all      *bool
	)
	switch args[0] {
	case "run", "shell":
		profile = fs.String("profile", netns.None.Name, "apply profile `NAME`")
		verbose = fs.Bool("v", false, "print the commands that build the testbed")
	case "check":
		profile = fs.String("profile", "", "check profile `NAME`")
		all = fs.Bool("all", false, "check every profile")
		jsonFile = fs.String("json", "", "also write the reports to `FILE` as JSON")
		verbose = fs.Bool("v", false, "print the progress")
	case "apply":
		verbose = fs.Bool("v", false, "print the commands that change the testbed")
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
		p, err := netns.Lookup(profiles, *profile)
		if err != nil {
			return 1, err
		}
		return runInTestbed(ctx, p, *verbose, command)
	case "check":
		var targets []netns.Profile
		switch {
		case *all && *profile == "" && fs.NArg() == 0:
			targets = profiles
		case !*all && *profile != "" && fs.NArg() == 0:
			p, err := netns.Lookup(profiles, *profile)
			if err != nil {
				return 1, err
			}
			targets = []netns.Profile{p}
		default:
			return 2, fmt.Errorf("%w: check needs -profile NAME or -all", errUsage)
		}
		return check(ctx, targets, *jsonFile, *verbose)
	case "apply":
		p, err := oneProfile(fs, profiles)
		if err != nil {
			return 2, err
		}
		if !netns.Inside() {
			return 1, errors.New("apply works inside a testbed: start one with testbed run or testbed shell")
		}
		return 0, stand(*verbose).Apply(ctx, p)
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

func runInTestbed(ctx context.Context, p netns.Profile, verbose bool, command []string) (int, error) {
	if err := netns.Enter(); err != nil {
		return 1, err
	}
	// From here on this process is root in its own namespaces.
	s := stand(verbose)
	if err := s.Up(ctx, p); err != nil {
		return 1, err
	}
	fmt.Fprintf(os.Stderr, "testbed: profile %s; namespaces %s, %s, %s; server at %v\n",
		p.Name, netns.Client, netns.Router, netns.Server, netns.ServerAddr)
	cmd := exec.CommandContext(ctx, command[0], command[1:]...) //nolint:gosec // running the user's command is the point
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	code, err := netns.Run(cmd)
	if downErr := s.Down(ctx); err == nil {
		err = downErr
	}
	return code, err
}

// check runs the self-check (NET-4) of each profile and fails if any is out
// of tolerance or could not be measured reliably.
func check(ctx context.Context, targets []netns.Profile, jsonFile string, verbose bool) (int, error) {
	if err := netns.Enter(); err != nil {
		return 1, err
	}
	s := stand(false)
	if err := s.Up(ctx, netns.None); err != nil {
		return 1, err
	}
	opts := selfcheck.Options{}
	if verbose {
		opts.Log = os.Stderr
	}
	var reports []*selfcheck.Report
	code := 0
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
		reports = append(reports, r)
	}
	if jsonFile != "" {
		data, err := json.MarshalIndent(reports, "", "  ")
		if err != nil {
			return 1, err
		}
		if err := os.WriteFile(jsonFile, append(data, '\n'), 0o644); err != nil { //nolint:gosec // a report, not a secret
			return 1, err
		}
	}
	return code, s.Down(ctx)
}

func stand(verbose bool) *netns.Stand {
	s := &netns.Stand{}
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
