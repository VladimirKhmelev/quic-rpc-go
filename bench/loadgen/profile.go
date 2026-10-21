package loadgen

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
)

// Profile is a load profile (GEN-3): what calls a run makes and how often.
// Every part is optional, but a profile needs at least one.
type Profile struct {
	Name        string     `json:"name"                yaml:"name"`
	Description string     `json:"description"         yaml:"description"`
	Small       *Small     `json:"small,omitempty"     yaml:"small"`
	Heavy       *Heavy     `json:"heavy,omitempty"     yaml:"heavy"`
	Echo        *Echo      `json:"echo,omitempty"      yaml:"echo"`
	Reconnect   *Reconnect `json:"reconnect,omitempty" yaml:"reconnect"`
}

// Small are small unary calls arriving as a Poisson process of Rate per
// second. The request and the response of a call both get a size drawn
// uniformly from MinSize to MaxSize bytes. Reconnect draws the sizes of its
// calls from here too.
type Small struct {
	Rate    float64 `json:"rate"     yaml:"rate"`
	MinSize int     `json:"min_size" yaml:"min_size"`
	MaxSize int     `json:"max_size" yaml:"max_size"`
}

// Heavy is a unary call every Every, with an empty request and a response
// of Size bytes.
type Heavy struct {
	Size  int           `json:"size"     yaml:"size"`
	Every time.Duration `json:"every_ns" yaml:"every"`
}

// Echo are messages of Size bytes on one bidi stream, a Poisson process of
// Rate per second; the server sends each back.
type Echo struct {
	Rate float64 `json:"rate" yaml:"rate"`
	Size int     `json:"size" yaml:"size"`
}

// Reconnect opens a new connection every Every and makes Calls small calls
// on it at once.
type Reconnect struct {
	Every time.Duration `json:"every_ns" yaml:"every"`
	Calls int           `json:"calls"    yaml:"calls"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Validate reports values the generator cannot run.
func (p Profile) Validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}
	size := func(n int) bool { return n >= 0 && n <= common.MaxResponse }
	check(nameRE.MatchString(p.Name), "name %q: want lowercase letters, digits and dashes", p.Name)
	check(p.Small != nil && p.Small.Rate > 0 || p.Heavy != nil || p.Echo != nil || p.Reconnect != nil, "no calls")
	if s := p.Small; s != nil {
		check(s.Rate >= 0, "small rate %v is negative", s.Rate)
		check(size(s.MinSize) && size(s.MaxSize) && s.MinSize <= s.MaxSize,
			"small sizes %d to %d: want 0 ≤ min ≤ max ≤ %d", s.MinSize, s.MaxSize, common.MaxResponse)
	}
	if h := p.Heavy; h != nil {
		check(h.Size > 0 && size(h.Size), "heavy size %d: want 1 to %d", h.Size, common.MaxResponse)
		check(h.Every > 0, "heavy period %v is not positive", h.Every)
	}
	if e := p.Echo; e != nil {
		check(e.Rate > 0, "echo rate %v is not positive", e.Rate)
		check(size(e.Size), "echo size %d: want 0 to %d", e.Size, common.MaxResponse)
	}
	if r := p.Reconnect; r != nil {
		check(r.Every > 0 && r.Calls > 0, "reconnect: want a positive period and calls, got %v and %d", r.Every, r.Calls)
		// The connection lives only for its calls; anything else would need
		// a long-lived one.
		check(p.Small != nil && p.Small.Rate == 0 && p.Heavy == nil && p.Echo == nil,
			"reconnect takes the sizes of small with rate 0 and no other calls")
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("load profile %q: %w", p.Name, err)
	}
	return nil
}

// ParseProfiles reads load profiles in the format of
// testbed/scenarios/load.yaml. Unknown fields are errors, and every profile
// must be valid and have a unique name.
func ParseProfiles(data []byte) ([]Profile, error) {
	var file struct {
		Loads []Profile `yaml:"loads"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("load profiles: %w", err)
	}
	seen := map[string]bool{}
	for _, p := range file.Loads {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("load profile %q is defined twice", p.Name)
		}
		seen[p.Name] = true
	}
	return file.Loads, nil
}

// Lookup returns the profile with the given name.
func Lookup(profiles []Profile, name string) (Profile, error) {
	names := make([]string, len(profiles))
	for i, p := range profiles {
		if p.Name == name {
			return p, nil
		}
		names[i] = p.Name
	}
	return Profile{}, fmt.Errorf("no load profile %q; known: %s", name, strings.Join(names, ", "))
}
