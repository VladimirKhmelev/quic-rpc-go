package env

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// snapshot returns every file under root with its content.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // the test's own files
		files[path] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestHostSetup runs host-setup.sh on a fake machine: it must fix every
// problem except the services it only warns about, and --restore must put
// every file back as it was.
func TestHostSetup(t *testing.T) {
	root := fakeMachine(t)
	state := filepath.Join(t.TempDir(), "state")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("bash", append([]string{"host-setup.sh"}, args...)...)
		cmd.Env = append(os.Environ(), "HOST_SETUP_ROOT="+root, "HOST_SETUP_STATE="+state)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("host-setup.sh %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	before := snapshot(t, root)

	if out := run("--dry-run"); !strings.Contains(out, "would set /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor: powersave -> performance") {
		t.Fatalf("dry run:\n%s", out)
	}
	if !maps.Equal(snapshot(t, root), before) {
		t.Fatal("the dry run changed files")
	}

	out := run()
	if !strings.Contains(out, "warning: dockerd is running") {
		t.Errorf("no warning about docker:\n%s", out)
	}
	want := []string{"containerd is running", "dockerd is running"}
	if got := Machine(root).Problems(); !reflect.DeepEqual(got, want) {
		t.Fatalf("problems after the setup: %q, want only %q", got, want)
	}
	// A second run changes nothing.
	if out := run(); strings.Contains(out, "set ") {
		t.Errorf("second run changed something:\n%s", out)
	}
	// Someone changes a value between runs; --restore must still put back
	// the value from before the first run.
	governor := filepath.Join(root, "sys/devices/system/cpu/cpu0/cpufreq/scaling_governor")
	if err := os.WriteFile(governor, []byte("schedutil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run()

	run("--restore")
	if after := snapshot(t, root); !maps.Equal(after, before) {
		for path, content := range before {
			if after[path] != content {
				t.Errorf("%s: %q after restore, was %q", path, after[path], content)
			}
		}
	}
	if _, err := os.Stat(state); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("state file left after restore: %v", err)
	}
	if out := run("--restore"); !strings.Contains(out, "nothing to restore") {
		t.Errorf("restore without state:\n%s", out)
	}
}
