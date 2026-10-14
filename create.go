package qcow2

import (
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
)

// CreateOptions describes a new image.
type CreateOptions struct {
	// Size is the virtual disk size in bytes, rounded up to a multiple of
	// 512 like qemu-img does. When zero and BackingFile is set, the size of
	// the backing file is used.
	Size int64

	// ClusterSize is a power of two between 512 bytes and 2 MiB.
	// Default: 64 KiB.
	ClusterSize int64

	// Version is the qcow2 version, 2 or 3. Default: 3.
	Version int

	// RefcountBits is the refcount width (1, 2, 4, ..., 64). Default: 16.
	// Version 2 images only support 16.
	RefcountBits int

	// BackingFile is stored in the image as is. Relative names are
	// resolved against the directory of the new image.
	BackingFile string

	// BackingFormat is "qcow2" or "raw". When empty it is detected from
	// the backing file and recorded in the image.
	BackingFormat string

	// CompressionType is the algorithm of compressed clusters, "zlib" or
	// "zstd" (version 3 only). Default: zlib.
	CompressionType string

	// LazyRefcounts sets the lazy_refcounts compatible feature (version 3
	// only), so that QEMU may defer refcount updates. This package always
	// updates refcounts right away.
	LazyRefcounts bool

	// Preallocation is one of PreallocOff (the default), PreallocMetadata,
	// PreallocFalloc and PreallocFull. It cannot be combined with a
	// backing file.
	Preallocation string

	// NoPunchHole and WriteThrough have the same meaning as in Options.
	NoPunchHole  bool
	WriteThrough bool
}

// Create creates a new image. It fails if the file already exists.
func Create(name string, opts CreateOptions) (*Image, error) {
	if err := validateCreateOptions(&opts); err != nil {
		return nil, err
	}
	// Creating the file the caller names is the purpose of this function;
	// like qemu-img, the image is created world-readable (subject to umask).
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644) // #nosec G302 G304
	if err != nil {
		return nil, err
	}
	img, err := createIn(fileStorage{f}, name, opts)
	if err != nil {
		return nil, withCleanup(err, f.Close(), os.Remove(name))
	}
	return img, nil
}

// validateCreateOptions fills in defaults and checks the options.
func validateCreateOptions(opts *CreateOptions) error {
	if opts.ClusterSize == 0 {
		opts.ClusterSize = 64 << 10
	}
	if opts.Version == 0 {
		opts.Version = 3
	}
	if opts.RefcountBits == 0 {
		opts.RefcountBits = 16
	}
	cs := opts.ClusterSize
	if cs&(cs-1) != 0 || cs < 1<<minClusterBits || cs > 1<<maxClusterBits {
		return fmt.Errorf("qcow2: invalid cluster size %d", cs)
	}
	if opts.Version != 2 && opts.Version != 3 {
		return fmt.Errorf("qcow2: invalid version %d", opts.Version)
	}
	rb := opts.RefcountBits
	if rb&(rb-1) != 0 || rb > 64 || (opts.Version == 2 && rb != 16) {
		return fmt.Errorf("qcow2: invalid refcount bits %d for version %d", rb, opts.Version)
	}
	if opts.Size < 0 {
		return fmt.Errorf("qcow2: invalid size %d", opts.Size)
	}
	switch opts.CompressionType {
	case "", "zlib":
	case "zstd":
		if opts.Version < 3 {
			return errors.New("qcow2: zstd compression needs a version 3 image")
		}
	default:
		return fmt.Errorf("qcow2: invalid compression type %q", opts.CompressionType)
	}
	if opts.LazyRefcounts && opts.Version < 3 {
		return errors.New("qcow2: lazy refcounts need a version 3 image")
	}
	if err := validPrealloc(opts.Preallocation); err != nil {
		return err
	}
	if opts.Preallocation != "" && opts.Preallocation != PreallocOff && opts.BackingFile != "" {
		return errors.New("qcow2: preallocation cannot be combined with a backing file")
	}
	return nil
}

// createIn opens the backing file and creates the image in f. It closes
// neither on failure.
func createIn(f storage, name string, opts CreateOptions) (*Image, error) {
	if err := validateCreateOptions(&opts); err != nil {
		return nil, err
	}
	var backing Backing
	if opts.BackingFile != "" {
		path := opts.BackingFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(name), path)
		}
		if opts.BackingFormat == "" {
			f, err := probeFormat(path)
			if err != nil {
				return nil, fmt.Errorf("qcow2: opening backing file: %w", err)
			}
			opts.BackingFormat = f
		}
		b, err := openBackingFile(path, opts.BackingFormat, 1)
		if err != nil {
			return nil, fmt.Errorf("qcow2: opening backing file: %w", err)
		}
		backing = b
		if opts.Size == 0 {
			opts.Size = b.Size()
		}
	} else if opts.BackingFormat != "" {
		return nil, errors.New("qcow2: BackingFormat without BackingFile")
	}
	opts.Size = alignUp(opts.Size, 512)
	img, err := create(f, name, opts, backing)
	if err != nil {
		return nil, withCleanup(err, closeBacking(backing))
	}
	return img, nil
}

func closeBacking(b Backing) error {
	if b == nil {
		return nil
	}
	return b.Close()
}

func create(f storage, name string, opts CreateOptions, backing Backing) (*Image, error) {
	h := &header{
		Version:       uint32(opts.Version),
		ClusterBits:   uint32(bits.TrailingZeros64(uint64(opts.ClusterSize))),
		Size:          uint64(opts.Size),
		RefcountOrder: uint32(bits.TrailingZeros(uint(opts.RefcountBits))),
		HeaderLength:  v2HeaderLength,
		backingFile:   opts.BackingFile,
		backingFormat: opts.BackingFormat,
	}
	if opts.Version == 3 {
		h.HeaderLength = v3HeaderLength
	}
	if opts.CompressionType == "zstd" {
		h.CompressionType = compressionZstd
		h.IncompatibleFeatures |= incompatCompressionType
	}
	if opts.LazyRefcounts {
		h.CompatibleFeatures |= compatLazyRefcounts
	}
	img := &Image{
		f: f, name: name, backing: backing, noPunchHole: opts.NoPunchHole,
		opts: Options{NoPunchHole: opts.NoPunchHole, WriteThrough: opts.WriteThrough},
	}
	if err := img.init(h, 0); err != nil {
		return nil, err
	}
	// Fail early if the header cannot fit.
	if _, err := h.marshal(int(img.clusterSize)); err != nil {
		return nil, err
	}

	// Cluster 0 holds the header, cluster 1 the refcount table; refcount
	// blocks are allocated on demand.
	img.fileEnd = 2 * img.clusterSize
	img.refTableOffset = img.clusterSize
	img.refTable = make([]uint64, img.clusterSize/8)
	img.refTableMoved = true
	h.RefcountTableOffset = uint64(img.refTableOffset)
	h.RefcountTableClusters = 1
	// Keep both clusters from being picked as the first refcount block.
	img.reserved[0] = struct{}{}
	img.reserved[img.refTableOffset] = struct{}{}
	if err := img.addRefcount(0, 1); err != nil {
		return nil, err
	}
	if err := img.addRefcount(img.refTableOffset, 1); err != nil {
		return nil, err
	}

	l1Size := img.l1EntriesFor(img.size)
	if l1Size > 32<<20/8 {
		return nil, errors.New("qcow2: virtual size too large for cluster size")
	}
	l1Clusters := max(1, (l1Size*8+img.clusterSize-1)/img.clusterSize)
	off, err := img.allocClusters(l1Clusters)
	if err != nil {
		return nil, err
	}
	img.l1 = make([]uint64, l1Size)
	img.l1Moved = true
	img.l1Offset = off
	h.L1Size = uint32(l1Size)
	h.L1TableOffset = uint64(off)

	if opts.Preallocation != "" && opts.Preallocation != PreallocOff {
		if err := img.preallocate(opts.Preallocation); err != nil {
			return nil, err
		}
	}

	// Write all metadata, then the header: until the header is complete
	// the file is not a qcow2 image at all.
	if err := img.flushAll(); err != nil {
		return nil, err
	}
	buf, err := h.marshal(int(img.clusterSize))
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteAt(buf, 0); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	return img, nil
}

// Resize grows the virtual disk to size bytes (rounded up to a multiple of
// 512). Use Shrink to make it smaller.
// When the image is grown over a range that is still covered by a larger
// backing file, that range is explicitly zeroed so that it reads as zeros,
// like qemu-img resize does.
func (img *Image) Resize(size int64) error {
	img.mu.Lock()
	defer img.mu.Unlock()
	if img.closed {
		return ErrClosed
	}
	if img.readOnly {
		return ErrReadOnly
	}
	size = alignUp(size, 512)
	if size < img.size {
		return fmt.Errorf("%w: shrinking with Resize; use Shrink", ErrUnsupported)
	}
	if size == img.size {
		return nil
	}
	for _, b := range img.bitmaps {
		if !b.managed {
			return fmt.Errorf("%w: resizing with bitmap %q that is in use or unsupported; remove it first", ErrUnsupported, b.name)
		}
	}
	if err := img.beforeGuestChange(); err != nil {
		return err
	}

	need := img.l1EntriesFor(size)
	if need > 32<<20/8 {
		return errors.New("qcow2: virtual size too large for cluster size")
	}
	if cur := int64(len(img.l1)); need > cur {
		curBytes := alignUp(cur*8, img.clusterSize)
		if img.l1Offset == 0 {
			curBytes = 0
		}
		l1 := make([]uint64, need)
		copy(l1, img.l1)
		if need*8 <= curBytes {
			// The new entries fit in the clusters already allocated; they
			// may hold garbage, so write them explicitly.
			for i := cur; i < need; i++ {
				img.l1Dirty[i] = struct{}{}
			}
		} else {
			off, err := img.allocClusters(alignUp(need*8, img.clusterSize) / img.clusterSize)
			if err != nil {
				return err
			}
			for c := int64(0); c < curBytes; c += img.clusterSize {
				img.deferFree(img.l1Offset + c)
			}
			img.l1Offset = off
			img.l1Moved = true
			img.hdr.L1TableOffset = uint64(off)
		}
		img.l1 = l1
		img.hdr.L1Size = uint32(need)
	}

	// The new size reaches the disk only after the L1 table and, below,
	// the L2 tables zeroing the range over the backing file.
	old := img.size
	img.size = size
	img.hdr.Size = uint64(size)
	img.hdrDirty = true
	// The new range counts as changed for enabled bitmaps.
	if err := img.resizeBitmaps(); err != nil {
		return err
	}
	if err := img.markDirty(old, size); err != nil {
		return err
	}

	if img.backing != nil {
		if end := min(size, img.backing.Size()); end > old {
			zero := make([]byte, img.clusterSize)
			for g := old; g < end; {
				n := min(img.clusterSize-g&(img.clusterSize-1), end-g)
				if err := img.writeCluster(g, zero[:n]); err != nil {
					return err
				}
				if err := img.maybeFlush(); err != nil {
					return err
				}
				g += n
			}
		}
	}
	if img.opts.WriteThrough {
		return img.flush()
	}
	return nil
}

// SetBackingFile changes the backing file recorded in the image without
// touching any data, like "qemu-img rebase -u". An empty name removes the
// backing file. When format is empty it is detected from the file. The
// caller is responsible for making sure the new backing file has the same
// contents as the old one for all clusters not allocated in this image.
func (img *Image) SetBackingFile(name, format string) error {
	img.mu.Lock()
	defer img.mu.Unlock()
	if err := img.writable(); err != nil {
		return err
	}
	backing, format, err := img.openNewBacking(name, format)
	if err != nil {
		return err
	}
	return img.switchBacking(name, format, backing)
}

// openNewBacking opens the backing file name (nil for none), detecting
// its format when empty, and returns it with its format.
func (img *Image) openNewBacking(name, format string) (Backing, string, error) {
	if name == "" {
		if format != "" {
			return nil, "", errors.New("qcow2: format without backing file")
		}
		return nil, "", nil
	}
	if format == "" && img.opts.BackingOpener == nil {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(img.name), path)
		}
		f, err := probeFormat(path)
		if err != nil {
			return nil, "", fmt.Errorf("qcow2: opening backing file: %w", err)
		}
		format = f
	}
	b, err := img.openBackingNamed(name, format)
	if err != nil {
		return nil, "", fmt.Errorf("qcow2: opening backing file: %w", err)
	}
	return b, format, nil
}

// switchBacking records a new backing file in the header and replaces the
// opened one. It takes ownership of backing.
func (img *Image) switchBacking(name, format string, backing Backing) error {
	// The header written below carries the in-memory L1 and refcount table
	// locations, so everything they point to must be on disk first.
	err := img.flushAll()
	h := *img.hdr
	h.raw = append([]byte(nil), img.hdr.raw...)
	h.backingFile, h.backingFormat = name, format
	if err == nil {
		err = img.writeHeader(&h)
	}
	if err != nil {
		return withCleanup(err, closeBacking(backing))
	}
	old := img.backing
	img.backing = backing
	if err := closeBacking(old); err != nil {
		return fmt.Errorf("qcow2: backing file changed, but closing the previous one failed: %w", err)
	}
	return nil
}
