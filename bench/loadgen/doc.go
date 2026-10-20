// Package loadgen is the load generator (GEN-2…GEN-5, GEN-9). It makes the
// calls of a load profile open-loop, on a schedule drawn from a seed that
// does not wait for answers, and records every call: the latency counts
// from the time the call was due, not from when it was sent, so a slow
// answer delays the record of every call that queued behind it instead of
// postponing the calls themselves. Failed and timed-out calls are records
// too. Closed-loop, one call after another, is there only for comparison.
package loadgen
