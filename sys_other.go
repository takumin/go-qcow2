//go:build !linux

package qcow2

import "os"

func punchHole(*os.File, int64, int64) error { return errPunchUnsupported }

func datasync(f *os.File) error { return f.Sync() }
