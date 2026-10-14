package qcow2

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// allocClusters finds n contiguous free clusters and sets their refcount
// to 1. The clusters are not written; callers must initialise them fully
// since a reused cluster still holds stale data.
func (img *Image) allocClusters(n int64) (int64, error) {
	off, err := img.reserve(n)
	if err != nil {
		return 0, err
	}
	for i := int64(0); i < n; i++ {
		if err := img.addRefcount(off+i*img.clusterSize, 1); err != nil {
			return 0, err
		}
	}
	return off, nil
}

// reserve returns the first run of n contiguous clusters at or after
// freeHint whose refcount is zero, extending the file if necessary. The
// clusters stay reserved until their refcount is raised, so that nested
// allocations (refcount blocks, refcount table) cannot hand them out again.
func (img *Image) reserve(n int64) (int64, error) {
	start := img.freeHint
	firstFree := int64(-1)
	for i := int64(0); i < n; {
		c := start + i*img.clusterSize
		free, err := img.isFree(c)
		if err != nil {
			return 0, err
		}
		if !free {
			start, i = c+img.clusterSize, 0
			continue
		}
		if firstFree < 0 {
			firstFree = c
		}
		i++
	}
	end := start + n*img.clusterSize
	for c := start; c < end; c += img.clusterSize {
		img.reserved[c] = struct{}{}
	}
	if firstFree < start {
		// A hole too small for this run was skipped; keep it findable.
		img.freeHint = firstFree
	} else {
		img.freeHint = end
	}
	img.fileEnd = max(img.fileEnd, end)
	return start, nil
}

func (img *Image) isFree(c int64) (bool, error) {
	if _, ok := img.reserved[c]; ok {
		return false, nil
	}
	if i := c >> img.clusterBits; i < int64(len(img.inUse)) && img.inUse[i] > 0 {
		return false, nil
	}
	rc, err := img.refcount(c)
	return rc == 0, err
}

func (img *Image) refIndex(host int64) (tableIdx, blockIdx int64) {
	c := host >> img.clusterBits
	return c / img.refsPerBlock, c % img.refsPerBlock
}

func (img *Image) loadRefBlock(off int64) ([]byte, error) {
	if off&(img.clusterSize-1) != 0 {
		return nil, fmt.Errorf("%w: unaligned refcount block offset 0x%x", ErrCorrupt, off)
	}
	if b := img.refCache.get(off); b != nil {
		return b, nil
	}
	b := make([]byte, img.clusterSize)
	if err := img.readFull(b, off); err != nil {
		return nil, err
	}
	img.refCache.put(off, b, false)
	return b, nil
}

// refcount returns the refcount of the cluster at host offset.
func (img *Image) refcount(host int64) (uint64, error) {
	ti, bi := img.refIndex(host)
	if ti >= int64(len(img.refTable)) {
		return 0, nil
	}
	off := int64(img.refTable[ti] & refTableMask)
	if off == 0 {
		return 0, nil
	}
	b, err := img.loadRefBlock(off)
	if err != nil {
		return 0, err
	}
	v, _, _ := img.refGet(b, bi)
	return v, nil
}

// refGet decodes refcount i of a block and returns the byte range holding it.
func (img *Image) refGet(b []byte, i int64) (v uint64, start, end int64) {
	switch img.refOrder {
	case 0, 1, 2:
		bits := uint64(1) << img.refOrder
		pos := uint64(i) * bits
		start = int64(pos / 8)
		return uint64(b[start]>>(pos%8)) & (1<<bits - 1), start, start + 1
	case 3:
		return uint64(b[i]), i, i + 1
	case 4:
		return uint64(binary.BigEndian.Uint16(b[i*2:])), i * 2, i*2 + 2
	case 5:
		return uint64(binary.BigEndian.Uint32(b[i*4:])), i * 4, i*4 + 4
	default:
		return binary.BigEndian.Uint64(b[i*8:]), i * 8, i*8 + 8
	}
}

func (img *Image) refSet(b []byte, i int64, v uint64) {
	switch img.refOrder {
	case 0, 1, 2:
		bits := uint64(1) << img.refOrder
		pos := uint64(i) * bits
		mask := byte(1<<bits-1) << (pos % 8)
		b[pos/8] = b[pos/8]&^mask | byte(v<<(pos%8))&mask
	case 3:
		b[i] = byte(v)
	case 4:
		binary.BigEndian.PutUint16(b[i*2:], uint16(v))
	case 5:
		binary.BigEndian.PutUint32(b[i*4:], uint32(v))
	default:
		binary.BigEndian.PutUint64(b[i*8:], v)
	}
}

func (img *Image) maxRefcount() uint64 {
	if img.refOrder == 6 {
		return math.MaxUint64
	}
	return uint64(1)<<(uint64(1)<<img.refOrder) - 1
}

// addRefcount adds delta to the refcount of the cluster at host offset,
// allocating refcount blocks and growing the refcount table as needed.
func (img *Image) addRefcount(host int64, delta int64) error {
	ti, bi := img.refIndex(host)
	if ti >= int64(len(img.refTable)) {
		if delta < 0 {
			return fmt.Errorf("%w: refcount underflow at 0x%x", ErrCorrupt, host)
		}
		if err := img.growRefTable(ti + 1); err != nil {
			return err
		}
	}
	blkOff := int64(img.refTable[ti] & refTableMask)
	if blkOff == 0 {
		if delta < 0 {
			return fmt.Errorf("%w: refcount underflow at 0x%x", ErrCorrupt, host)
		}
		// New refcount block. Its own refcount is set afterwards, possibly
		// inside itself; until then it is protected by img.reserved.
		var err error
		if blkOff, err = img.reserve(1); err != nil {
			return err
		}
		img.refCache.put(blkOff, make([]byte, img.clusterSize), true)
		img.refTable[ti] = uint64(blkOff)
		img.refTableDirty[ti] = struct{}{}
		if err := img.addRefcount(blkOff, 1); err != nil {
			return err
		}
	}
	b, err := img.loadRefBlock(blkOff)
	if err != nil {
		return err
	}
	v, _, _ := img.refGet(b, bi)
	switch {
	case delta >= 0 && img.maxRefcount()-v < uint64(delta):
		return fmt.Errorf("qcow2: refcount overflow at 0x%x", host)
	case delta < 0 && v < uint64(-delta):
		return fmt.Errorf("%w: refcount underflow at 0x%x", ErrCorrupt, host)
	}
	nv := uint64(int64(v) + delta)
	img.refSet(b, bi, nv)
	img.refCache.markDirty(blkOff, b)
	delete(img.reserved, host)
	if nv == 0 {
		return img.freed(host)
	}
	return nil
}

// deferFree drops one reference to a host cluster once the change that
// removed the reference from the metadata has reached the disk.
func (img *Image) deferFree(host int64) {
	img.pendingFrees = append(img.pendingFrees, host)
}

// freed makes a cluster whose refcount dropped to zero available for
// reuse, forgets anything cached about its old contents and returns its
// space to the file system.
func (img *Image) freed(host int64) error {
	img.freeHint = min(img.freeHint, host)
	img.l2Cache.remove(host)
	img.refCache.remove(host)
	img.decompBuf = nil
	if img.compPos&^(img.clusterSize-1) == host {
		img.compPos = 0
	}
	if img.noPunchHole {
		return nil
	}
	err := img.f.punchHole(host, img.clusterSize)
	if errors.Is(err, errPunchUnsupported) {
		img.noPunchHole = true
		return nil
	}
	return err
}

// growRefTable moves the refcount table to a larger location holding at
// least minEntries entries.
func (img *Image) growRefTable(minEntries int64) error {
	perCluster := img.clusterSize / 8
	entries := max(minEntries, int64(len(img.refTable))*2)
	var clusters int64
	for {
		clusters = (entries + perCluster - 1) / perCluster
		// The new table and the refcount blocks describing it must be
		// covered by the table itself; leave generous slack.
		end := img.fileEnd + (clusters+64)*img.clusterSize
		need, _ := img.refIndex(end)
		if need+1 <= clusters*perCluster {
			break
		}
		entries = need + 1
	}
	entries = clusters * perCluster

	oldOff := img.refTableOffset
	oldClusters := int64(len(img.refTable)) / perCluster

	newOff, err := img.reserve(clusters)
	if err != nil {
		return err
	}
	// Everything below happens in memory; flush writes the new table
	// before switching the header to it, and frees the old one last.
	t := make([]uint64, entries)
	copy(t, img.refTable)
	img.hdr.RefcountTableOffset = uint64(newOff)
	img.hdr.RefcountTableClusters = uint32(clusters)
	img.refTable = t
	img.refTableOffset = newOff
	img.refTableMoved = true
	img.hdrRefTableDirty = true

	for i := int64(0); i < clusters; i++ {
		if err := img.addRefcount(newOff+i*img.clusterSize, 1); err != nil {
			return err
		}
	}
	if oldOff != 0 {
		for i := int64(0); i < oldClusters; i++ {
			img.deferFree(oldOff + i*img.clusterSize)
		}
	}
	return nil
}
