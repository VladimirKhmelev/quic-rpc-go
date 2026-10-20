package loadgen

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/scenarios"
)

func bundled(t *testing.T, name string) Profile {
	t.Helper()
	profiles, err := ParseProfiles(scenarios.Loads)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Lookup(profiles, name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBundledProfiles checks load.yaml against the table of load parameters
// in repo-requirements.md.
func TestBundledProfiles(t *testing.T) {
	small := &Small{Rate: 200, MinSize: 100, MaxSize: 1000}
	want := map[string]Profile{
		"small":       {Small: small},
		"small-heavy": {Small: small, Heavy: &Heavy{Size: 1 << 20, Every: time.Second}},
		"bidi":        {Echo: &Echo{Rate: 20, Size: 200}},
		"reconnect":   {Small: &Small{MinSize: 100, MaxSize: 1000}, Reconnect: &Reconnect{Every: 5 * time.Second, Calls: 10}},
	}
	profiles, err := ParseProfiles(scenarios.Loads)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != len(want) {
		t.Fatalf("%d profiles, want %d", len(profiles), len(want))
	}
	for _, p := range profiles {
		w, ok := want[p.Name]
		if !ok || p.Description == "" {
			t.Errorf("unexpected profile %q or no description", p.Name)
			continue
		}
		w.Name, w.Description = p.Name, p.Description
		if !reflect.DeepEqual(p, w) {
			t.Errorf("profile %s:\n got %+v\nwant %+v", p.Name, p, w)
		}
	}
	if _, err := Lookup(profiles, "bulk"); err == nil || !strings.Contains(err.Error(), "small-heavy") {
		t.Errorf("Lookup of an unknown profile: %v", err)
	}
}

func TestValidate(t *testing.T) {
	small := func() *Small { return &Small{Rate: 10, MinSize: 1, MaxSize: 2} }
	tests := []struct {
		name string
		p    Profile
		want string
	}{
		{"name", Profile{Name: "Small", Small: small()}, "name"},
		{"nothing", Profile{Name: "x"}, "no calls"},
		{"sizes reversed", Profile{Name: "x", Small: &Small{Rate: 1, MinSize: 5, MaxSize: 4}}, "small sizes"},
		{"too large", Profile{Name: "x", Heavy: &Heavy{Size: common.MaxResponse + 1, Every: time.Second}}, "heavy size"},
		{"no period", Profile{Name: "x", Heavy: &Heavy{Size: 1}}, "heavy period"},
		{"echo rate", Profile{Name: "x", Echo: &Echo{Size: 1}}, "echo rate"},
		{"reconnect without sizes", Profile{Name: "x", Reconnect: &Reconnect{Every: time.Second, Calls: 1}}, "reconnect takes"},
		{"reconnect with a stream", Profile{Name: "x", Small: &Small{MaxSize: 1}, Echo: &Echo{Rate: 1},
			Reconnect: &Reconnect{Every: time.Second, Calls: 1}}, "reconnect takes"},
	}
	for _, tt := range tests {
		if err := tt.p.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error about %q", tt.name, err, tt.want)
		}
	}
	for _, in := range []string{"loads:\n  - name: a\n    small: {rate: 1, size: 3}\n", "loads:\n  - {name: a, small: {rate: 1}}\n  - {name: a, small: {rate: 1}}\n"} {
		if _, err := ParseProfiles([]byte(in)); err == nil {
			t.Errorf("ParseProfiles(%q) succeeded", in)
		}
	}
}

func TestScheduleIsReproducible(t *testing.T) {
	p := bundled(t, "small-heavy")
	a, b := Schedule(p, 7, 10*time.Second), Schedule(p, 7, 10*time.Second)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("the same seed gave different schedules")
	}
	if reflect.DeepEqual(a, Schedule(p, 8, 10*time.Second)) {
		t.Fatal("another seed gave the same schedule")
	}
	// The small calls do not depend on the heavy one.
	var withHeavy []Event
	for _, ev := range a {
		if ev.Kind == KindSmall {
			withHeavy = append(withHeavy, ev)
		}
	}
	if !reflect.DeepEqual(withHeavy, Schedule(bundled(t, "small"), 7, 10*time.Second)) {
		t.Fatal("adding the heavy call changed the small ones")
	}
}

// TestScheduleStreamsAreIndependent: parts of a profile draw from streams
// of their own; with one stream, the first small call and the first echo
// message of the same rate would arrive at the same time (later ones part,
// as the small calls also draw their sizes).
func TestScheduleStreamsAreIndependent(t *testing.T) {
	p := Profile{Name: "p", Small: &Small{Rate: 50, MinSize: 1, MaxSize: 1}, Echo: &Echo{Rate: 50}}
	first := map[Kind]time.Duration{}
	for _, ev := range Schedule(p, 1, 10*time.Second) {
		if _, ok := first[ev.Kind]; !ok {
			first[ev.Kind] = ev.At
		}
	}
	if len(first) != 2 || first[KindSmall] == first[KindEcho] {
		t.Fatalf("the first small call and echo message arrive at %v", first)
	}
}

func TestScheduleSmall(t *testing.T) {
	const total = 100 * time.Second
	events := Schedule(bundled(t, "small"), 1, total)
	// 200/s for 100 s: 20000 arrivals, standard deviation √20000 ≈ 141.
	if n := len(events); math.Abs(float64(n)-20000) > 3*141 {
		t.Errorf("%d calls in %v at 200/s", n, total)
	}
	var sum, gapSum, gapSq float64
	for i, ev := range events {
		if ev.Kind != KindSmall || ev.ReqSize != ev.RespSize || ev.ReqSize < 100 || ev.ReqSize > 1000 || ev.At >= total {
			t.Fatalf("event %d: %+v", i, ev)
		}
		if i > 0 && ev.At < events[i-1].At {
			t.Fatalf("event %d out of order", i)
		}
		sum += float64(ev.ReqSize)
		if i > 0 {
			gap := (ev.At - events[i-1].At).Seconds()
			gapSum += gap
			gapSq += gap * gap
		}
	}
	// A Poisson process: exponential gaps, whose standard deviation equals
	// their mean; even gaps would have none.
	n := float64(len(events) - 1)
	mean := gapSum / n
	if cv := math.Sqrt(gapSq/n-mean*mean) / mean; math.Abs(cv-1) > 0.05 {
		t.Errorf("gaps vary by %.2f of their mean, want 1 for Poisson arrivals", cv)
	}
	// Uniform on 100…1000: mean 550, standard deviation of the mean
	// 260/√20000 ≈ 1.8.
	if mean := sum / float64(len(events)); math.Abs(mean-550) > 6 {
		t.Errorf("mean size %.1f, want 550", mean)
	}
}

func TestScheduleHeavyAndReconnect(t *testing.T) {
	var heavy []time.Duration
	for _, ev := range Schedule(bundled(t, "small-heavy"), 1, 3500*time.Millisecond) {
		if ev.Kind == KindHeavy {
			heavy = append(heavy, ev.At)
			if ev.ReqSize != 0 || ev.RespSize != 1<<20 {
				t.Errorf("heavy call %+v", ev)
			}
		}
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}; !reflect.DeepEqual(heavy, want) {
		t.Errorf("heavy calls at %v, want %v", heavy, want)
	}

	events := Schedule(bundled(t, "reconnect"), 1, 12*time.Second)
	if len(events) != 3*11 {
		t.Fatalf("%d events, want 3 connections of a connect and 10 calls", len(events))
	}
	for i, ev := range events {
		conn, first := i/11+1, i%11 == 0
		if ev.Conn != conn || ev.At != time.Duration(conn-1)*5*time.Second || (ev.Kind == KindConnect) != first {
			t.Errorf("event %d: %+v", i, ev)
		}
	}
}
