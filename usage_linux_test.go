package qcow2

import (
	"syscall"
	"testing"
)

// allocated returns the bytes the file system has allocated for path.
func allocated(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks * 512
}

// diskUsage returns allocated(path) and whether the platform reports it.
func diskUsage(t *testing.T, path string) (int64, bool) {
	t.Helper()
	return allocated(t, path), true
}
