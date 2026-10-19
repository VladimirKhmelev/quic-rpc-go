// Package env records the environment of a run (NET-5, GEN-7): the
// machine, its tuning and the testbed, so that a result can be traced back
// to the exact setup. Problems tells whether the machine is tuned for a
// series; host-setup.sh in this directory tunes it.
package env

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Thresholds of NET-5, shared with host-setup.sh.
const (
	// MaxIdleLatency is the longest exit latency of an idle state a series
	// tolerates: deeper states delay the timers of netem when they wake
	// the CPU, by about 0.2 ms each on the reference machine.
	MaxIdleLatency = 10 * time.Microsecond
	// MinSocketBuffer is the receive buffer quic-go asks for; a lower
	// net.core.rmem_max caps it.
	MinSocketBuffer = 7 << 20
)

// Env is the environment of a run.
type Env struct {
	Time   time.Time `json:"time"`
	Kernel string    `json:"kernel"`
	CPU    string    `json:"cpu"`
	CPUs   []CPU     `json:"cpus"`
	// Turbo is nil if the machine does not tell.
	Turbo *bool `json:"turbo"`
	// OnAC is nil without a mains power supply, as on a desktop or a VM.
	OnAC    *bool      `json:"on_ac"`
	RmemMax int        `json:"rmem_max"`
	WmemMax int        `json:"wmem_max"`
	LoadAvg [3]float64 `json:"load_avg"`
	// Background lists running services that disturb measurements.
	Background []string          `json:"background"`
	Tools      map[string]string `json:"tools,omitempty"`
	Testbed    *Testbed          `json:"testbed,omitempty"`
}

// CPU is the frequency and idle setup of one logical CPU.
type CPU struct {
	ID       int    `json:"id"`
	Governor string `json:"governor,omitempty"`
	EPP      string `json:"epp,omitempty"`
	// IdleStates are all idle states, the disabled ones too.
	IdleStates []IdleState `json:"idle_states,omitempty"`
}

// IdleState is a C-state of a CPU.
type IdleState struct {
	Name     string        `json:"name"`
	Latency  time.Duration `json:"latency"`
	Disabled bool          `json:"disabled"`
}

// Testbed is the setup of the running testbed.
type Testbed struct {
	TCP string `json:"tcp"`
	// Namespaces are the namespaces by name.
	Namespaces map[string]Namespace `json:"namespaces"`
}

// Namespace is the network setup of one namespace.
type Namespace struct {
	// Offloads are the features of each interface that the testbed sets.
	Offloads map[string]map[string]string `json:"offloads"`
	Qdiscs   []string                     `json:"qdiscs"`
	Routes   []string                     `json:"routes"`
	Sysctls  map[string]string            `json:"sysctls,omitempty"`
}

// backgroundServices are processes whose presence makes a machine unfit
// for a series: they wake CPUs and move packets.
var backgroundServices = []string{"dockerd", "containerd"}

// Machine reads the machine from the /sys and /proc trees under root, "/"
// normally; anything it cannot read stays empty.
func Machine(root string) *Env {
	read := func(path string) string {
		b, err := os.ReadFile(filepath.Join(root, path)) //nolint:gosec // fixed paths of /sys and /proc
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	e := &Env{Time: time.Now().UTC(), Kernel: read("proc/sys/kernel/osrelease")}
	for line := range strings.Lines(read("proc/cpuinfo")) {
		if name, model, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(name) == "model name" {
			e.CPU = strings.TrimSpace(model)
			break
		}
	}
	dirs, _ := filepath.Glob(filepath.Join(root, "sys/devices/system/cpu/cpu[0-9]*"))
	for _, dir := range dirs {
		id, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(dir), "cpu"))
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(root, dir)
		c := CPU{ID: id, Governor: read(rel + "/cpufreq/scaling_governor"), EPP: read(rel + "/cpufreq/energy_performance_preference")}
		states, _ := filepath.Glob(filepath.Join(dir, "cpuidle/state[0-9]*"))
		slices.SortFunc(states, func(a, b string) int { return stateNumber(a) - stateNumber(b) })
		for _, st := range states {
			srel, _ := filepath.Rel(root, st)
			us, _ := strconv.Atoi(read(srel + "/latency"))
			c.IdleStates = append(c.IdleStates, IdleState{
				Name: read(srel + "/name"), Latency: time.Duration(us) * time.Microsecond, Disabled: read(srel+"/disable") == "1",
			})
		}
		e.CPUs = append(e.CPUs, c)
	}
	slices.SortFunc(e.CPUs, func(a, b CPU) int { return a.ID - b.ID })
	switch noTurbo, boost := read("sys/devices/system/cpu/intel_pstate/no_turbo"), read("sys/devices/system/cpu/cpufreq/boost"); {
	case noTurbo != "":
		e.Turbo = ptr(noTurbo == "0")
	case boost != "":
		e.Turbo = ptr(boost == "1")
	}
	supplies, _ := filepath.Glob(filepath.Join(root, "sys/class/power_supply/*"))
	for _, s := range supplies {
		rel, _ := filepath.Rel(root, s)
		if read(rel+"/type") == "Mains" {
			e.OnAC = ptr(read(rel+"/online") == "1")
		}
	}
	e.RmemMax, _ = strconv.Atoi(read("proc/sys/net/core/rmem_max"))
	e.WmemMax, _ = strconv.Atoi(read("proc/sys/net/core/wmem_max"))
	for i, f := range strings.Fields(read("proc/loadavg")) {
		if i < len(e.LoadAvg) {
			e.LoadAvg[i], _ = strconv.ParseFloat(f, 64)
		}
	}
	e.Background = []string{}
	comms, _ := filepath.Glob(filepath.Join(root, "proc/[0-9]*/comm"))
	for _, c := range comms {
		rel, _ := filepath.Rel(root, c)
		if name := read(rel); slices.Contains(backgroundServices, name) && !slices.Contains(e.Background, name) {
			e.Background = append(e.Background, name)
		}
	}
	slices.Sort(e.Background)
	return e
}

func stateNumber(dir string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(dir), "state"))
	return n
}

func ptr[T any](v T) *T { return &v }

// Problems lists how the machine differs from NET-5. A value the machine
// does not report counts as a problem too: a series cannot rely on it.
func (e *Env) Problems() []string {
	var problems []string
	if len(e.CPUs) == 0 {
		problems = append(problems, "no CPU information")
	}
	governors := map[string][]int{}
	var deep []string
	for _, c := range e.CPUs {
		if c.Governor != "performance" {
			g := cmp.Or(c.Governor, "unknown")
			governors[g] = append(governors[g], c.ID)
		}
		for _, s := range c.IdleStates {
			if !s.Disabled && s.Latency > MaxIdleLatency && !slices.Contains(deep, s.Name) {
				deep = append(deep, s.Name)
			}
		}
	}
	for _, g := range slices.Sorted(maps.Keys(governors)) {
		problems = append(problems, fmt.Sprintf("governor %s on CPUs %s, want performance", g, ranges(governors[g])))
	}
	if len(deep) > 0 {
		problems = append(problems, fmt.Sprintf("idle states deeper than %v enabled: %s", MaxIdleLatency, strings.Join(deep, ", ")))
	}
	switch {
	case e.Turbo == nil:
		problems = append(problems, "turbo boost state unknown")
	case *e.Turbo:
		problems = append(problems, "turbo boost is on")
	}
	if e.OnAC != nil && !*e.OnAC {
		problems = append(problems, "running on battery")
	}
	if e.RmemMax < MinSocketBuffer || e.WmemMax < MinSocketBuffer {
		problems = append(problems, fmt.Sprintf("socket buffer limits rmem_max %d, wmem_max %d, want at least %d", e.RmemMax, e.WmemMax, MinSocketBuffer))
	}
	for _, s := range e.Background {
		problems = append(problems, s+" is running")
	}
	return problems
}

// ranges writes sorted CPU numbers as ranges: 0-3,8.
func ranges(ids []int) string {
	slices.Sort(ids)
	var parts []string
	for i := 0; i < len(ids); {
		j := i
		for j+1 < len(ids) && ids[j+1] == ids[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, strconv.Itoa(ids[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", ids[i], ids[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}
