package qcow2

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Storage holds an image that does not live in a file opened by this
// package, such as a buffer in memory or a remote block device.
//
// The image stays consistent after a crash only if Sync makes every
// previous write durable before it returns, like fsync.
type Storage interface {
	io.ReaderAt
	io.WriterAt
	// Size returns the current size in bytes.
	Size() (int64, error)
	// Truncate changes the size; growing it fills with zeros.
	Truncate(size int64) error
	// Sync makes all previous writes durable.
	Sync() error
	// Close releases the storage. Image.Close calls it.
	Close() error
}

// HolePuncher is an optional interface of Storage. PunchHole releases the
// space of a range, which must read as zeros afterwards, without changing
// the size. Freed clusters are punched when the storage implements it and
// Options.NoPunchHole is not set.
type HolePuncher interface {
	PunchHole(off, length int64) error
}

// OpenStorage opens an image held by s, read-only when flag is
// os.O_RDONLY and writable when it is os.O_RDWR. Relative backing file
// names are resolved against the current directory; set
// Options.BackingOpener to open them from somewhere else.
func OpenStorage(s Storage, flag int, opts *Options) (*Image, error) {
	var o Options
	if opts != nil {
		o = *opts
	}
	readOnly := flag&(os.O_WRONLY|os.O_RDWR) == 0
	if o.NoBacking && !readOnly {
		return nil, errors.New("qcow2: NoBacking requires read-only access")
	}
	return openStorage(userStorage{s}, "", readOnly, o, 0)
}

// OpenReaderAt opens a read-only image of the given size read through r,
// such as an HTTP range reader. Closing the image closes r if it
// implements io.Closer.
func OpenReaderAt(r io.ReaderAt, size int64, opts *Options) (*Image, error) {
	return OpenStorage(readerStorage{r, size}, os.O_RDONLY, opts)
}

// CreateStorage creates a new image in s, which must be empty. Relative
// backing file names are resolved against the current directory.
func CreateStorage(s Storage, opts CreateOptions) (*Image, error) {
	size, err := s.Size()
	if err != nil {
		return nil, err
	}
	if size != 0 {
		return nil, fmt.Errorf("qcow2: storage is not empty (%d bytes)", size)
	}
	return createIn(userStorage{s}, "", opts)
}

// storage is the file holding an image. Besides Storage it exists so that
// tests can simulate crashes and lost writes.
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

func (s fileStorage) allocate(off, n int64) error { return allocate(s.File, off, n) }

// userStorage adapts a Storage.
type userStorage struct{ Storage }

func (s userStorage) size() (int64, error) { return s.Size() }

func (s userStorage) punchHole(off, n int64) error {
	if p, ok := s.Storage.(HolePuncher); ok {
		return p.PunchHole(off, n)
	}
	return errPunchUnsupported
}

// readerStorage is a read-only Storage reading from an io.ReaderAt.
type readerStorage struct {
	r    io.ReaderAt
	size int64
}

func (s readerStorage) ReadAt(p []byte, off int64) (int, error) { return s.r.ReadAt(p, off) }
func (s readerStorage) WriteAt([]byte, int64) (int, error)      { return 0, ErrReadOnly }
func (s readerStorage) Size() (int64, error)                    { return s.size, nil }
func (s readerStorage) Truncate(int64) error                    { return ErrReadOnly }
func (s readerStorage) Sync() error                             { return nil }

func (s readerStorage) Close() error {
	if c, ok := s.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
