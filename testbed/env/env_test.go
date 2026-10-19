package env

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeMachine writes the files Machine reads: two CPUs of a laptop as
// shipped, on intel_pstate, with docker running.
func fakeMachine(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"proc/sys/kernel/osrelease":  "6.17.0-20-generic",
		"proc/cpuinfo":               "processor\t: 0\nmodel name\t: 12th Gen Intel(R) Core(TM) i5-12450H\n",
		"proc/sys/net/core/rmem_max": "212992",
		"proc/sys/net/core/wmem_max": "212992",
		"proc/loadavg":               "0.52 0.48 0.40 1/900 1234",
		"proc/1/comm":                "systemd",
		"proc/77/comm":               "dockerd",
		"proc/78/comm":               "containerd",
		"sys/devices/system/cpu/intel_pstate/no_turbo": "0",
		"sys/class/power_supply/AC/type":               "Mains",
		"sys/class/power_supply/AC/online":             "1",
		"sys/class/power_supply/BAT0/type":             "Battery",
	}
	for cpu := range 2 {
		dir := "sys/devices/system/cpu/cpu" + strconv.Itoa(cpu)
		files[dir+"/cpufreq/scaling_governor"] = "powersave"
		files[dir+"/cpufreq/energy_performance_preference"] = "balance_performance"
		for i, st := range []struct{ name, latency string }{{"POLL", "0"}, {"C1E", "2"}, {"C6", "170"}, {"C10", "230"}} {
			sd := dir + "/cpuidle/state" + strconv.Itoa(i)
			files[sd+"/name"], files[sd+"/latency"], files[sd+"/disable"] = st.name, st.latency, "0"
		}
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestMachine(t *testing.T) {
	e := Machine(fakeMachine(t))
	if e.Kernel != "6.17.0-20-generic" || e.CPU != "12th Gen Intel(R) Core(TM) i5-12450H" {
		t.Errorf("kernel %q, CPU %q", e.Kernel, e.CPU)
	}
	if len(e.CPUs) != 2 || e.CPUs[1].ID != 1 || e.CPUs[0].Governor != "powersave" || e.CPUs[0].EPP != "balance_performance" {
		t.Fatalf("CPUs: %+v", e.CPUs)
	}
	wantStates := []IdleState{{"POLL", 0, false}, {"C1E", 2 * time.Microsecond, false}, {"C6", 170 * time.Microsecond, false}, {"C10", 230 * time.Microsecond, false}}
	if !reflect.DeepEqual(e.CPUs[0].IdleStates, wantStates) {
		t.Errorf("idle states %+v", e.CPUs[0].IdleStates)
	}
	if e.Turbo == nil || !*e.Turbo || e.OnAC == nil || !*e.OnAC {
		t.Errorf("turbo %v, on AC %v", e.Turbo, e.OnAC)
	}
	if e.RmemMax != 212992 || e.WmemMax != 212992 || e.LoadAvg != [3]float64{0.52, 0.48, 0.40} {
		t.Errorf("buffers %d/%d, load %v", e.RmemMax, e.WmemMax, e.LoadAvg)
	}
	if !reflect.DeepEqual(e.Background, []string{"containerd", "dockerd"}) {
		t.Errorf("background %v", e.Background)
	}
}

func TestProblems(t *testing.T) {
	e := Machine(fakeMachine(t))
	want := []string{
		"governor powersave on CPUs 0-1, want performance",
		"idle states deeper than 10µs enabled: C6, C10",
		"turbo boost is on",
		"socket buffer limits rmem_max 212992, wmem_max 212992, want at least 7340032",
		"containerd is running",
		"dockerd is running",
	}
	if got := e.Problems(); !reflect.DeepEqual(got, want) {
		t.Fatalf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := (&Env{}).Problems(); len(got) == 0 || got[0] != "no CPU information" {
		t.Errorf("an empty environment: %v", got)
	}
	if got := ranges([]int{8, 0, 1, 2, 5, 6}); got != "0-2,5-6,8" {
		t.Errorf("ranges = %q", got)
	}
}

// TestThisMachine reads the real machine: it must find its CPUs.
func TestThisMachine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /sys and /proc")
	}
	e := Machine("/")
	if len(e.CPUs) == 0 {
		t.Fatalf("no CPUs in /sys: %+v", e)
	}
	if e.Kernel == "" || e.RmemMax == 0 {
		t.Fatalf("kernel %q, rmem_max %d", e.Kernel, e.RmemMax)
	}
}
