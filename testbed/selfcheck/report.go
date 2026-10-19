package selfcheck

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
)

// Check is one measured value against its target.
type Check struct {
	Name      string  `json:"name"`
	Direction string  `json:"direction,omitempty"`
	Unit      string  `json:"unit"`
	Target    float64 `json:"target"`
	Measured  float64 `json:"measured"`
	Samples   int     `json:"samples"`
	OK        bool    `json:"ok"`
}

// Report is the outcome of a self-check.
type Report struct {
	Profile string  `json:"profile"`
	Plan    Plan    `json:"plan"`
	Checks  []Check `json:"checks"`
	// Invalid lists why the measurement itself cannot be trusted, such as
	// drops in the probe's own socket buffer. An invalid report says
	// nothing about the testbed either way.
	Invalid []string `json:"invalid,omitempty"`
}

// OK reports whether the measurement is valid and every check passed: only
// then may a series start.
func (r *Report) OK() bool {
	if len(r.Invalid) > 0 {
		return false
	}
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

func (r *Report) add(c Check) {
	c.OK = within(c.Measured, c.Target)
	r.Checks = append(r.Checks, c)
}

func (r *Report) invalid(format string, args ...any) {
	r.Invalid = append(r.Invalid, fmt.Sprintf(format, args...))
}

func (r *Report) String() string {
	status := "OK"
	switch {
	case len(r.Invalid) > 0:
		status = "INVALID MEASUREMENT"
	case !r.OK():
		status = "OUT OF TOLERANCE"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "profile %s: %s\n", r.Profile, status)
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "  check\tdirection\ttarget\tmeasured\tsamples\t")
	for _, c := range r.Checks {
		verdict := "ok"
		if !c.OK {
			verdict = "FAIL"
		}
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s %s\t%s %s\t%d\t%s\n",
			c.Name, c.Direction, number(c.Target), c.Unit, number(c.Measured), c.Unit, c.Samples, verdict)
	}
	_ = w.Flush()
	for _, why := range r.Invalid {
		fmt.Fprintf(&b, "  invalid: %s\n", why)
	}
	return b.String()
}

func number(v float64) string { return strconv.FormatFloat(v, 'g', 4, 64) }
