package selfcheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"
)

var pingTime = regexp.MustCompile(`time=([0-9.]+) ms`)

// parsePing returns the round-trip times of the replies in ping output.
func parsePing(out string) ([]time.Duration, error) {
	var rtts []time.Duration
	for _, m := range pingTime.FindAllStringSubmatch(out, -1) {
		ms, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return nil, fmt.Errorf("ping time %q: %w", m[1], err)
		}
		rtts = append(rtts, time.Duration(ms*float64(time.Millisecond)))
	}
	return rtts, nil
}

// errNotListening means the iperf3 server was not up yet.
var errNotListening = errors.New("iperf3 server is not listening yet")

var notListening = regexp.MustCompile(`unable to (connect|send control message)`)

// minIntervals is how many whole one-second intervals of the receiver a
// goodput needs.
const minIntervals = 3

// parseIperf returns the goodput in bits per second from the JSON output
// of an iperf3 UDP client run with --get-server-output against a server
// with -J: the median over the receiver's one-second intervals, without the
// first one, while the bottleneck queue fills, and the partial last one.
// The totals of iperf3 are not used: the receiver times the test by control
// messages that the impairments delay, which skews them by up to 10%.
func parseIperf(out []byte) (bps float64, intervals int, err error) {
	var r struct {
		Server *struct {
			Intervals []struct {
				Sum struct {
					Start         float64 `json:"start"`
					End           float64 `json:"end"`
					BitsPerSecond float64 `json:"bits_per_second"`
				} `json:"sum"`
			} `json:"intervals"`
		} `json:"server_output_json"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return 0, 0, fmt.Errorf("iperf3 output: %w", err)
	}
	switch {
	case r.Error != "":
		if notListening.MatchString(r.Error) {
			return 0, 0, fmt.Errorf("%w: %s", errNotListening, r.Error)
		}
		return 0, 0, fmt.Errorf("iperf3: %s", r.Error)
	case r.Server == nil:
		return 0, 0, errors.New("iperf3 output has no server_output_json")
	}
	var rates []float64
	for _, iv := range r.Server.Intervals {
		if s := iv.Sum; s.Start >= 1 && s.End-s.Start >= 0.9 {
			rates = append(rates, s.BitsPerSecond)
		}
	}
	if len(rates) < minIntervals {
		return 0, 0, fmt.Errorf("iperf3 reported %d whole intervals after the first second, want %d", len(rates), minIntervals)
	}
	slices.Sort(rates)
	return rates[len(rates)/2], len(rates), nil
}
