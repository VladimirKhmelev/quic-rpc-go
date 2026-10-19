//go:build linux

package env

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
)

var unavailable error

// TestMain runs the tests in a testbed of their own, as netns does.
func TestMain(m *testing.M) {
	if err := netns.Enter(); err != nil {
		if os.Getenv("QRPC_TESTBED_REQUIRED") != "" {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		unavailable = err
	}
	os.Exit(m.Run())
}

func TestParseOffloads(t *testing.T) {
	const out = `Features for eth0:
rx-checksumming: on
tcp-segmentation-offload: off
	tx-tcp-segmentation: off
generic-segmentation-offload: off [requested on]
generic-receive-offload: on
large-receive-offload: off [fixed]
`
	want := map[string]string{"tso": "off", "gso": "off", "gro": "on"}
	if got := parseOffloads(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAddTools(t *testing.T) {
	e := &Env{}
	e.AddTools(t.Context())
	if e.Tools["go"] != runtime.Version() || e.Tools["ip"] == "" || e.Tools["ip"] == "missing" {
		t.Fatalf("tools: %v", e.Tools)
	}
}

func TestAddTestbed(t *testing.T) {
	if unavailable != nil {
		t.Skip(unavailable)
	}
	s := &netns.Stand{TCP: netns.TCPAligned}
	if err := s.Up(t.Context(), netns.None); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Down(context.Background()) }()
	e := &Env{}
	if err := e.AddTestbed(t.Context(), netns.TCPAligned); err != nil {
		t.Fatal(err)
	}
	tb := e.Testbed
	if got := tb.Namespaces[netns.Client].Offloads["eth0"]; !reflect.DeepEqual(got, map[string]string{"tso": "off", "gso": "off", "gro": "on"}) {
		t.Errorf("client eth0 offloads %v", got)
	}
	if got := tb.Namespaces[netns.Router].Offloads["toserver"]["gro"]; got != "off" {
		t.Errorf("router toserver gro %q", got)
	}
	client := tb.Namespaces[netns.Client]
	if !strings.Contains(strings.Join(client.Qdiscs, "\n"), "qdisc fq ") ||
		!strings.Contains(strings.Join(client.Routes, "\n"), "initcwnd 32") ||
		client.Sysctls["net.ipv4.tcp_slow_start_after_idle"] != "0" ||
		client.Sysctls["net.ipv4.tcp_rmem"] != "4096 131072 33554432" {
		t.Errorf("client: %+v", client)
	}
	if tb.Namespaces[netns.Router].Sysctls != nil {
		t.Error("TCP settings recorded for the router")
	}
}
