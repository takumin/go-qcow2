//go:build !linux

package qcow2

import "testing"

func diskUsage(*testing.T, string) (int64, bool) { return 0, false }
