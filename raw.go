package qcow2

import (
	"io"
	"os"
)

// Raw is a raw disk image used as a read-only backing file.
type Raw struct {
	f    *os.File
	size int64
}

// OpenRaw opens a raw image read-only.
func OpenRaw(name string) (*Raw, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, withCleanup(err, f.Close())
	}
	return &Raw{f: f, size: size}, nil
}

// ReadAt implements io.ReaderAt.
func (r *Raw) ReadAt(p []byte, off int64) (int, error) { return r.f.ReadAt(p, off) }

// Size returns the size of the raw image.
func (r *Raw) Size() int64 { return r.size }

// Close closes the file.
func (r *Raw) Close() error { return r.f.Close() }
