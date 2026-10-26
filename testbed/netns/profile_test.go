package netns

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/testbed/scenarios"
)

func TestBundledProfiles(t *testing.T) {
	profiles, err := ParseProfiles(scenarios.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	// The table of NET-7 in repo-requirements.md.
	want := []Profile{
		{Name: "datacenter", RTT: time.Millisecond},
		{Name: "nearby-zones", RTT: 5 * time.Millisecond, Jitter: time.Millisecond, Loss: 0.1},
		{Name: "interregion", RTT: 150 * time.Millisecond, Jitter: 10 * time.Millisecond, Loss: 0.5, Rate: 100e6, QueueBDP: 1},
		{Name: "congested", RTT: 50 * time.Millisecond, Jitter: 5 * time.Millisecond, Loss: 1, Rate: 10e6, QueueBDP: 4},
		{Name: "degraded", RTT: 100 * time.Millisecond, Jitter: 20 * time.Millisecond, Loss: 2, LossBurst: 4, Reorder: 1, Rate: 10e6, QueueBDP: 1,
			Outage: Outage{Duration: 2 * time.Second, Every: time.Minute}},
	}
	if len(profiles) != len(want) {
		t.Fatalf("got %d profiles, want %d", len(profiles), len(want))
	}
	for i, p := range profiles {
		if p.Description == "" {
			t.Errorf("profile %s has no description", p.Name)
		}
		p.Description = ""
		if p != want[i] {
			t.Errorf("profile %d:\n got %+v\nwant %+v", i, p, want[i])
		}
	}
}

func TestQueueBytes(t *testing.T) {
	tests := []struct {
		p    Profile
		want int
	}{
		{Profile{RTT: 150 * time.Millisecond, Rate: 100e6, QueueBDP: 1}, 1875000},
		{Profile{RTT: 50 * time.Millisecond, Rate: 10e6, QueueBDP: 4}, 250000},
		{Profile{RTT: 100 * time.Millisecond, Rate: 10e6, QueueBDP: 1}, 125000},
		{Profile{RTT: 100 * time.Millisecond}, 0},
	}
	for _, tt := range tests {
		if got := tt.p.QueueBytes(); got != tt.want {
			t.Errorf("QueueBytes(%+v) = %d, want %d", tt.p, got, tt.want)
		}
	}
}

// TestGemodel checks the Gilbert model against its two promises: the
// stationary loss rate and the mean burst length.
func TestGemodel(t *testing.T) {
	for _, p := range []Profile{
		{Loss: 2, LossBurst: 4},
		{Loss: 0.5, LossBurst: 2},
		{Loss: 10, LossBurst: 1.5},
	} {
		good2bad, bad2good := p.Gemodel()
		g, b := good2bad.Fraction(), bad2good.Fraction()
		if loss := g / (g + b); math.Abs(loss-p.Loss.Fraction()) > 1e-12 {
			t.Errorf("%+v: stationary loss %v, want %v", p, loss, p.Loss.Fraction())
		}
		if burst := 1 / b; math.Abs(burst-p.LossBurst) > 1e-12 {
			t.Errorf("%+v: mean burst %v, want %v", p, burst, p.LossBurst)
		}
	}
}

func TestParseUnits(t *testing.T) {
	percents := map[string]float64{"0.5%": 0.5, "0%": 0, "100%": 100, "1e-1%": 0.1}
	for in, want := range percents {
		var p Percent
		if err := p.UnmarshalText([]byte(in)); err != nil || float64(p) != want {
			t.Errorf("Percent %q = %v, %v; want %v", in, p, err, want)
		}
	}
	for _, in := range []string{"0.5", "%", "x%", "NaN%", "Inf%"} {
		var p Percent
		if err := p.UnmarshalText([]byte(in)); err == nil {
			t.Errorf("Percent %q = %v, want an error", in, p)
		}
	}
	rates := map[string]Rate{"100Mbit": 100e6, "1gbit": 1e9, "500kbit": 500e3, "64bit": 64}
	for in, want := range rates {
		var r Rate
		if err := r.UnmarshalText([]byte(in)); err != nil || r != want {
			t.Errorf("Rate %q = %v, %v; want %v", in, r, err, want)
		}
	}
	for _, in := range []string{"100", "Mbit", "1.5Mbit", "-1Mbit", "100MB", "99999999999999Gbit"} {
		var r Rate
		if err := r.UnmarshalText([]byte(in)); err == nil {
			t.Errorf("Rate %q = %v, want an error", in, r)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := Profile{Name: "ok", RTT: 100 * time.Millisecond}
	tests := []struct {
		name   string
		change func(*Profile)
		want   string // part of the error
	}{
		{"name", func(p *Profile) { p.Name = "Inter Region" }, "name"},
		{"negative rtt", func(p *Profile) { p.RTT = -1 }, "rtt"},
		{"jitter above one-way delay", func(p *Profile) { p.Jitter = 51 * time.Millisecond }, "jitter"},
		{"loss of 100%", func(p *Profile) { p.Loss = 100 }, "loss"},
		{"burst below one", func(p *Profile) { p.Loss, p.LossBurst = 1, 0.5 }, "loss_burst"},
		{"burst without loss", func(p *Profile) { p.LossBurst = 4 }, "without loss"},
		{"burst impossible for the loss", func(p *Profile) { p.Loss, p.LossBurst = 90, 2 }, "bursts"},
		{"reorder above 100%", func(p *Profile) { p.Reorder = 101 }, "reorder"},
		{"reorder without delay", func(p *Profile) { p.RTT, p.Reorder = 0, 1 }, "needs a delay"},
		{"rate without queue", func(p *Profile) { p.Rate = 10e6 }, "needs queue_bdp"},
		{"rate without rtt", func(p *Profile) { p.RTT, p.Rate, p.QueueBDP = 0, 10e6, 1 }, "needs rtt"},
		{"queue without rate", func(p *Profile) { p.QueueBDP = 1 }, "without rate"},
		{"queue below two frames", func(p *Profile) { p.Rate, p.QueueBDP = 100e3, 0.1 }, "two full frames"},
		{"outage without a period", func(p *Profile) { p.Outage.Duration = time.Second }, "outage"},
		{"outage as long as its period", func(p *Profile) { p.Outage = Outage{Duration: time.Second, Every: time.Second} }, "outage"},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("base profile: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := ok
			tt.change(&p)
			err := p.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate(%+v) = %v, want an error about %q", p, err, tt.want)
			}
		})
	}
}

func TestParseProfilesRejects(t *testing.T) {
	tests := map[string]string{
		"unknown field": "profiles:\n  - name: a\n    latency: 1ms\n",
		"duplicate":     "profiles:\n  - name: a\n  - name: a\n",
		"invalid":       "profiles:\n  - name: a\n    loss: 0.5\n",
		"not yaml":      "profiles: [",
	}
	for name, in := range tests {
		if _, err := ParseProfiles([]byte(in)); err == nil {
			t.Errorf("%s: parsed %q without an error", name, in)
		}
	}
}

// TestParseProfilesFiles: profiles come from several files, with names
// unique across them.
func TestParseProfilesFiles(t *testing.T) {
	a := []byte("profiles:\n  - name: a\n    rtt: 1ms\n")
	b := []byte("profiles:\n  - name: b\n    rtt: 2ms\n")
	profiles, err := ParseProfiles(a, b)
	if err != nil || len(profiles) != 2 || profiles[0].Name != "a" || profiles[1].RTT != 2*time.Millisecond {
		t.Errorf("ParseProfiles(a, b) = %+v, %v", profiles, err)
	}
	if _, err := ParseProfiles(a, a); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("a profile in two files: %v", err)
	}
}

func TestLookup(t *testing.T) {
	profiles := []Profile{{Name: "a"}, {Name: "b"}}
	if p, err := Lookup(profiles, "b"); err != nil || p.Name != "b" {
		t.Fatalf("Lookup(b) = %+v, %v", p, err)
	}
	if p, err := Lookup(profiles, "none"); err != nil || p != None {
		t.Fatalf("Lookup(none) = %+v, %v", p, err)
	}
	if _, err := Lookup(profiles, "c"); err == nil || !strings.Contains(err.Error(), "a, b") {
		t.Fatalf("Lookup(c) = %v, want an error listing the profiles", err)
	}
}

// TestProfileJSON: a profile written to JSON, as the data of a run records
// it, reads back the same.
func TestProfileJSON(t *testing.T) {
	profiles, err := ParseProfiles(scenarios.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Lookup(profiles, "degraded")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back Profile
	if err := json.Unmarshal(data, &back); err != nil || back != p {
		t.Fatalf("%s read back as %+v, %v", data, back, err)
	}
	if !strings.Contains(string(data), `"loss_pct":2,`) || !strings.Contains(string(data), `"rate_bps":10000000,`) {
		t.Errorf("JSON %s", data)
	}
}
