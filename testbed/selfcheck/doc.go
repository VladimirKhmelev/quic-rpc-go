// Package selfcheck measures the running testbed against its profile
// before a series (NET-4): the RTT with ping, losses, loss bursts and
// reordering with a probe of numbered UDP packets, and the bandwidth with
// iperf3. Every value must be within 10% of its target, and a target of
// zero must be met exactly.
//
// The measurements are sized so that a correct testbed fails a check with
// probability about 0.3% (three standard deviations within the tolerance),
// and drops caused by the measurement itself make a report invalid rather
// than failed. The method is described in docs/testbed.md.
package selfcheck
