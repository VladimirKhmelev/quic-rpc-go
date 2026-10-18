// Package scenarios holds the descriptions of the experiments: for now the
// network profiles of the testbed (NET-7).
package scenarios

import _ "embed"

// Profiles is profiles.yaml, the network profiles applied by name.
//
//go:embed profiles.yaml
var Profiles []byte
