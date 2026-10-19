package netns

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Profile describes the network between the client and the server. RTT is
// split evenly between the two directions; Jitter, Loss and Reorder apply to
// each direction as given (NET-3).
type Profile struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	RTT time.Duration `yaml:"rtt"`
	// Jitter is the standard deviation of the one-way delay. Packets keep
	// their order: a packet is never sent before the one queued ahead of it.
	Jitter time.Duration `yaml:"jitter"`
	Loss   Percent       `yaml:"loss"`
	// LossBurst is the mean length of a loss burst in packets. Zero or one
	// means independent losses; more uses the Gilbert model of netem.
	LossBurst float64 `yaml:"loss_burst"`
	// Reorder is the share of packets sent without the delay, ahead of the
	// packets queued before them.
	Reorder Percent `yaml:"reorder"`
	// Rate is the bottleneck bandwidth; zero means unlimited.
	Rate Rate `yaml:"rate"`
	// QueueBDP is the bottleneck queue in bandwidth-delay products (Rate ×
	// RTT); it is required with Rate.
	QueueBDP float64 `yaml:"queue_bdp"`
	// Outage cuts the link now and then; see Stand.Outages.
	Outage Outage `yaml:"outage"`
}

// Outage cuts the link in both directions for Duration, starting every
// Every.
type Outage struct {
	Duration time.Duration `yaml:"duration"`
	Every    time.Duration `yaml:"every"`
}

// None is the profile without impairments.
var None = Profile{Name: "none", Description: "Без воздействий"}

// Percent is a percentage: 0.5 means 0.5%. In YAML it is written with the
// percent sign.
type Percent float64

// UnmarshalText parses a percentage such as "0.5%".
func (p *Percent) UnmarshalText(text []byte) error {
	s, ok := strings.CutSuffix(string(text), "%")
	if !ok {
		return fmt.Errorf("percentage %q: want a number followed by %%", text)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("percentage %q: want a number followed by %%", text)
	}
	*p = Percent(v)
	return nil
}

// Fraction returns the percentage as a fraction: 0.5% is 0.005.
func (p Percent) Fraction() float64 { return float64(p) / 100 }

func (p Percent) String() string { return strconv.FormatFloat(float64(p), 'f', -1, 64) + "%" }

// Rate is a bandwidth in bits per second.
type Rate uint64

var rateUnits = []struct {
	suffix string
	scale  uint64
}{
	{"gbit", 1e9}, {"mbit", 1e6}, {"kbit", 1e3}, {"bit", 1},
}

// UnmarshalText parses a bandwidth with a decimal unit, as tc does:
// "100Mbit", "1Gbit", "500kbit".
func (r *Rate) UnmarshalText(text []byte) error {
	lower := strings.ToLower(string(text))
	for _, u := range rateUnits {
		s, ok := strings.CutSuffix(lower, u.suffix)
		if !ok {
			continue
		}
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil || v > math.MaxUint64/u.scale {
			break
		}
		*r = Rate(v * u.scale)
		return nil
	}
	return fmt.Errorf("rate %q: want a whole number followed by bit, kbit, Mbit or Gbit", text)
}

func (r Rate) String() string { return strconv.FormatUint(uint64(r), 10) + "bit" }

// BDP returns the bandwidth-delay product in bytes, or 0 without a rate.
func (p Profile) BDP() float64 { return float64(p.Rate) / 8 * p.RTT.Seconds() }

// QueueBytes returns the size of the bottleneck queue in bytes, or 0
// without a rate.
func (p Profile) QueueBytes() int { return int(math.Round(p.QueueBDP * p.BDP())) }

// Gemodel returns the transition probabilities of netem's Gilbert model
// that give the profile's loss rate and mean burst length: every packet in
// the bad state is lost, none in the good one, so a burst lasts 1/r packets
// on average and the stationary share of the bad state, p/(p+r), is the
// loss rate.
func (p Profile) Gemodel() (good2bad, bad2good Percent) {
	loss := p.Loss.Fraction()
	r := 1 / p.LossBurst
	return Percent(100 * loss * r / (1 - loss)), Percent(100 * r)
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// minQueueBytes is the smallest bottleneck queue: two full Ethernet frames.
const minQueueBytes = 2 * (1500 + 14)

// Validate reports values that netem cannot reproduce or that contradict
// each other.
func (p Profile) Validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}
	check(nameRE.MatchString(p.Name), "name %q: want lowercase letters, digits and dashes", p.Name)
	check(p.RTT >= 0, "rtt %v is negative", p.RTT)
	check(p.Jitter >= 0, "jitter %v is negative", p.Jitter)
	check(p.Jitter <= p.RTT/2, "jitter %v exceeds the one-way delay %v", p.Jitter, p.RTT/2)
	check(p.Loss >= 0 && p.Loss < 100, "loss %v: want at least 0%% and below 100%%", p.Loss)
	check(p.LossBurst == 0 || p.LossBurst >= 1, "loss_burst %v: want 0 or at least 1", p.LossBurst)
	check(p.LossBurst <= 1 || p.Loss > 0, "loss_burst %v without loss", p.LossBurst)
	if p.LossBurst > 1 && p.Loss > 0 && p.Loss < 100 {
		good2bad, _ := p.Gemodel()
		check(good2bad <= 100, "loss %v cannot come in bursts of %v packets", p.Loss, p.LossBurst)
	}
	check(p.Reorder >= 0 && p.Reorder <= 100, "reorder %v: want 0%% to 100%%", p.Reorder)
	check(p.Reorder == 0 || p.RTT > 0, "reorder %v needs a delay to overtake: set rtt", p.Reorder)
	check(p.QueueBDP >= 0, "queue_bdp %v is negative", p.QueueBDP)
	if p.Rate > 0 {
		check(p.RTT > 0, "rate %v needs rtt to size the queue in BDPs", p.Rate)
		check(p.QueueBDP > 0, "rate %v needs queue_bdp", p.Rate)
		if p.RTT > 0 && p.QueueBDP > 0 {
			check(p.QueueBytes() >= minQueueBytes, "queue of %d bytes is below %d bytes, two full frames", p.QueueBytes(), minQueueBytes)
		}
	} else {
		check(p.QueueBDP == 0, "queue_bdp %v without rate", p.QueueBDP)
	}
	if o := p.Outage; o != (Outage{}) {
		check(o.Duration > 0 && o.Duration < o.Every, "outage of %v every %v: want a duration above zero and below the period", o.Duration, o.Every)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("profile %q: %w", p.Name, err)
	}
	return nil
}

// ParseProfiles reads profiles in the format of
// testbed/scenarios/profiles.yaml. Unknown fields are errors, and every
// profile must be valid and have a unique name.
func ParseProfiles(data []byte) ([]Profile, error) {
	var file struct {
		Profiles []Profile `yaml:"profiles"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("profiles: %w", err)
	}
	seen := map[string]bool{}
	for _, p := range file.Profiles {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("profile %q is defined twice", p.Name)
		}
		seen[p.Name] = true
	}
	return file.Profiles, nil
}

// Lookup returns the profile with the given name; "none" is None.
func Lookup(profiles []Profile, name string) (Profile, error) {
	if name == None.Name {
		return None, nil
	}
	for _, p := range profiles {
		if p.Name == name {
			return p, nil
		}
	}
	names := make([]string, len(profiles))
	for i, p := range profiles {
		names[i] = p.Name
	}
	return Profile{}, fmt.Errorf("no profile %q; known: %s", name, strings.Join(names, ", "))
}
