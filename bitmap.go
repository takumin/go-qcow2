package qcow2

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
)

// Persistent dirty bitmaps (the qcow2 "bitmaps" extension).
//
// Bitmaps are loaded lazily and kept in memory. Enabled bitmaps record
// every range changed by WriteAt, Discard and Resize. Changes are stored
// when the image is closed, by writing new bitmap tables and a new bitmap
// directory and then switching the header to them, so a crash never
// leaves a malformed bitmap.
//
// Like QEMU, before the first change to the guest disk the enabled bitmaps
// are flagged "in use" on disk: if the image is not closed properly, they
// stay flagged and must be considered inconsistent (remove them). Opening
// an image writable without changing it leaves its bitmaps valid.

var (
	// ErrBitmapNotFound is returned for operations on an unknown bitmap.
	ErrBitmapNotFound = errors.New("qcow2: bitmap not found")
	// ErrBitmapInUse is returned when reading a bitmap that was not saved
	// properly; such a bitmap can only be removed.
	ErrBitmapInUse = errors.New("qcow2: bitmap is in use (inconsistent)")
)

const (
	bmeInUse               = 1 << 0
	bmeAuto                = 1 << 1
	bmeExtraDataCompatible = 1 << 2
	bmeKnownFlags          = bmeInUse | bmeAuto | bmeExtraDataCompatible

	bitmapTypeDirty    = 1
	minBitmapGranBits  = 9
	maxBitmapGranBits  = 31
	maxBitmapNameSize  = 1023
	maxBitmaps         = 65535
	maxBitmapDirSize   = 64 << 20
	maxBitmapTableSize = 0x8000000

	autoclearBitmaps = 1 << 0

	bitmapTableOffsetMask = 0x00fffffffffffe00
)

// BitmapInfo describes a persistent dirty bitmap.
type BitmapInfo struct {
	Name string
	// Granularity is the number of guest bytes covered by one bit.
	Granularity int64
	// Enabled bitmaps record writes (the "auto" flag).
	Enabled bool
	// InUse is set for bitmaps that were not saved properly, typically
	// because the image was not closed. Their contents are unusable.
	InUse bool
	// Unsupported is set for bitmaps using features this package does not
	// know. They are preserved but can only be removed.
	Unsupported bool
}

// Extent is a range of the guest disk.
type Extent struct {
	Offset, Length int64
}

type bitmap struct {
	name     string
	granBits uint
	flags    uint32
	typ      uint8
	extra    []byte

	// Location on disk; tableOffset is 0 for a bitmap not stored yet.
	tableOffset int64
	tableSize   uint32
	dirPos      int64 // position of the entry in the directory on disk
	inUseOnDisk bool  // we set the in-use flag on disk this session

	// managed bitmaps are maintained by this package; others are kept
	// verbatim.
	managed bool
	bits    []byte // loaded contents of a managed bitmap
	changed bool   // bits differ from the stored tables
}

func (b *bitmap) enabled() bool { return b.flags&bmeAuto != 0 }

func (b *bitmap) info() BitmapInfo {
	return BitmapInfo{
		Name:        b.name,
		Granularity: int64(1) << b.granBits,
		Enabled:     b.enabled(),
		InUse:       b.flags&bmeInUse != 0,
		Unsupported: !b.managed && b.flags&bmeInUse == 0,
	}
}

// bitmapBytes returns the size of the bit array of a bitmap.
func (img *Image) bitmapBytes(granBits uint) int64 {
	nbits := (img.size + int64(1)<<granBits - 1) >> granBits
	return (nbits + 7) / 8
}

func (img *Image) bitmapTableSize(granBits uint) int64 {
	return (img.bitmapBytes(granBits) + img.clusterSize - 1) / img.clusterSize
}

// loadBitmapDirectory reads the bitmap directory referenced by the header.
func (img *Image) loadBitmapDirectory() error {
	h := img.hdr
	var ext []byte
	for _, e := range h.extensions {
		if e.Type == extBitmaps {
			ext = e.Data
		}
	}
	if ext == nil || h.AutoclearFeatures&autoclearBitmaps == 0 {
		// Without the autoclear bit the extension is stale; drop it from
		// the header the next time it is written.
		img.dropBitmapExtension()
		return nil
	}
	be := binary.BigEndian
	if len(ext) < 24 {
		return fmt.Errorf("%w: bitmaps extension too short", ErrCorrupt)
	}
	n := be.Uint32(ext[0:])
	dirSize := int64(be.Uint64(ext[8:]))
	dirOff := int64(be.Uint64(ext[16:]))
	if n == 0 || n > maxBitmaps || dirSize > maxBitmapDirSize || dirOff&(img.clusterSize-1) != 0 {
		return fmt.Errorf("%w: invalid bitmaps extension", ErrCorrupt)
	}
	dir := make([]byte, dirSize)
	if err := img.readFull(dir, dirOff); err != nil {
		return err
	}
	var p int64
	for range n {
		if p+24 > dirSize {
			return fmt.Errorf("%w: bitmap directory truncated", ErrCorrupt)
		}
		e := dir[p:]
		b := &bitmap{
			tableOffset: int64(be.Uint64(e[0:])),
			tableSize:   be.Uint32(e[8:]),
			flags:       be.Uint32(e[12:]),
			typ:         e[16],
			granBits:    uint(e[17]),
			dirPos:      p,
		}
		nameSize := int64(be.Uint16(e[18:]))
		extraSize := int64(be.Uint32(e[20:]))
		size := alignUp(24+extraSize+nameSize, 8)
		if nameSize == 0 || nameSize > maxBitmapNameSize || p+size > dirSize {
			return fmt.Errorf("%w: invalid bitmap directory entry", ErrCorrupt)
		}
		b.extra = bytes.Clone(e[24 : 24+extraSize])
		b.name = string(e[24+extraSize : 24+extraSize+nameSize])
		if b.tableOffset&(img.clusterSize-1) != 0 || b.tableSize > maxBitmapTableSize {
			return fmt.Errorf("%w: invalid table of bitmap %q", ErrCorrupt, b.name)
		}
		b.managed = b.typ == bitmapTypeDirty &&
			b.flags&^bmeKnownFlags == 0 && b.flags&bmeInUse == 0 &&
			b.granBits >= minBitmapGranBits && b.granBits <= maxBitmapGranBits &&
			(extraSize == 0 || b.flags&bmeExtraDataCompatible != 0) &&
			int64(b.tableSize) == img.bitmapTableSize(b.granBits)
		img.bitmaps = append(img.bitmaps, b)
		p += size
	}
	img.bitmapDirOffset, img.bitmapDirSize = dirOff, dirSize
	return nil
}

func (img *Image) dropBitmapExtension() {
	h := img.hdr
	exts := h.extensions[:0]
	for _, e := range h.extensions {
		if e.Type != extBitmaps {
			exts = append(exts, e)
		}
	}
	h.extensions = exts
	h.AutoclearFeatures &^= autoclearBitmaps
}

// loadBits reads the contents of a managed bitmap.
func (img *Image) loadBits(b *bitmap) error {
	if b.bits != nil {
		return nil
	}
	n := img.bitmapBytes(b.granBits)
	buf := make([]byte, int64(b.tableSize)*img.clusterSize)
	if b.tableOffset != 0 {
		var t []uint64
		if err := img.readTable(&t, b.tableOffset, int(b.tableSize)); err != nil {
			return err
		}
		for i, e := range t {
			chunk := buf[int64(i)*img.clusterSize : int64(i+1)*img.clusterSize]
			off := int64(e & bitmapTableOffsetMask)
			switch {
			case e&^bitmapTableOffsetMask&^1 != 0 || off != 0 && e&1 != 0:
				return fmt.Errorf("%w: invalid table entry of bitmap %q", ErrCorrupt, b.name)
			case off != 0:
				if err := img.readFull(chunk, off); err != nil {
					return err
				}
			case e&1 != 0:
				for j := range chunk {
					chunk[j] = 0xff
				}
			}
		}
	}
	b.bits = buf[:n]
	return nil
}

func (img *Image) findBitmap(name string) (*bitmap, error) {
	for _, b := range img.bitmaps {
		if b.name == name {
			return b, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrBitmapNotFound, name)
}

// Bitmaps lists the persistent dirty bitmaps of the image.
func (img *Image) Bitmaps() []BitmapInfo {
	img.mu.Lock()
	defer img.mu.Unlock()
	infos := make([]BitmapInfo, len(img.bitmaps))
	for i, b := range img.bitmaps {
		infos[i] = b.info()
	}
	return infos
}

// AddBitmap creates an empty bitmap. A granularity of 0 selects the
// cluster size, but at least 4 KiB, like QEMU. Enabled bitmaps record
// subsequent writes. Requires a version 3 image.
func (img *Image) AddBitmap(name string, granularity int64, enabled bool) error {
	img.mu.Lock()
	defer img.mu.Unlock()
	if err := img.writable(); err != nil {
		return err
	}
	if img.hdr.Version < 3 {
		return fmt.Errorf("%w: bitmaps need a version 3 image", ErrUnsupported)
	}
	if name == "" || len(name) > maxBitmapNameSize {
		return fmt.Errorf("qcow2: invalid bitmap name %q", name)
	}
	if _, err := img.findBitmap(name); err == nil {
		return fmt.Errorf("qcow2: bitmap %q already exists", name)
	}
	if len(img.bitmaps) >= maxBitmaps {
		return errors.New("qcow2: too many bitmaps")
	}
	if granularity == 0 {
		granularity = max(img.clusterSize, 4096)
	}
	g := bits.TrailingZeros64(uint64(granularity))
	if granularity&(granularity-1) != 0 || g < minBitmapGranBits || g > maxBitmapGranBits {
		return fmt.Errorf("qcow2: invalid bitmap granularity %d", granularity)
	}
	b := &bitmap{name: name, granBits: uint(g), typ: bitmapTypeDirty, managed: true, changed: true}
	if enabled {
		b.flags = bmeAuto
	}
	b.bits = make([]byte, img.bitmapBytes(b.granBits))
	img.bitmaps = append(img.bitmaps, b)
	img.bitmapsChanged = true
	return nil
}

// RemoveBitmap deletes a bitmap. This is the only operation allowed on
// bitmaps that are in use or unsupported.
func (img *Image) RemoveBitmap(name string) error {
	img.mu.Lock()
	defer img.mu.Unlock()
	if err := img.writable(); err != nil {
		return err
	}
	b, err := img.findBitmap(name)
	if err != nil {
		return err
	}
	for i, x := range img.bitmaps {
		if x == b {
			img.bitmaps = append(img.bitmaps[:i], img.bitmaps[i+1:]...)
		}
	}
	if b.tableOffset != 0 {
		img.removedBitmaps = append(img.removedBitmaps, b)
	}
	img.bitmapsChanged = true
	return nil
}

// ClearBitmap marks the whole disk clean in a bitmap.
func (img *Image) ClearBitmap(name string) error {
	img.mu.Lock()
	defer img.mu.Unlock()
	b, err := img.managedBitmap(name, true)
	if err != nil {
		return err
	}
	clear(b.bits)
	b.changed = true
	img.bitmapsChanged = true
	return nil
}

// SetBitmapEnabled enables or disables recording of writes in a bitmap.
func (img *Image) SetBitmapEnabled(name string, enabled bool) error {
	img.mu.Lock()
	defer img.mu.Unlock()
	b, err := img.managedBitmap(name, true)
	if err != nil {
		return err
	}
	if enabled {
		b.flags |= bmeAuto
	} else {
		b.flags &^= bmeAuto
	}
	img.bitmapsChanged = true
	return nil
}

// DirtyExtents returns the ranges of the guest disk marked dirty in a
// bitmap, merged and clipped to the disk size.
func (img *Image) DirtyExtents(name string) ([]Extent, error) {
	img.mu.Lock()
	defer img.mu.Unlock()
	b, err := img.managedBitmap(name, false)
	if err != nil {
		return nil, err
	}
	var exts []Extent
	gran := int64(1) << b.granBits
	nbits := (img.size + gran - 1) / gran
	for i := int64(0); i < nbits; {
		if b.bits[i/8] == 0 && i%8 == 0 {
			i += 8
			continue
		}
		if b.bits[i/8]&(1<<(i%8)) == 0 {
			i++
			continue
		}
		start := i
		for i < nbits && b.bits[i/8]&(1<<(i%8)) != 0 {
			i++
		}
		end := min(i*gran, img.size)
		exts = append(exts, Extent{Offset: start * gran, Length: end - start*gran})
	}
	return exts, nil
}

func (img *Image) writable() error {
	if img.closed {
		return ErrClosed
	}
	if img.readOnly {
		return ErrReadOnly
	}
	return nil
}

// managedBitmap returns a loaded bitmap that this package can use.
func (img *Image) managedBitmap(name string, modify bool) (*bitmap, error) {
	if img.closed {
		return nil, ErrClosed
	}
	if modify && img.readOnly {
		return nil, ErrReadOnly
	}
	b, err := img.findBitmap(name)
	if err != nil {
		return nil, err
	}
	if !b.managed {
		if b.flags&bmeInUse != 0 {
			return nil, fmt.Errorf("%w: %q", ErrBitmapInUse, name)
		}
		return nil, fmt.Errorf("%w: bitmap %q", ErrUnsupported, name)
	}
	return b, img.loadBits(b)
}

// beforeGuestChange flags the enabled bitmaps stored on disk as in use and
// syncs, before the guest disk is changed for the first time.
func (img *Image) beforeGuestChange() error {
	defer img.metadataUpdate()()
	marked := false
	for _, b := range img.bitmaps {
		if !b.managed || !b.enabled() || b.tableOffset == 0 || b.inUseOnDisk {
			continue
		}
		var v [4]byte
		binary.BigEndian.PutUint32(v[:], b.flags|bmeInUse)
		if _, err := img.f.WriteAt(v[:], img.bitmapDirOffset+b.dirPos+12); err != nil {
			return err
		}
		b.inUseOnDisk = true
		marked = true
	}
	if !marked {
		return nil
	}
	img.bitmapsChanged = true
	return img.f.Sync()
}

// markDirty records a change of the guest range [off, end) in the
// enabled bitmaps.
func (img *Image) markDirty(off, end int64) error {
	if end <= off {
		return nil
	}
	for _, b := range img.bitmaps {
		if !b.managed || !b.enabled() {
			continue
		}
		if err := img.loadBits(b); err != nil {
			return err
		}
		for i := off >> b.granBits; i <= (end-1)>>b.granBits; i++ {
			b.bits[i/8] |= 1 << (i % 8)
		}
		b.changed = true
		img.bitmapsChanged = true
	}
	return nil
}

// resizeBitmaps adapts the bitmaps to a new virtual size (img.size).
func (img *Image) resizeBitmaps() error {
	for _, b := range img.bitmaps {
		if !b.managed {
			continue
		}
		if err := img.loadBits(b); err != nil {
			return err
		}
		n := img.bitmapBytes(b.granBits)
		b.bits = append(b.bits, make([]byte, n-int64(len(b.bits)))...)
		b.changed = true
		img.bitmapsChanged = true
	}
	return nil
}

// storeBitmaps writes changed bitmaps and a new directory, switches the
// header to them and frees the old structures.
func (img *Image) storeBitmaps() error {
	if !img.bitmapsChanged {
		return nil
	}
	defer img.metadataUpdate()()
	be := binary.BigEndian
	var free []int64
	collect := func(b *bitmap) error {
		var t []uint64
		if err := img.readTable(&t, b.tableOffset, int(b.tableSize)); err != nil {
			return err
		}
		for _, e := range t {
			if off := int64(e & bitmapTableOffsetMask); off != 0 {
				free = append(free, off)
			}
		}
		for c := int64(0); c < int64(b.tableSize)*8; c += img.clusterSize {
			free = append(free, b.tableOffset+c)
		}
		return nil
	}
	for _, b := range img.removedBitmaps {
		if err := collect(b); err != nil {
			return err
		}
	}

	type placed struct {
		b           *bitmap
		tableOffset int64
		tableSize   uint32
	}
	var entries []placed
	for _, b := range img.bitmaps {
		p := placed{b, b.tableOffset, b.tableSize}
		if b.managed && b.changed {
			if b.tableOffset != 0 {
				if err := collect(b); err != nil {
					return err
				}
			}
			off, n, err := img.writeBitmapTable(b)
			if err != nil {
				return err
			}
			p.tableOffset, p.tableSize = off, n
		}
		entries = append(entries, p)
	}

	var dir []byte
	positions := make([]int64, len(entries))
	for i, p := range entries {
		b := p.b
		flags := b.flags
		if b.managed {
			flags &^= bmeInUse
		}
		e := make([]byte, alignUp(int64(24+len(b.extra)+len(b.name)), 8))
		be.PutUint64(e[0:], uint64(p.tableOffset))
		be.PutUint32(e[8:], p.tableSize)
		be.PutUint32(e[12:], flags)
		e[16] = b.typ
		e[17] = byte(b.granBits)
		be.PutUint16(e[18:], uint16(len(b.name)))
		be.PutUint32(e[20:], uint32(len(b.extra)))
		copy(e[24:], b.extra)
		copy(e[24+len(b.extra):], b.name)
		positions[i] = int64(len(dir))
		dir = append(dir, e...)
	}
	if int64(len(dir)) > maxBitmapDirSize {
		return errors.New("qcow2: bitmap directory too large")
	}
	var dirOff int64
	if len(dir) > 0 {
		var err error
		dirOff, err = img.allocClusters(alignUp(int64(len(dir)), img.clusterSize) / img.clusterSize)
		if err != nil {
			return err
		}
		buf := make([]byte, alignUp(int64(len(dir)), img.clusterSize))
		copy(buf, dir)
		img.dataDirty = true
		if _, err := img.f.WriteAt(buf, dirOff); err != nil {
			return err
		}
	}
	if img.bitmapDirOffset != 0 {
		for c := int64(0); c < img.bitmapDirSize; c += img.clusterSize {
			free = append(free, img.bitmapDirOffset+c)
		}
	}

	// Everything the new directory refers to must be on disk before the
	// header points to it.
	if err := img.flushAll(); err != nil {
		return err
	}
	h := *img.hdr
	h.raw = bytes.Clone(img.hdr.raw)
	h.extensions = nil
	for _, e := range img.hdr.extensions {
		if e.Type != extBitmaps {
			h.extensions = append(h.extensions, e)
		}
	}
	if len(entries) > 0 {
		ext := make([]byte, 24)
		be.PutUint32(ext[0:], uint32(len(entries)))
		be.PutUint64(ext[8:], uint64(len(dir)))
		be.PutUint64(ext[16:], uint64(dirOff))
		h.extensions = append(h.extensions, headerExtension{Type: extBitmaps, Data: ext})
		h.AutoclearFeatures |= autoclearBitmaps
	} else {
		h.AutoclearFeatures &^= autoclearBitmaps
	}
	if err := img.writeHeader(&h); err != nil {
		return err
	}

	for i, p := range entries {
		b := p.b
		b.tableOffset, b.tableSize, b.dirPos = p.tableOffset, p.tableSize, positions[i]
		b.changed, b.inUseOnDisk = false, false
		if b.managed {
			b.flags &^= bmeInUse
		}
	}
	img.removedBitmaps = nil
	img.bitmapDirOffset, img.bitmapDirSize = dirOff, int64(len(dir))
	img.bitmapsChanged = false
	for _, off := range free {
		img.deferFree(off)
	}
	return img.flushAll()
}

// writeBitmapTable writes the contents of a bitmap to new clusters and
// returns its table. All-zero and all-one clusters take no space.
func (img *Image) writeBitmapTable(b *bitmap) (int64, uint32, error) {
	n := img.bitmapTableSize(b.granBits)
	t := make([]byte, alignUp(n*8, img.clusterSize))
	ones := bytes.Repeat([]byte{0xff}, int(img.clusterSize))
	for i := int64(0); i < n; i++ {
		chunk := make([]byte, img.clusterSize)
		copy(chunk, b.bits[min(i*img.clusterSize, int64(len(b.bits))):])
		var e uint64
		switch {
		case allZero(chunk):
		case bytes.Equal(chunk, ones):
			e = 1
		default:
			off, err := img.allocClusters(1)
			if err != nil {
				return 0, 0, err
			}
			img.dataDirty = true
			if _, err := img.f.WriteAt(chunk, off); err != nil {
				return 0, 0, err
			}
			e = uint64(off)
		}
		binary.BigEndian.PutUint64(t[i*8:], e)
	}
	off, err := img.allocClusters(int64(len(t)) / img.clusterSize)
	if err != nil {
		return 0, 0, err
	}
	img.dataDirty = true
	if _, err := img.f.WriteAt(t, off); err != nil {
		return 0, 0, err
	}
	return off, uint32(n), nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// writeHeader replaces the header in cluster 0 and syncs.
func (img *Image) writeHeader(h *header) error {
	buf, err := h.marshal(int(img.clusterSize))
	if err != nil {
		return err
	}
	if _, err := img.f.WriteAt(buf, 0); err != nil {
		return err
	}
	if err := img.f.Sync(); err != nil {
		return err
	}
	*img.hdr = *h
	return nil
}
