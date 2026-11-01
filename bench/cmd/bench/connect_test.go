package main

import (
	"os"
	"testing"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/loadgen"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/scenarios"
)

func TestBaselineMatrix(t *testing.T) {
	networks, err := networkProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	loads, err := loadgen.ParseProfiles(scenarios.Loads)
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile("../../../testbed/scenarios/baselines.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := parseMatrix(bytes, networks, loads)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan(m)) != 160 {
		t.Fatalf("got %d runs, want 160", len(plan(m)))
	}
	for _, v := range m.Variants {
		if (v.Transport == "grpc-h3" || v.Transport == "connect-h2") && (v.PoolSize != 1 || v.PoolPolicy != "single") {
			t.Fatal("HTTP baselines must use one connection")
		}
	}
}
