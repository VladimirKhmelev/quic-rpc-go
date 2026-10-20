// Package scenarios holds the descriptions of the experiments: the network
// profiles of the testbed (NET-7) and the load profiles of the load
// generator (GEN-3).
package scenarios

import _ "embed"

// Profiles is profiles.yaml, the network profiles applied by name.
//
//go:embed profiles.yaml
var Profiles []byte

// Loads is load.yaml, the load profiles run by name.
//
//go:embed load.yaml
var Loads []byte
