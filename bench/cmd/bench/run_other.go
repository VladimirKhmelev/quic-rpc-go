//go:build !linux

package main

import "errors"

func benchRun(runConfig) (int, error) {
	return 1, errors.New("run needs Linux: the testbed is made of Linux namespaces")
}
