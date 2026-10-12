// Package qcow2 reads and writes QEMU qcow2 disk images.
//
// Supported features:
//
//   - qcow2 version 2 and 3, cluster sizes from 512 bytes to 2 MiB
//   - every refcount width (refcount_order 0-6)
//   - zlib and zstd compressed clusters (read; writes decompress the cluster
//     into a freshly allocated one)
//   - zero clusters (version 3)
//   - backing files (qcow2 or raw), including chains of backing files
//   - internal snapshots are preserved: writes honour the COPIED flag and
//     perform copy-on-write for clusters shared with snapshots
//   - creating images, growing images and changing the backing file
//   - persistent dirty bitmaps: kept up to date on writes, readable and
//     manageable (see Bitmaps and DirtyExtents)
//
// Not supported: encryption, external data files and extended L2 entries
// (subclusters).
//
// Clusters whose refcount drops to zero are reused by later allocations.
// On Linux their space is also returned to the file system with
// fallocate(PUNCH_HOLE) unless Options.NoPunchHole is set.
package qcow2

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

var (
	// ErrNotQcow2 is returned when the file does not start with the qcow2 magic.
	ErrNotQcow2 = errors.New("qcow2: not a qcow2 image")
	// ErrUnsupported is returned for images using features this package does not implement.
	ErrUnsupported = errors.New("qcow2: unsupported feature")
	// ErrCorrupt is returned when inconsistent metadata is found.
	ErrCorrupt = errors.New("qcow2: corrupt image")
	// ErrReadOnly is returned when writing to an image opened read-only.
	ErrReadOnly = errors.New("qcow2: image is read-only")
	// ErrClosed is returned when using an image after Close.
	ErrClosed = errors.New("qcow2: image is closed")
	// ErrOutOfRange is returned for writes beyond the virtual disk size.
	ErrOutOfRange = errors.New("qcow2: access beyond end of virtual disk")

	errPunchUnsupported = errors.New("qcow2: hole punching not supported")
)

const (
	l1OffsetMask   = 0x00fffffffffffe00
	l2OffsetMask   = 0x00fffffffffffe00
	refTableMask   = 0xfffffffffffffe00
	flagCopied     = uint64(1) << 63
	flagCompressed = uint64(1) << 62
	flagZero       = uint64(1) << 0

	maxBackingDepth = 64
	cacheBytes      = 32 << 20
)

// Backing is a read-only source of guest data used for clusters that are
// not allocated in an image. *Image implements Backing.
type Backing interface {
	io.ReaderAt
	Size() int64
	Close() error
}

// Options controls how an image is opened.
type Options struct {
	// NoBacking skips opening the backing file. Unallocated clusters then
	// read as zeros. Only allowed for read-only access.
	NoBacking bool

	// BackingOpener, if set, replaces the default logic to open backing
	// files. path is already resolved relative to the image directory and
	// format is the backing format recorded in the image ("" if unknown).
	BackingOpener func(path, format string) (Backing, error)

	// NoPunchHole keeps the space of freed clusters allocated in the file
	// system. By default it is released with fallocate(PUNCH_HOLE) on Linux
	// (other platforms and file systems without support keep it).
	NoPunchHole bool

	// WriteThrough flushes after every WriteAt, Discard and Resize, so
	// that each call is durable when it returns. This costs several syncs
	// per call. Without it, call Flush (or Close) to make changes durable.
	WriteThrough bool
}

// Image is an open qcow2 image. It implements io.ReaderAt and io.WriterAt
// and is safe for concurrent use.
type Image struct {
	mu       sync.Mutex
	f        storage
	name     string
	readOnly bool
	closed   bool

	hdr         *header
	clusterBits uint
	clusterSize int64
	l2Bits      uint
	size        int64

	l1       []uint64
	l1Offset int64

	refTable       []uint64
	refTableOffset int64
	refOrder       uint
	refsPerBlock   int64

	csizeShift  uint
	csizeMask   uint64
	coffsetMask uint64

	l2Cache  *tableCache
	refCache *tableCache

	backing Backing
	opts    Options
	depth   int

	// fileEnd is the cluster-aligned end of the file.
	fileEnd int64
	// freeHint is where the search for free clusters starts; no cluster
	// below it is free.
	freeHint int64
	// reserved holds clusters handed out by reserve whose refcount has not
	// been raised yet.
	reserved map[int64]struct{}
	// noPunchHole disables releasing freed clusters to the file system.
	noPunchHole bool

	// Changes not yet written; see flush.go.
	dataDirty        bool
	l1Dirty          map[int64]struct{}
	l1Moved          bool
	refTableDirty    map[int64]struct{}
	refTableMoved    bool
	hdrRefTableDirty bool
	hdrDirty         bool
	pendingFrees     []int64

	// Persistent dirty bitmaps; see bitmap.go.
	bitmaps         []*bitmap
	removedBitmaps  []*bitmap
	bitmapDirOffset int64
	bitmapDirSize   int64
	bitmapsChanged  bool

	decompEntry uint64
	decompBuf   []byte
}

// Open opens an image read-only.
func Open(name string) (*Image, error) {
	return OpenFile(name, os.O_RDONLY, nil)
}

// OpenFile opens an image with the given flag, which must be os.O_RDONLY or
// os.O_RDWR. Backing files are always opened read-only.
func OpenFile(name string, flag int, opts *Options) (*Image, error) {
	return openImage(name, flag, opts, 0)
}

func openImage(name string, flag int, opts *Options, depth int) (*Image, error) {
	if depth > maxBackingDepth {
		return nil, fmt.Errorf("qcow2: backing chain deeper than %d", maxBackingDepth)
	}
	var o Options
	if opts != nil {
		o = *opts
	}
	readOnly := flag&(os.O_WRONLY|os.O_RDWR) == 0
	if o.NoBacking && !readOnly {
		return nil, errors.New("qcow2: NoBacking requires read-only access")
	}
	fflag := os.O_RDONLY
	if !readOnly {
		fflag = os.O_RDWR
	}
	f, err := os.OpenFile(name, fflag, 0)
	if err != nil {
		return nil, err
	}
	return openStorage(fileStorage{f}, name, readOnly, o, depth)
}

func openStorage(st storage, name string, readOnly bool, o Options, depth int) (*Image, error) {
	img := &Image{f: st, name: name, readOnly: readOnly, opts: o, depth: depth, noPunchHole: o.NoPunchHole}
	if err := img.load(); err != nil {
		st.Close()
		return nil, err
	}
	return img, nil
}

func (img *Image) load() error {
	fileSize, err := img.f.size()
	if err != nil {
		return err
	}
	fixed := make([]byte, v3HeaderLength)
	n, err := img.f.ReadAt(fixed, 0)
	if err != nil && err != io.EOF {
		return err
	}
	if n < v2HeaderLength || binary.BigEndian.Uint32(fixed) != qcowMagic {
		return ErrNotQcow2
	}
	cb := binary.BigEndian.Uint32(fixed[20:])
	if cb < minClusterBits || cb > maxClusterBits {
		return fmt.Errorf("%w: cluster bits %d", ErrUnsupported, cb)
	}
	cluster0 := make([]byte, min(int64(1)<<cb, fileSize))
	if _, err := img.f.ReadAt(cluster0, 0); err != nil && err != io.EOF {
		return err
	}
	h, err := parseHeader(cluster0)
	if err != nil {
		return err
	}
	if err := img.init(h, fileSize); err != nil {
		return err
	}
	if err := img.checkFeatures(); err != nil {
		return err
	}

	// L1 table.
	if h.L1Size > 0 {
		if int64(h.L1Size) > 32<<20/8 {
			return fmt.Errorf("%w: L1 table too large", ErrCorrupt)
		}
		if err := img.readTable(&img.l1, img.l1Offset, int(h.L1Size)); err != nil {
			return fmt.Errorf("qcow2: reading L1 table: %w", err)
		}
	}
	need := img.l1EntriesFor(img.size)
	if int64(len(img.l1)) < need {
		return fmt.Errorf("%w: L1 table too small for virtual size", ErrCorrupt)
	}

	// Refcount table.
	if img.refTableOffset == 0 || img.refTableOffset&(img.clusterSize-1) != 0 {
		return fmt.Errorf("%w: invalid refcount table offset", ErrCorrupt)
	}
	refEntries := int64(h.RefcountTableClusters) * img.clusterSize / 8
	if refEntries > 64<<20/8 {
		return fmt.Errorf("%w: refcount table too large", ErrCorrupt)
	}
	if err := img.readTable(&img.refTable, img.refTableOffset, int(refEntries)); err != nil {
		return fmt.Errorf("qcow2: reading refcount table: %w", err)
	}

	if h.Version >= 3 {
		if err := img.loadBitmapDirectory(); err != nil {
			return err
		}
	}
	if keep := h.AutoclearFeatures & autoclearBitmaps; !img.readOnly && h.AutoclearFeatures != keep {
		// Autoclear bits guard data that writers unaware of it would leave
		// stale; all but the bitmaps bit must be cleared before modifying
		// the image.
		h.AutoclearFeatures = keep
		if err := img.writeUint64(offAutoclearFeatures, keep); err != nil {
			return err
		}
		if err := img.f.Sync(); err != nil {
			return err
		}
	}

	if h.backingFile != "" && !img.opts.NoBacking {
		b, err := img.openBacking()
		if err != nil {
			return fmt.Errorf("qcow2: opening backing file %q: %w", h.backingFile, err)
		}
		img.backing = b
	}
	return nil
}

// init derives the geometry fields from the header.
func (img *Image) init(h *header, fileSize int64) error {
	img.hdr = h
	img.clusterBits = uint(h.ClusterBits)
	img.clusterSize = int64(1) << img.clusterBits
	img.l2Bits = img.clusterBits - 3
	if h.Size > 1<<62 {
		return fmt.Errorf("%w: virtual size too large", ErrCorrupt)
	}
	img.size = int64(h.Size)
	img.l1Offset = int64(h.L1TableOffset)
	img.refTableOffset = int64(h.RefcountTableOffset)
	img.refOrder = uint(h.RefcountOrder)
	img.refsPerBlock = img.clusterSize * 8 >> img.refOrder

	img.csizeShift = 62 - (img.clusterBits - 8)
	img.csizeMask = uint64(1)<<(img.clusterBits-8) - 1
	img.coffsetMask = uint64(1)<<img.csizeShift - 1

	n := max(cacheBytes/int(img.clusterSize), 4)
	img.l2Cache = newTableCache(n)
	img.refCache = newTableCache(n)
	img.fileEnd = alignUp(fileSize, img.clusterSize)
	img.freeHint = 0
	img.reserved = make(map[int64]struct{})
	img.l1Dirty = make(map[int64]struct{})
	img.refTableDirty = make(map[int64]struct{})
	return nil
}

func (img *Image) checkFeatures() error {
	h := img.hdr
	if h.CryptMethod != 0 {
		return fmt.Errorf("%w: encryption", ErrUnsupported)
	}
	if img.l1Offset&(img.clusterSize-1) != 0 {
		return fmt.Errorf("%w: unaligned L1 table", ErrCorrupt)
	}
	known := uint64(incompatDirty | incompatCorrupt | incompatExternalData | incompatCompressionType | incompatExtendedL2)
	if unknown := h.IncompatibleFeatures &^ known; unknown != 0 {
		return fmt.Errorf("%w: unknown incompatible features 0x%x", ErrUnsupported, unknown)
	}
	if h.IncompatibleFeatures&incompatExternalData != 0 || h.hasExtension(extExternalDataFile) {
		return fmt.Errorf("%w: external data file", ErrUnsupported)
	}
	if h.IncompatibleFeatures&incompatExtendedL2 != 0 {
		return fmt.Errorf("%w: extended L2 entries", ErrUnsupported)
	}
	switch h.CompressionType {
	case compressionZlib:
	case compressionZstd:
		if h.IncompatibleFeatures&incompatCompressionType == 0 {
			return fmt.Errorf("%w: compression type set without incompatible bit", ErrCorrupt)
		}
	default:
		return fmt.Errorf("%w: compression type %d", ErrUnsupported, h.CompressionType)
	}
	if !img.readOnly {
		if h.IncompatibleFeatures&incompatCorrupt != 0 {
			return fmt.Errorf("%w: image is marked corrupt; open it read-only or repair it with qemu-img check -r all", ErrCorrupt)
		}
		if h.IncompatibleFeatures&incompatDirty != 0 {
			return fmt.Errorf("%w: image is dirty (lazy refcounts); repair it with qemu-img check -r all", ErrUnsupported)
		}
	}
	return nil
}

func (img *Image) openBacking() (Backing, error) {
	path := img.hdr.backingFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(img.name), path)
	}
	format := img.hdr.backingFormat
	if img.opts.BackingOpener != nil {
		return img.opts.BackingOpener(path, format)
	}
	return openBackingFile(path, format, img.depth+1)
}

func openBackingFile(path, format string, depth int) (Backing, error) {
	if format == "" {
		f, err := probeFormat(path)
		if err != nil {
			return nil, err
		}
		format = f
	}
	switch format {
	case "qcow2":
		return openImage(path, os.O_RDONLY, nil, depth)
	case "raw":
		return OpenRaw(path)
	default:
		return nil, fmt.Errorf("%w: backing format %q", ErrUnsupported, format)
	}
}

// probeFormat returns "qcow2" if the file starts with the qcow2 magic and
// "raw" otherwise.
func probeFormat(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var m [4]byte
	if _, err := io.ReadFull(f, m[:]); err == nil && binary.BigEndian.Uint32(m[:]) == qcowMagic {
		return "qcow2", nil
	}
	return "raw", nil
}

// Size returns the virtual disk size in bytes.
func (img *Image) Size() int64 {
	img.mu.Lock()
	defer img.mu.Unlock()
	return img.size
}

// ClusterSize returns the cluster size in bytes.
func (img *Image) ClusterSize() int64 { return img.clusterSize }

// Version returns the qcow2 format version (2 or 3).
func (img *Image) Version() int { return int(img.hdr.Version) }

// BackingFile returns the backing file name as stored in the image and its
// format. Both are empty when the image has no backing file.
func (img *Image) BackingFile() (name, format string) {
	img.mu.Lock()
	defer img.mu.Unlock()
	return img.hdr.backingFile, img.hdr.backingFormat
}

// Backing returns the opened backing file, or nil.
func (img *Image) Backing() Backing {
	img.mu.Lock()
	defer img.mu.Unlock()
	return img.backing
}

// Flush makes all previous writes durable. Metadata is written in an
// order that keeps the image consistent if the process or the machine
// crashes at any point: after a crash the image contains at least
// everything written before the last successful Flush, and is never
// corrupt (at worst some clusters are leaked).
func (img *Image) Flush() error {
	img.mu.Lock()
	defer img.mu.Unlock()
	if img.closed {
		return ErrClosed
	}
	if img.readOnly {
		return nil
	}
	return img.flush()
}

// Close flushes the image, truncates unused clusters from the end of the
// file and closes it and its backing chain.
func (img *Image) Close() error {
	img.mu.Lock()
	defer img.mu.Unlock()
	if img.closed {
		return ErrClosed
	}
	img.closed = true
	var errs []error
	if !img.readOnly {
		err := img.storeBitmaps()
		if err == nil {
			err = img.flushAll()
		}
		if err == nil {
			err = img.truncateFile()
		}
		errs = append(errs, err)
	}
	errs = append(errs, img.f.Close())
	if img.backing != nil {
		errs = append(errs, img.backing.Close())
	}
	return errors.Join(errs...)
}

// ReadAt reads len(p) bytes of guest data starting at off.
func (img *Image) ReadAt(p []byte, off int64) (int, error) {
	img.mu.Lock()
	defer img.mu.Unlock()
	if img.closed {
		return 0, ErrClosed
	}
	if off < 0 {
		return 0, errors.New("qcow2: negative offset")
	}
	if off >= img.size {
		return 0, io.EOF
	}
	var eof error
	if int64(len(p)) > img.size-off {
		p = p[:img.size-off]
		eof = io.EOF
	}
	done := 0
	for done < len(p) {
		g := off + int64(done)
		in := g & (img.clusterSize - 1)
		n := int(min(img.clusterSize-in, int64(len(p)-done)))
		entry, err := img.l2Entry(g)
		if err != nil {
			return done, err
		}
		if err := img.readFromEntry(entry, g, p[done:done+n]); err != nil {
			return done, err
		}
		done += n
	}
	return done, eof
}

// WriteAt writes p to the guest disk at off. Writes beyond the virtual
// size fail with ErrOutOfRange; use Resize first.
func (img *Image) WriteAt(p []byte, off int64) (int, error) {
	img.mu.Lock()
	defer img.mu.Unlock()
	if img.closed {
		return 0, ErrClosed
	}
	if img.readOnly {
		return 0, ErrReadOnly
	}
	if off < 0 || off > img.size || int64(len(p)) > img.size-off {
		return 0, ErrOutOfRange
	}
	if err := img.beforeGuestChange(); err != nil {
		return 0, err
	}
	if err := img.markDirty(off, off+int64(len(p))); err != nil {
		return 0, err
	}
	done := 0
	for done < len(p) {
		g := off + int64(done)
		in := g & (img.clusterSize - 1)
		n := int(min(img.clusterSize-in, int64(len(p)-done)))
		if err := img.writeCluster(g, p[done:done+n]); err != nil {
			return done, err
		}
		if err := img.maybeFlush(); err != nil {
			return done, err
		}
		done += n
	}
	if img.opts.WriteThrough {
		if err := img.flush(); err != nil {
			return 0, err
		}
	}
	return done, nil
}

// readFromEntry fills buf with guest data at guest offset g, which lies in
// the cluster described by the L2 entry. buf must not cross the cluster.
func (img *Image) readFromEntry(entry uint64, g int64, buf []byte) error {
	in := g & (img.clusterSize - 1)
	if entry&flagCompressed != 0 {
		data, err := img.decompress(entry)
		if err != nil {
			return err
		}
		copy(buf, data[in:])
		return nil
	}
	if img.hdr.Version >= 3 && entry&flagZero != 0 {
		clear(buf)
		return nil
	}
	host := int64(entry & l2OffsetMask)
	if host == 0 {
		return img.readBacking(buf, g)
	}
	if host&(img.clusterSize-1) != 0 {
		return fmt.Errorf("%w: unaligned data cluster offset 0x%x", ErrCorrupt, host)
	}
	return img.readFull(buf, host+in)
}

func (img *Image) readBacking(buf []byte, g int64) error {
	if img.backing == nil {
		clear(buf)
		return nil
	}
	bsize := img.backing.Size()
	n := 0
	if g < bsize {
		n = int(min(int64(len(buf)), bsize-g))
		m, err := img.backing.ReadAt(buf[:n], g)
		if err != nil && !(err == io.EOF && m == n) {
			return err
		}
	}
	clear(buf[n:])
	return nil
}

// readFull reads from the image file, treating data past EOF as zeros.
func (img *Image) readFull(buf []byte, off int64) error {
	n, err := img.f.ReadAt(buf, off)
	if err == io.EOF {
		clear(buf[n:])
		return nil
	}
	return err
}

func (img *Image) readTable(dst *[]uint64, off int64, entries int) error {
	buf := make([]byte, entries*8)
	if err := img.readFull(buf, off); err != nil {
		return err
	}
	t := make([]uint64, entries)
	for i := range t {
		t[i] = binary.BigEndian.Uint64(buf[i*8:])
	}
	*dst = t
	return nil
}

func (img *Image) writeUint64(off int64, v uint64) error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	_, err := img.f.WriteAt(b[:], off)
	return err
}

func (img *Image) l1EntriesFor(size int64) int64 {
	perL1 := img.clusterSize << img.l2Bits
	return (size + perL1 - 1) / perL1
}

func alignUp(n, a int64) int64 {
	return (n + a - 1) / a * a
}
