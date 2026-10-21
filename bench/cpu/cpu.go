// Package cpu reads from /proc how much CPU time processes and CPUs have
// used (GEN-6), and the lists of CPUs the kernel writes as 0-3,8.
package cpu

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// tick is the unit of the times in /proc: USER_HZ, which Linux fixes at 100
// per second for user space.
const tick = 10 * time.Millisecond

// Times is the CPU time a process has used, in all its threads.
type Times struct {
	User   time.Duration `json:"user_ns"`
	System time.Duration `json:"system_ns"`
}

// Sub returns the time used from u to t.
func (t Times) Sub(u Times) Times { return Times{User: t.User - u.User, System: t.System - u.System} }

// Process reads the CPU time of process pid from the /proc tree under root,
// "/" normally.
func Process(root string, pid int) (Times, error) {
	b, err := os.ReadFile(filepath.Join(root, "proc", strconv.Itoa(pid), "stat")) //nolint:gosec // a file of /proc
	if err != nil {
		return Times{}, fmt.Errorf("cpu: %w", err)
	}
	// The command name, field 2, is in parentheses and may hold spaces
	// and parentheses itself; utime and stime are fields 14 and 15.
	i := bytes.LastIndexByte(b, ')')
	f := strings.Fields(string(b[i+1:]))
	if i < 0 || len(f) < 13 {
		return Times{}, fmt.Errorf("cpu: /proc/%d/stat: %q", pid, b)
	}
	user, err1 := ticks(f[11])
	system, err2 := ticks(f[12])
	if err := errors.Join(err1, err2); err != nil {
		return Times{}, fmt.Errorf("cpu: /proc/%d/stat: %w", pid, err)
	}
	return Times{User: user, System: system}, nil
}

// Core is the time a CPU has spent in each state since boot.
type Core struct {
	ID      int           `json:"id"`
	User    time.Duration `json:"user_ns"`
	Nice    time.Duration `json:"nice_ns"`
	System  time.Duration `json:"system_ns"`
	Idle    time.Duration `json:"idle_ns"`
	IOWait  time.Duration `json:"iowait_ns"`
	IRQ     time.Duration `json:"irq_ns"`
	SoftIRQ time.Duration `json:"softirq_ns"`
	Steal   time.Duration `json:"steal_ns"`
}

func (c *Core) fields() []*time.Duration {
	return []*time.Duration{&c.User, &c.Nice, &c.System, &c.Idle, &c.IOWait, &c.IRQ, &c.SoftIRQ, &c.Steal}
}

// Sub returns the time spent from d to c.
func (c Core) Sub(d Core) Core {
	out := c
	for i, f := range out.fields() {
		*f -= *d.fields()[i]
	}
	return out
}

// Cores reads the time of every CPU from /proc/stat under root, ordered by
// CPU number.
func Cores(root string) ([]Core, error) {
	b, err := os.ReadFile(filepath.Join(root, "proc/stat")) //nolint:gosec // a file of /proc
	if err != nil {
		return nil, fmt.Errorf("cpu: %w", err)
	}
	var cores []Core
	for line := range strings.Lines(string(b)) {
		f := strings.Fields(line)
		// "cpu" alone is the sum of all.
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") || f[0] == "cpu" {
			continue
		}
		c := Core{}
		if c.ID, err = strconv.Atoi(strings.TrimPrefix(f[0], "cpu")); err != nil || len(f) < 9 {
			return nil, fmt.Errorf("cpu: /proc/stat: %q", line)
		}
		for i, field := range c.fields() {
			if *field, err = ticks(f[i+1]); err != nil {
				return nil, fmt.Errorf("cpu: /proc/stat: %w", err)
			}
		}
		cores = append(cores, c)
	}
	if len(cores) == 0 {
		return nil, errors.New("cpu: no CPUs in /proc/stat")
	}
	slices.SortFunc(cores, func(a, b Core) int { return a.ID - b.ID })
	return cores, nil
}

func ticks(s string) (time.Duration, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad time %q", s)
	}
	return time.Duration(n) * tick, nil
}

// List is a set of CPU numbers in ascending order.
type List []int

// ParseList reads a list as the kernel and taskset write it: 0-3,8. The
// empty string is the empty list.
func ParseList(s string) (List, error) {
	var l List
	if s = strings.TrimSpace(s); s == "" {
		return l, nil
	}
	for part := range strings.SplitSeq(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		first, err1 := strconv.Atoi(lo)
		last, err2 := first, error(nil)
		if isRange {
			last, err2 = strconv.Atoi(hi)
		}
		if err1 != nil || err2 != nil || first < 0 || last < first {
			return nil, fmt.Errorf("cpu: bad CPU list %q", s)
		}
		for id := first; id <= last; id++ {
			l = append(l, id)
		}
	}
	slices.Sort(l)
	return slices.Compact(l), nil
}

// String writes l as ParseList reads it.
func (l List) String() string {
	var parts []string
	for i := 0; i < len(l); {
		j := i
		for j+1 < len(l) && l[j+1] == l[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, strconv.Itoa(l[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", l[i], l[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// Online reads the CPUs that are online, under root.
func Online(root string) (List, error) {
	b, err := os.ReadFile(filepath.Join(root, "sys/devices/system/cpu/online")) //nolint:gosec // a file of /sys
	if err != nil {
		return nil, fmt.Errorf("cpu: %w", err)
	}
	return ParseList(string(b))
}

// Allowed reads the CPUs process pid may run on, under root.
func Allowed(root string, pid int) (List, error) {
	b, err := os.ReadFile(filepath.Join(root, "proc", strconv.Itoa(pid), "status")) //nolint:gosec // a file of /proc
	if err != nil {
		return nil, fmt.Errorf("cpu: %w", err)
	}
	for line := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
			return ParseList(v)
		}
	}
	return nil, fmt.Errorf("cpu: no Cpus_allowed_list in /proc/%d/status", pid)
}
