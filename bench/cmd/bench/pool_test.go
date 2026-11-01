package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
	"github.com/VladimirKhmelev/quic-rpc-go/bench/loadgen"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/scenarios"
)

const pooledVariant = "name: grpc, transport: grpc-h2-pool, tcp: default, pool_size: 2"

func TestPoolSeries(t *testing.T) {
	s := newTestSeries(t)
	s.setMatrix(strings.Replace(testMatrix, "name: grpc, transport: grpc-h2, tcp: default", pooledVariant, 1))
	_, lines, err := s.run(seriesConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if strings.Contains(line, "-transport grpc-h2-pool ") && !strings.Contains(line, "-pool-size 2") {
			t.Fatal(line)
		}
	}
	bytes, err := os.ReadFile(filepath.Join(s.dir, "series.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved seriesFile
	if err := json.Unmarshal(bytes, &saved); err != nil {
		t.Fatal(err)
	}
	want := common.ClientOptions{PoolSize: 2, PoolPolicy: "round-robin"}
	if saved.Matrix.Variants[1].ClientOptions != want {
		t.Fatal("matrix lost pool settings")
	}
	for _, r := range saved.Runs {
		if r.Transport == "grpc-h2-pool" && r.ClientOptions != want {
			t.Fatal("plan lost pool settings")
		}
	}
	s.setMatrix(strings.Replace(strings.Replace(testMatrix, "name: grpc, transport: grpc-h2, tcp: default", pooledVariant, 1), "pool_size: 2", "pool_size: 4", 1))
	if _, lines, err := s.run(seriesConfig{}); err == nil || !strings.Contains(err.Error(), "another matrix") || len(lines) != 0 {
		t.Fatalf("resumed with a different pool: %v, %v", err, lines)
	}
}

// Old series omit the new setup fields. Resuming must recognize them as one
// connection and preserve the stored plan and its completed runs.
func TestLegacySeriesConnectionSetup(t *testing.T) {
	s := newTestSeries(t)
	if _, _, err := s.run(seriesConfig{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, "series.json")
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(bytes, &stored); err != nil {
		t.Fatal(err)
	}
	remove := func(items []any) {
		for _, item := range items {
			fields := item.(map[string]any)
			delete(fields, "pool_size")
			delete(fields, "pool_policy")
		}
	}
	remove(stored["matrix"].(map[string]any)["variants"].([]any))
	remove(stored["runs"].([]any))
	legacy, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, lines, err := s.run(seriesConfig{}); err != nil || len(lines) != 0 {
		t.Fatalf("legacy resume: %v, %v", err, lines)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(legacy) {
		t.Fatal("resume rewrote the historical plan")
	}
}

func TestPoolMatrix(t *testing.T) {
	networks, err := networkProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	loads, err := loadgen.ParseProfiles(scenarios.Loads)
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile("../../../testbed/scenarios/h2pool.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := parseMatrix(bytes, networks, loads)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan(m)) != 264 {
		t.Fatal("wrong pool comparison plan")
	}
	for _, size := range []int{1, 2, 4, 8} {
		found := false
		for _, v := range m.Variants {
			if v.Transport == "grpc-h2-pool" && v.PoolSize == size {
				found = true
			}
		}
		if !found {
			t.Fatalf("no pool of size %d", size)
		}
	}
	for _, edit := range []string{
		"name: grpc, transport: grpc-h2-pool, tcp: aligned, pool_size: -1",
		"name: grpc, transport: grpc-h2-pool, tcp: aligned, pool_policy: least-busy",
		"name: grpc, transport: grpc-h2, tcp: aligned, pool_size: 2",
	} {
		text := strings.Replace(testMatrix, "name: grpc, transport: grpc-h2, tcp: default", edit, 1)
		if _, err := parseMatrix([]byte(text), networks, loads); err == nil {
			t.Fatalf("invalid pool matrix: %s", edit)
		}
	}
}

func TestPoolCLIValidation(t *testing.T) {
	for _, args := range [][]string{
		{"run", "-transport", "grpc-h2-pool", "-load", "small", "-pool-size", "0"},
		{"run", "-transport", "grpc-h2-pool", "-load", "small", "-pool-size", "-1"},
		{"run", "-transport", "quic", "-load", "small", "-pool-size", "2"},
		{"load", "-transport", "grpc-h2-pool", "-addr", "localhost:1", "-certs", "missing", "-profile", "small", "-out", "missing", "-pool-size", "0"},
	} {
		if code, err := run(args); code != 2 || err == nil {
			t.Fatalf("%v: %d, %v", args, code, err)
		}
	}
}
