package qcow2

import (
	"io"
	"os"
)

// storage is the file holding an image. It exists so that tests can
// simulate crashes and lost writes.
type storage interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
	Truncate(size int64) error
	Close() error
	size() (int64, error)
	punchHole(off, n int64) error
}

type fileStorage struct{ *os.File }

func (s fileStorage) size() (int64, error) {
	fi, err := s.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func (s fileStorage) Sync() error { return datasync(s.File) }

func (s fileStorage) punchHole(off, n int64) error { return punchHole(s.File, off, n) }
