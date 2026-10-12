package qcow2

import (
	"encoding/binary"
)

// Metadata changes are kept in memory (dirty L2 tables and refcount blocks
// in the caches, dirty L1 and refcount table entries, header fields) and
// written by flush in dependency order:
//
//  1. guest data and refcount blocks
//  2. refcount table, then the header fields locating it
//  3. L2 tables
//  4. L1 table
//  5. header fields for the virtual size and the L1 table
//
// Before a step writes pointers, the writes of the previous steps are
// synced, so every cluster has its refcount (and contents) on disk before
// anything on disk points to it; one final sync makes the flush durable.
// Steps without changes are skipped, so a flush of in-place overwrites
// costs one sync and one that allocated clusters two.
//
// Refcount decrements of clusters whose last on-disk pointer was removed
// are applied only after that removal has been synced: only then may the
// clusters be reused or punched. The decrements themselves are written by
// the next flush (step 1); losing them in a crash merely leaks clusters.
//
// A crash at any point therefore leaves a consistent image holding the
// metadata of the last completed step; the worst outcome is leaked
// clusters, which RepairLeaks reclaims.

// needsFlush reports whether anything is waiting to be written or synced.
func (img *Image) needsFlush() bool {
	return img.dataDirty ||
		img.refCache.dirty > 0 || img.l2Cache.dirty > 0 ||
		img.refTableMoved || len(img.refTableDirty) > 0 || img.hdrRefTableDirty ||
		img.l1Moved || len(img.l1Dirty) > 0 || img.hdrDirty ||
		len(img.pendingFrees) > 0
}

// flush makes everything written so far durable. Refcount decrements it
// applies stay in memory until the next flush; see flushAll.
func (img *Image) flush() error {
	defer img.metadataUpdate()()
	if !img.needsFlush() {
		return nil
	}

	// unsynced is set while writes are not known to be on disk; barrier
	// syncs them before anything depending on them is written.
	unsynced := img.dataDirty
	barrier := func() error {
		if !unsynced {
			return nil
		}
		unsynced = false
		return img.f.Sync()
	}

	// 1. Guest data and refcount blocks.
	if img.refCache.dirty > 0 {
		if err := img.writeDirtyTables(img.refCache); err != nil {
			return err
		}
		unsynced = true
	}

	// 2. Refcount table.
	if img.refTableMoved || len(img.refTableDirty) > 0 {
		if err := barrier(); err != nil {
			return err
		}
		if err := img.writeTable(img.refTable, img.refTableOffset, &img.refTableMoved, img.refTableDirty); err != nil {
			return err
		}
		unsynced = true
	}
	if img.hdrRefTableDirty {
		if err := barrier(); err != nil {
			return err
		}
		var b [12]byte
		binary.BigEndian.PutUint64(b[0:], img.hdr.RefcountTableOffset)
		binary.BigEndian.PutUint32(b[8:], img.hdr.RefcountTableClusters)
		if _, err := img.f.WriteAt(b[:], offRefcountTableOffset); err != nil {
			return err
		}
		img.hdrRefTableDirty = false
		unsynced = true
	}

	// 3. L2 tables.
	if img.l2Cache.dirty > 0 {
		if err := barrier(); err != nil {
			return err
		}
		if err := img.writeDirtyTables(img.l2Cache); err != nil {
			return err
		}
		unsynced = true
	}

	// 4. L1 table.
	if img.l1Moved || len(img.l1Dirty) > 0 {
		if err := barrier(); err != nil {
			return err
		}
		if err := img.writeTable(img.l1, img.l1Offset, &img.l1Moved, img.l1Dirty); err != nil {
			return err
		}
		unsynced = true
	}

	// 5. Virtual size and L1 location (bytes 24-47, a single sector).
	if img.hdrDirty {
		if err := barrier(); err != nil {
			return err
		}
		var b [24]byte
		binary.BigEndian.PutUint64(b[0:], img.hdr.Size)
		binary.BigEndian.PutUint32(b[8:], img.hdr.CryptMethod)
		binary.BigEndian.PutUint32(b[12:], img.hdr.L1Size)
		binary.BigEndian.PutUint64(b[16:], img.hdr.L1TableOffset)
		if _, err := img.f.WriteAt(b[:], offSize); err != nil {
			return err
		}
		img.hdrDirty = false
		unsynced = true
	}

	if err := barrier(); err != nil {
		return err
	}
	img.dataDirty = false

	// Release clusters that nothing on disk refers to any more.
	frees := img.pendingFrees
	img.pendingFrees = nil
	for _, host := range frees {
		if err := img.addRefcount(host, -1); err != nil {
			return err
		}
	}
	return nil
}

// metadataUpdate tells test storage that metadata is being written, so
// that simulated crashes can concentrate there. Use as
// defer img.metadataUpdate()().
func (img *Image) metadataUpdate() func() {
	t, ok := img.f.(interface{ flushing(bool) })
	if !ok {
		return func() {}
	}
	t.flushing(true)
	return func() { t.flushing(false) }
}

// flushAll flushes until nothing is pending, including the refcount
// decrements applied by the previous round, so that the image on disk is
// exactly the image in memory.
func (img *Image) flushAll() error {
	for img.needsFlush() {
		if err := img.flush(); err != nil {
			return err
		}
	}
	return nil
}

// maybeFlush flushes when too much is held in memory.
func (img *Image) maybeFlush() error {
	if img.l2Cache.dirty+img.refCache.dirty > img.l2Cache.cap/2 || len(img.pendingFrees) > 1<<16 {
		return img.flush()
	}
	return nil
}

func (img *Image) writeDirtyTables(c *tableCache) error {
	for _, e := range c.dirtyEntries() {
		if _, err := img.f.WriteAt(e.buf, e.off); err != nil {
			return err
		}
		c.setDirty(e, false)
	}
	return nil
}

// writeTable writes a table of 64-bit entries (L1 or refcount table):
// completely, zero-padded to whole clusters, when it has moved, otherwise
// only its dirty entries.
func (img *Image) writeTable(t []uint64, off int64, moved *bool, dirty map[int64]struct{}) error {
	if *moved {
		buf := make([]byte, alignUp(int64(len(t))*8, img.clusterSize))
		for i, v := range t {
			binary.BigEndian.PutUint64(buf[i*8:], v)
		}
		if _, err := img.f.WriteAt(buf, off); err != nil {
			return err
		}
		*moved = false
		clear(dirty)
		return nil
	}
	for i := range dirty {
		if err := img.writeUint64(off+i*8, t[i]); err != nil {
			return err
		}
	}
	clear(dirty)
	return nil
}

// truncateFile cuts off unreferenced clusters at the end of the file. All
// changes must have been flushed with flushAll.
func (img *Image) truncateFile() error {
	last := int64(-1)
	for ti := int64(len(img.refTable)) - 1; ti >= 0 && last < 0; ti-- {
		off := int64(img.refTable[ti] & refTableMask)
		if off == 0 {
			continue
		}
		b, err := img.loadRefBlock(off)
		if err != nil {
			return err
		}
		for bi := img.refsPerBlock - 1; bi >= 0; bi-- {
			if v, _, _ := img.refGet(b, bi); v != 0 {
				last = ti*img.refsPerBlock + bi
				break
			}
		}
	}
	end := (last + 1) * img.clusterSize
	size, err := img.f.size()
	if err != nil {
		return err
	}
	if end >= size {
		return nil
	}
	if err := img.f.Truncate(end); err != nil {
		return err
	}
	img.fileEnd = end
	img.freeHint = min(img.freeHint, end)
	return img.f.Sync()
}
