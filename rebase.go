package qcow2

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Rebase changes the backing file without changing the guest contents,
// like "qemu-img rebase" in safe mode: every cluster not allocated in the
// image that reads differently from the new backing file (name, which may
// be empty to remove the backing file) is copied from the current one
// into the image first. When format is empty it is detected.
func (img *Image) Rebase(name, format string) error {
	img.mu.Lock()
	defer img.mu.Unlock()
	if err := img.writable(); err != nil {
		return err
	}
	backing, format, err := img.openNewBacking(name, format)
	if err != nil {
		return err
	}
	if err := img.copyBackingDiff(backing); err != nil {
		return withCleanup(err, closeBacking(backing))
	}
	return img.switchBacking(name, format, backing)
}

// copyBackingDiff writes into the image the clusters it does not allocate
// whose contents differ between the current backing file and next.
func (img *Image) copyBackingDiff(next Backing) error {
	cs := img.clusterSize
	cur := make([]byte, cs)
	alt := make([]byte, cs)
	// Past the end of both backing files everything reads as zeros.
	var end int64
	if img.backing != nil {
		end = img.backing.Size()
	}
	if next != nil {
		end = max(end, next.Size())
	}
	for g := int64(0); g < min(img.size, end); {
		n := min(cs-g&(cs-1), img.size-g)
		entry, err := img.l2Entry(g)
		if err != nil {
			return err
		}
		if entry&(flagCompressed|l2OffsetMask) != 0 || img.hdr.Version >= 3 && entry&flagZero != 0 {
			g += n
			continue
		}
		if err := img.readBacking(cur[:n], g); err != nil {
			return err
		}
		if err := readBackingFrom(next, alt[:n], g); err != nil {
			return err
		}
		if !bytes.Equal(cur[:n], alt[:n]) {
			if err := img.writeCluster(g, cur[:n]); err != nil {
				return err
			}
			if err := img.maybeFlush(); err != nil {
				return err
			}
		}
		g += n
	}
	return nil
}

// readBackingFrom fills buf with the contents of b at off, zeros past its
// end or when b is nil.
func readBackingFrom(b Backing, buf []byte, off int64) error {
	n := 0
	if b != nil && off < b.Size() {
		n = int(min(int64(len(buf)), b.Size()-off))
		m, err := b.ReadAt(buf[:n], off)
		if err != nil && (err != io.EOF || m != n) {
			return err
		}
	}
	clear(buf[n:])
	return nil
}

// Commit writes every range allocated in the image into its backing file,
// like "qemu-img commit -d": afterwards the backing file holds the guest
// contents of the image, which is left unchanged. The backing file is
// grown when it is smaller than the image. It must be a qcow2 or raw file
// opened by this package (not through Options.BackingOpener); its other
// users must not have it open.
func (img *Image) Commit() error {
	img.mu.Lock()
	defer img.mu.Unlock()
	if err := img.writable(); err != nil {
		return err
	}
	if img.hdr.backingFile == "" {
		return errors.New("qcow2: image has no backing file")
	}
	if img.opts.BackingOpener != nil {
		return fmt.Errorf("%w: committing to a backing file opened by Options.BackingOpener", ErrUnsupported)
	}
	path := img.hdr.backingFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(img.name), path)
	}
	format := img.hdr.backingFormat
	if format == "" {
		f, err := probeFormat(path)
		if err != nil {
			return err
		}
		format = f
	}
	entries, err := img.mapLocked(0, img.size)
	if err != nil {
		return err
	}

	var dst commitTarget
	switch format {
	case "qcow2":
		b, err := openImage(path, os.O_RDWR, nil, img.depth+1)
		if err != nil {
			return fmt.Errorf("qcow2: opening backing file: %w", err)
		}
		dst = b
	case "raw":
		f, err := os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- the backing file named by the image
		if err != nil {
			return fmt.Errorf("qcow2: opening backing file: %w", err)
		}
		dst = rawTarget{f}
	default:
		return fmt.Errorf("%w: backing format %q", ErrUnsupported, format)
	}
	err = img.commitTo(dst, entries)
	err = errors.Join(err, dst.Close())
	if err != nil {
		return err
	}

	// The opened backing file has stale caches; open it again.
	b, err := img.openBacking()
	if err != nil {
		return fmt.Errorf("qcow2: reopening backing file after commit: %w", err)
	}
	old := img.backing
	img.backing = b
	return closeBacking(old)
}

// commitTarget is a backing file opened for writing.
type commitTarget interface {
	io.WriterAt
	Size() int64
	Resize(size int64) error
	WriteZeroes(off, length int64, unmap bool) error
	Flush() error
	Close() error
}

func (img *Image) commitTo(dst commitTarget, entries []MapEntry) error {
	if dst.Size() < img.size {
		if err := dst.Resize(img.size); err != nil {
			return err
		}
	}
	buf := make([]byte, max(img.clusterSize, 1<<20))
	for _, e := range entries {
		if e.Depth != 0 || !e.Present {
			continue
		}
		if e.Zero {
			if err := dst.WriteZeroes(e.Offset, e.Length, false); err != nil {
				return err
			}
			continue
		}
		for off := e.Offset; off < e.Offset+e.Length; {
			n := min(int64(len(buf)), e.Offset+e.Length-off)
			if err := img.readLocked(buf[:n], off); err != nil {
				return err
			}
			if _, err := dst.WriteAt(buf[:n], off); err != nil {
				return err
			}
			off += n
		}
	}
	return dst.Flush()
}

// rawTarget is a raw backing file opened for writing.
type rawTarget struct{ f *os.File }

func (r rawTarget) WriteAt(p []byte, off int64) (int, error) { return r.f.WriteAt(p, off) }

func (r rawTarget) Size() int64 {
	fi, err := r.f.Stat()
	if err != nil {
		return 0
	}
	return fi.Size()
}

func (r rawTarget) Resize(size int64) error { return r.f.Truncate(size) }

func (r rawTarget) WriteZeroes(off, length int64, _ bool) error {
	zeros := make([]byte, min(length, 1<<20))
	for done := int64(0); done < length; {
		n := min(int64(len(zeros)), length-done)
		if _, err := r.f.WriteAt(zeros[:n], off+done); err != nil {
			return err
		}
		done += n
	}
	return nil
}

func (r rawTarget) Flush() error { return datasync(r.f) }
func (r rawTarget) Close() error { return r.f.Close() }
