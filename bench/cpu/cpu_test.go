package cpu

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

// fakeProc writes the files the package reads for process 42 and two CPUs.
func fakeProc(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		// The name holds a space and a parenthesis, as prctl allows.
		"proc/42/stat": "42 (bench (x) y) S 1 42 42 0 -1 4194560 3004 0 0 0 1234 56 0 0 20 0 7 0 897 2462883840 9035 " +
			"18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 2 0 0 0 0 0 0 0 0 0 0 0 0 0\n",
		"proc/42/status": "Name:\tbench\nCpus_allowed:\t00c\nCpus_allowed_list:\t2-3\nMems_allowed_list:\t0\n",
		"proc/stat": "cpu  30 0 20 1000 5 0 7 0 0 0\n" +
			"cpu1 10 0 5 500 2 0 1 0 0 0\n" +
			"cpu0 20 0 15 500 3 1 6 2 0 0\n" +
			"intr 12345 0 0\nctxt 98765\n",
		"sys/devices/system/cpu/online": "0-1\n",
	}
	for name, data := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestFakeProc(t *testing.T) {
	root := fakeProc(t)
	if got, err := Process(root, 42); err != nil || got != (Times{User: 12340 * time.Millisecond, System: 560 * time.Millisecond}) {
		t.Errorf("Process = %+v, %v", got, err)
	}
	cores, err := Cores(root)
	ms := time.Millisecond
	want := []Core{
		{ID: 0, User: 200 * ms, System: 150 * ms, Idle: 5000 * ms, IOWait: 30 * ms, IRQ: 10 * ms, SoftIRQ: 60 * ms, Steal: 20 * ms},
		{ID: 1, User: 100 * ms, System: 50 * ms, Idle: 5000 * ms, IOWait: 20 * ms, SoftIRQ: 10 * ms},
	}
	if err != nil || !reflect.DeepEqual(cores, want) {
		t.Errorf("Cores = %+v, %v\nwant %+v", cores, err, want)
	}
	if got := want[0].Sub(want[1]); got != (Core{ID: 0, User: 100 * ms, System: 100 * ms, IOWait: 10 * ms, IRQ: 10 * ms, SoftIRQ: 50 * ms, Steal: 20 * ms}) {
		t.Errorf("Sub = %+v", got)
	}
	if got, err := Allowed(root, 42); err != nil || !slices.Equal(got, List{2, 3}) {
		t.Errorf("Allowed = %v, %v", got, err)
	}
	if got, err := Online(root); err != nil || !slices.Equal(got, List{0, 1}) {
		t.Errorf("Online = %v, %v", got, err)
	}
	if _, err := Process(root, 43); err == nil {
		t.Error("Process of a missing process")
	}
}

// TestLive reads this process and this machine.
func TestLive(t *testing.T) {
	before, err := Process("/", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	spins := 0
	for start := time.Now(); time.Since(start) < 200*time.Millisecond; {
		spins++
	}
	after, err := Process("/", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if used := after.Sub(before); used.User+used.System < 50*time.Millisecond {
		t.Errorf("a busy loop of 200 ms (%d spins) used %+v", spins, used)
	}
	cores, err := Cores("/")
	if err != nil {
		t.Fatal(err)
	}
	online, err := Online("/")
	if err != nil {
		t.Fatal(err)
	}
	var ids List
	for _, c := range cores {
		ids = append(ids, c.ID)
	}
	if !slices.Equal(ids, online) {
		t.Errorf("/proc/stat has CPUs %v, online are %v", ids, online)
	}
	if allowed, err := Allowed("/", os.Getpid()); err != nil || len(allowed) == 0 {
		t.Errorf("Allowed = %v, %v", allowed, err)
	}
}

func TestList(t *testing.T) {
	for in, want := range map[string]List{"": nil, "3": {3}, "2-3": {2, 3}, "0-2,8,10-11": {0, 1, 2, 8, 10, 11}, "5,4,4-5\n": {4, 5}} {
		got, err := ParseList(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("ParseList(%q) = %v, %v, want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"a", "3-", "-1", "3-2", "1,,2"} {
		if l, err := ParseList(in); err == nil {
			t.Errorf("ParseList(%q) = %v", in, l)
		}
	}
	if got := (List{0, 1, 2, 8, 10, 11}).String(); got != "0-2,8,10-11" {
		t.Errorf("String = %q", got)
	}
}
