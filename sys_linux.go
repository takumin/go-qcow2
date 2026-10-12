//go:build linux

package qcow2

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// datasync flushes the file's data and the metadata needed to read it
// back (such as its size), skipping timestamps.
func datasync(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		for {
			serr = unix.Fdatasync(int(fd))
			if serr != unix.EINTR {
				return
			}
		}
	}); err != nil {
		return err
	}
	return serr
}

// punchHole deallocates the given range of f without changing its size.
func punchHole(f *os.File, off, size int64) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) {
		for {
			ferr = unix.Fallocate(int(fd), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, size)
			if ferr != unix.EINTR {
				return
			}
		}
	}); err != nil {
		return err
	}
	if errors.Is(ferr, unix.EOPNOTSUPP) || errors.Is(ferr, unix.ENOSYS) {
		return errPunchUnsupported
	}
	return ferr
}
