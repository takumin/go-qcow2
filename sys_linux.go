//go:build linux

package qcow2

import (
	"errors"
	"io"
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

// seekData returns the start of the first data region at or after off, or
// io.EOF when there is none.
func seekData(f *os.File, off int64) (int64, error) { return seek(f, off, unix.SEEK_DATA) }

// seekHole returns the start of the first hole at or after off; the end of
// the file counts as a hole.
func seekHole(f *os.File, off int64) (int64, error) { return seek(f, off, unix.SEEK_HOLE) }

func seek(f *os.File, off int64, whence int) (int64, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pos int64
	var serr error
	if err := rc.Control(func(fd uintptr) {
		pos, serr = unix.Seek(int(fd), off, whence)
	}); err != nil {
		return 0, err
	}
	switch {
	case errors.Is(serr, unix.ENXIO):
		return 0, io.EOF
	case errors.Is(serr, unix.EINVAL), errors.Is(serr, unix.EOPNOTSUPP):
		return 0, errSeekUnsupported
	}
	return pos, serr
}
