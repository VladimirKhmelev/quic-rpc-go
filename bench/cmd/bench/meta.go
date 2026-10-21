//go:build linux

package main

import (
	"context"
	"os"
	"os/exec"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
	"github.com/VladimirKhmelev/quic-rpc-go/bench/cpu"
	"github.com/VladimirKhmelev/quic-rpc-go/bench/loadgen"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/env"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
)

// meta is meta.json: everything about a run but its calls (GEN-6, GEN-7).
// data/schema.md describes it.
type meta struct {
	RunID     string           `json:"run_id"`
	Transport common.Transport `json:"transport"`
	Network   netns.Profile    `json:"network"`
	Load      loadgen.Profile  `json:"load"`
	TCP       netns.TCP        `json:"tcp"`
	Seed      uint64           `json:"seed"`
	Warmup    time.Duration    `json:"warmup_ns"`
	Measure   time.Duration    `json:"measure_ns"`
	Timeout   time.Duration    `json:"timeout_ns"`
	// Outages are when the outages of the network profile were due to
	// start, since the start of the schedule.
	Outages []time.Duration `json:"outages_ns"`

	Build build    `json:"build"`
	Env   *env.Env `json:"env"`
	// Problems are how the machine differs from NET-5.
	Problems     []string     `json:"problems"`
	Server       process      `json:"server"`
	Client       process      `json:"client"`
	Orchestrator orchestrator `json:"orchestrator"`
	// TLS lists the setups the connections negotiated; one is expected.
	TLS []tlsInfo `json:"tls"`

	Window window `json:"window"`
	// Cores is the time every CPU spent in each state during the window.
	Cores   []cpu.Core `json:"cores"`
	Results results    `json:"results"`
}

// build is the program that ran the server, the client and the
// orchestrator alike.
type build struct {
	Go string `json:"go"`
	// Commit is the git commit of the source and Dirty whether the source
	// differed from it; CommitSource tells where they come from: "binary"
	// (stamped by go build) or "git" (asked at run time, after go run).
	Commit       string `json:"commit"`
	Dirty        bool   `json:"dirty"`
	CommitSource string `json:"commit_source"`
	// Modules are the versions of the modules the program is built of.
	Modules map[string]string `json:"modules"`
}

// process is the server or the client.
type process struct {
	// CPUs are those asked for, empty for no pinning; CPUsAllowed are
	// those the kernel lets the process run on.
	CPUs        string `json:"cpus"`
	CPUsAllowed string `json:"cpus_allowed"`
	// Env are the variables of the process that tune Go and quic-go.
	Env []string `json:"env"`
	// CPU is the time the process used during the window.
	CPU cpu.Times `json:"cpu"`
}

type orchestrator struct {
	CPUsAllowed string `json:"cpus_allowed"`
}

// window is the measurement as the orchestrator saw it: when the load
// generator said the phases began, and the time between the two samples of
// the CPU.
type window struct {
	Start    time.Time     `json:"start"`
	Measure  time.Time     `json:"measure"`
	End      time.Time     `json:"end"`
	Duration time.Duration `json:"duration_ns"`
}

// results sum up the calls of the measurement.
type results struct {
	Kinds []loadgen.KindSummary `json:"kinds"`
	// Payload bytes per second of the answered calls, sent in requests
	// and received in responses.
	SentPerSecond     float64 `json:"sent_bytes_per_s"`
	ReceivedPerSecond float64 `json:"received_bytes_per_s"`
	InFlightMean      float64 `json:"in_flight_mean"`
	InFlightMax       int     `json:"in_flight_max"`
}

// readBuild reads the build of this program. go run does not stamp the
// commit into the binary; git tells it then.
func readBuild(ctx context.Context) build {
	b := build{Modules: map[string]string{}}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return b
	}
	b.Go = info.GoVersion
	for _, m := range info.Deps {
		if m.Replace != nil {
			m = m.Replace
		}
		b.Modules[m.Path] = m.Version
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Commit, b.CommitSource = s.Value, "binary"
		case "vcs.modified":
			b.Dirty = s.Value == "true"
		}
	}
	if b.Commit != "" {
		return b
	}
	head, err1 := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output()
	status, err2 := exec.CommandContext(ctx, "git", "status", "--porcelain").Output()
	if err1 == nil && err2 == nil {
		b.Commit, b.Dirty, b.CommitSource = strings.TrimSpace(string(head)), len(status) > 0, "git"
	}
	return b
}

// tuning are the variables of a process that change how Go and quic-go run.
var tuning = []string{"GOMAXPROCS", "GOGC", "GOMEMLIMIT", "GODEBUG", "QUIC_GO_"}

// processEnv reads the variables of process pid that tune Go and quic-go.
func processEnv(pid int) ([]string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, err
	}
	vars := []string{}
	for v := range strings.SplitSeq(string(b), "\x00") {
		for _, t := range tuning {
			if strings.HasPrefix(v, t) && (strings.HasSuffix(t, "_") || strings.HasPrefix(v, t+"=")) {
				vars = append(vars, v)
			}
		}
	}
	slices.Sort(vars)
	return vars, nil
}
