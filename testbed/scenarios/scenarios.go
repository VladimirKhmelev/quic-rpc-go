// Package scenarios holds the descriptions of the experiments: the network
// profiles of the testbed (NET-7), the points of block 1 (EXP-2) and the
// load profiles of the load generator (GEN-3).
package scenarios

import _ "embed"

// Profiles is profiles.yaml, the network profiles applied by name.
//
//go:embed profiles.yaml
var Profiles []byte

// Points is points.yaml, the points of block 1, applied by name like the
// profiles.
//
//go:embed points.yaml
var Points []byte

// Loads is load.yaml, the load profiles run by name.
//
//go:embed load.yaml
var Loads []byte
