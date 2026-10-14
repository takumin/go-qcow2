package qcow2

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// AmendOptions lists the format options Amend changes; zero values keep
// the current setting.
type AmendOptions struct {
	// Version converts the image to qcow2 version 2 or 3. Downgrading
	// needs an image without bitmaps and with zlib compression; zero
	// clusters are turned into clusters of zeros and the refcount width
	// becomes 16 bits.
	Version int
	// RefcountBits changes the refcount width (1, 2, 4, ..., 64; version
	// 2 images only support 16).
	RefcountBits int
	// LazyRefcounts sets or clears the lazy_refcounts compatible feature
	// (version 3 only).
	LazyRefcounts *bool
}

// Amend changes format options of the image in place, like
// "qemu-img amend". Every step is crash safe: new structures are written
// before the header switches to them.
func (img *Image) Amend(opts AmendOptions) error {
	img.mu.Lock()
	defer img.mu.Unlock()
	if err := img.writable(); err != nil {
		return err
	}
	version := int(img.hdr.Version)
	if opts.Version != 0 {
		if opts.Version != 2 && opts.Version != 3 {
			return fmt.Errorf("qcow2: invalid version %d", opts.Version)
		}
		version = opts.Version
	}
	if rb := opts.RefcountBits; rb != 0 {
		if rb&(rb-1) != 0 || rb > 64 || version == 2 && rb != 16 {
			return fmt.Errorf("qcow2: invalid refcount bits %d for version %d", rb, version)
		}
	}
	if opts.LazyRefcounts != nil && *opts.LazyRefcounts && version < 3 {
		return errors.New("qcow2: lazy refcounts need a version 3 image")
	}
	if version == 2 && img.hdr.Version >= 3 {
		if len(img.bitmaps) > 0 {
			return fmt.Errorf("%w: downgrading an image with bitmaps; remove them first", ErrUnsupported)
		}
		if img.hdr.CompressionType != compressionZlib {
			return fmt.Errorf("%w: downgrading an image with zstd compression", ErrUnsupported)
		}
	}
	if err := img.flushAll(); err != nil {
		return err
	}

	if version == 3 && img.hdr.Version < 3 {
		if err := img.upgrade(); err != nil {
			return err
		}
	}
	if rb := opts.RefcountBits; rb != 0 && uint(1)<<img.refOrder != uint(rb) {
		if err := img.rebuildRefcounts(uint(bits.TrailingZeros(uint(rb)))); err != nil {
			return err
		}
	}
	if version == 2 && img.hdr.Version >= 3 {
		if err := img.downgrade(); err != nil {
			return err
		}
	}
	if opts.LazyRefcounts != nil && img.hdr.Version >= 3 {
		h := img.headerCopy()
		if *opts.LazyRefcounts {
			h.CompatibleFeatures |= compatLazyRefcounts
		} else {
			h.CompatibleFeatures &^= compatLazyRefcounts
		}
		if h.CompatibleFeatures != img.hdr.CompatibleFeatures {
			return img.writeHeader(h)
		}
	}
	return nil
}

func (img *Image) headerCopy() *header {
	h := *img.hdr
	h.raw = bytes.Clone(img.hdr.raw)
	h.extensions = append([]headerExtension(nil), img.hdr.extensions...)
	return &h
}

// upgrade converts a version 2 image to version 3.
func (img *Image) upgrade() error {
	// Version 3 needs the virtual size in the extra data of snapshots.
	short := false
	for _, sn := range img.snapshots {
		short = short || len(sn.extra) < 16
	}
	if short {
		snaps := make([]*snapshot, len(img.snapshots))
		for i, sn := range img.snapshots {
			c := *sn
			if len(c.extra) < 16 {
				extra := make([]byte, snapshotExtraDataSize)
				binary.BigEndian.PutUint64(extra[0:], uint64(c.vmStateSize))
				copy(extra, c.extra)
				binary.BigEndian.PutUint64(extra[8:], uint64(img.size))
				binary.BigEndian.PutUint64(extra[16:], ^uint64(0))
				c.extra = extra
			}
			snaps[i] = &c
		}
		if err := img.writeSnapshots(snaps); err != nil {
			return err
		}
	}
	h := img.headerCopy()
	h.Version = 3
	h.HeaderLength = v3HeaderLength
	h.IncompatibleFeatures, h.CompatibleFeatures, h.AutoclearFeatures = 0, 0, 0
	h.RefcountOrder = 4
	h.CompressionType = compressionZlib
	return img.writeHeader(h)
}

// downgrade converts a version 3 image to version 2.
func (img *Image) downgrade() error {
	// Bitmaps removed in this session are still in the bitmap directory.
	if err := img.storeBitmaps(); err != nil {
		return err
	}
	if img.refOrder != 4 {
		if err := img.rebuildRefcounts(4); err != nil {
			return err
		}
	}
	if err := img.expandZeroClusters(); err != nil {
		return err
	}
	if err := img.flushAll(); err != nil {
		return err
	}
	h := img.headerCopy()
	h.Version = 2
	h.HeaderLength = v2HeaderLength
	h.IncompatibleFeatures, h.CompatibleFeatures, h.AutoclearFeatures = 0, 0, 0
	return img.writeHeader(h)
}

// expandZeroClusters replaces the zero clusters of the active image and of
// every snapshot, which version 2 cannot express, by clusters of zeros, or
// by unallocated clusters when there is no backing file.
func (img *Image) expandZeroClusters() error {
	seen := map[int64]bool{}
	expand := func(l1 []uint64) error {
		for _, e := range l1 {
			l2 := int64(e & l1OffsetMask)
			if l2 == 0 || seen[l2] {
				continue
			}
			seen[l2] = true
			t, err := img.loadL2(l2)
			if err != nil {
				return err
			}
			// A cluster mapped by a shared L2 table is referenced once per
			// L1 table reaching it.
			sharers, err := img.refcount(l2)
			if err != nil {
				return err
			}
			changed := false
			for j := 0; j < len(t); j += 8 {
				entry := binary.BigEndian.Uint64(t[j:])
				if entry&flagCompressed != 0 || entry&flagZero == 0 {
					continue
				}
				ne, err := img.expandZeroEntry(entry, sharers)
				if err != nil {
					return err
				}
				binary.BigEndian.PutUint64(t[j:], ne)
				changed = true
			}
			if changed {
				img.l2Cache.markDirty(l2, t)
			}
			if err := img.maybeFlush(); err != nil {
				return err
			}
		}
		return nil
	}
	if err := expand(img.l1); err != nil {
		return err
	}
	for _, sn := range img.snapshots {
		var l1 []uint64
		if err := img.readTable(&l1, sn.l1Offset, int(sn.l1Size)); err != nil {
			return err
		}
		if err := expand(l1); err != nil {
			return err
		}
	}
	return nil
}

// expandZeroEntry returns an L2 entry without the zero flag that reads as
// zeros, writing or allocating a cluster of zeros as needed.
func (img *Image) expandZeroEntry(entry uint64, sharers uint64) (uint64, error) {
	zeros := make([]byte, img.clusterSize)
	if host := int64(entry & l2OffsetMask); host != 0 {
		// Preallocated zero clusters are only ever referenced as such, so
		// their contents can be replaced in place.
		img.dataDirty = true
		if _, err := img.f.WriteAt(zeros, host); err != nil {
			return 0, err
		}
		return entry &^ flagZero, nil
	}
	if img.backing == nil {
		return 0, nil
	}
	c, err := img.allocClusters(1)
	if err != nil {
		return 0, err
	}
	if sharers > 1 {
		if err := img.addRefcount(c, int64(sharers-1)); err != nil {
			return 0, err
		}
	}
	img.dataDirty = true
	if _, err := img.f.WriteAt(zeros, c); err != nil {
		return 0, err
	}
	if sharers == 1 {
		return uint64(c) | flagCopied, nil
	}
	return uint64(c), nil
}

// rebuildRefcounts writes new refcount structures with 2^order-bit
// entries after the end of the file, holding the references counted from
// the metadata, and switches the header to them. The old structures are
// freed. Leaks disappear on the way; corruptions make it fail.
func (img *Image) rebuildRefcounts(order uint) error {
	if err := img.flushAll(); err != nil {
		return err
	}
	res, rc, err := img.check()
	if err != nil {
		return err
	}
	if res.Corruptions > 0 {
		return fmt.Errorf("%w: %d corruptions found, repair the image first (first: %s)", ErrCorrupt, res.Corruptions, res.Errors[0])
	}
	cs := img.clusterSize
	refs := rc.refs
	// The old refcount structures are not referenced by the new ones.
	var old []int64
	for c := int64(0); c < alignUp(int64(len(img.refTable))*8, cs); c += cs {
		old = append(old, img.refTableOffset+c)
	}
	for _, e := range img.refTable {
		if off := int64(e & refTableMask); off != 0 {
			old = append(old, off)
		}
	}
	for _, c := range old {
		refs[c>>img.clusterBits]--
	}
	maxRef := uint64(math.MaxUint64)
	if order < 6 {
		maxRef = uint64(1)<<(uint64(1)<<order) - 1
	}
	for c, n := range refs {
		if n > maxRef {
			return fmt.Errorf("qcow2: cluster 0x%x has %d references, more than %d-bit refcounts hold", int64(c)<<img.clusterBits, n, 1<<order)
		}
	}

	// Lay out the table and then the blocks after the file; they must
	// cover themselves.
	rpb := cs * 8 >> order
	start := int64(len(refs))
	var tableClusters, blocks int64
	for {
		total := start + tableClusters + blocks
		nb := (total + rpb - 1) / rpb
		nt := max(1, (nb*8+cs-1)/cs)
		if nb == blocks && nt == tableClusters {
			break
		}
		blocks, tableClusters = nb, nt
	}
	if tableClusters*cs > 64<<20 {
		return errors.New("qcow2: refcount table too large")
	}
	tableOff := start * cs
	blockOff := tableOff + tableClusters*cs
	end := blockOff + blocks*cs

	table := make([]byte, tableClusters*cs)
	buf := make([]byte, cs)
	for k := range blocks {
		clear(buf)
		for i := range rpb {
			c := k*rpb + i
			var v uint64
			switch {
			case c < int64(len(refs)):
				v = refs[c]
			case c < end/cs:
				v = 1 // the new structures
			}
			if v != 0 {
				refSetOrder(buf, order, i, v)
			}
		}
		if _, err := img.f.WriteAt(buf, blockOff+k*cs); err != nil {
			return err
		}
		binary.BigEndian.PutUint64(table[k*8:], uint64(blockOff+k*cs))
	}
	if _, err := img.f.WriteAt(table, tableOff); err != nil {
		return err
	}
	if err := img.f.Sync(); err != nil {
		return err
	}
	h := img.headerCopy()
	h.RefcountOrder = uint32(order)
	h.RefcountTableOffset = uint64(tableOff)
	h.RefcountTableClusters = uint32(tableClusters)
	if err := img.writeHeader(h); err != nil {
		return err
	}

	img.refOrder = order
	img.refsPerBlock = rpb
	img.refTableOffset = tableOff
	if err := img.readTable(&img.refTable, tableOff, int(tableClusters*cs/8)); err != nil {
		return err
	}
	img.refCache = newTableCache(img.refCache.cap)
	img.fileEnd = end
	img.freeHint = 0
	clear(img.reserved)
	for _, c := range old {
		if err := img.freed(c); err != nil {
			return err
		}
	}
	return nil
}
