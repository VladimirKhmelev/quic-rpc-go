//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
)

// TestLoadProfiles: the bundled profiles hold the profiles and the points of
// block 1; a file of profiles replaces them.
func TestLoadProfiles(t *testing.T) {
	profiles, err := loadProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"interregion", "base", "loss-2"} {
		if _, err := netns.Lookup(profiles, name); err != nil {
			t.Error(err)
		}
	}
	file := filepath.Join(t.TempDir(), "profiles.yaml")
	if err := os.WriteFile(file, []byte("profiles:\n  - name: only\n    rtt: 1ms\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if profiles, err = loadProfiles(file); err != nil || len(profiles) != 1 || profiles[0].Name != "only" {
		t.Errorf("loadProfiles(%s) = %v, %v", file, profiles, err)
	}
}
