package qcow2

import (
	"encoding/binary"
	"fmt"
)

// CheckResult reports the refcount consistency of an image, like
// "qemu-img check".
type CheckResult struct {
	// Leaks is the number of clusters whose refcount is higher than the
	// number of references to them. Leaks waste space but are harmless;
	// RepairLeaks reclaims them. A crash while writing can leave leaks.
	Leaks int64
	// LeaksPastEOF counts leaked refcounts of clusters beyond the end of
	// the file, which a crash can leave behind when the file did not grow
	// as far as the refcounts. qemu-img check does not report them;
	// RepairLeaks reclaims them too.
	LeaksPastEOF int64
	// Corruptions is the number of problems that can lose data: clusters
	// referenced more often than their refcount says, invalid offsets and
	// wrong COPIED flags.
	Corruptions int64
	// Errors describes the corruptions (at most 100).
	Errors []string

	leaks []leak
}

type leak struct {
	off    int64
	excess uint64
}

const maxCheckErrors = 100

func (r *CheckResult) corrupt(format string, args ...any) {
	r.Corruptions++
	if len(r.Errors) < maxCheckErrors {
		r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
	}
}

// Check recomputes the refcount of every cluster from the image metadata
// (header, L1/L2 tables, refcount structures, internal snapshots and
// persistent bitmaps) and compares it with the stored refcounts. Pending
// changes of a writable image are flushed first.
func (img *Image) Check() (*CheckResult, error) {
	img.mu.Lock()
	defer img.mu.Unlock()
	if img.closed {
		return nil, ErrClosed
	}
	return img.check()
}

// RepairLeaks lowers the refcount of leaked clusters to their actual number
// of references, making them available for reuse (and punching them out of
// the file). It refuses to touch an image with corruptions. Only leaked
// clusters are written, so a crash during the repair is harmless. It
// returns the number of clusters repaired (Leaks + LeaksPastEOF).
func (img *Image) RepairLeaks() (int64, error) {
	img.mu.Lock()
	defer img.mu.Unlock()
	if img.closed {
		return 0, ErrClosed
	}
	if img.readOnly {
		return 0, ErrReadOnly
	}
	res, err := img.check()
	if err != nil {
		return 0, err
	}
	if res.Corruptions > 0 {
		return 0, fmt.Errorf("%w: %d corruptions found, not repairing (first: %s)", ErrCorrupt, res.Corruptions, res.Errors[0])
	}
	for _, l := range res.leaks {
		if err := img.addRefcount(l.off, -int64(l.excess)); err != nil {
			return 0, err
		}
	}
	return int64(len(res.leaks)), img.flushAll()
}

// refCounter accumulates the references found in the metadata.
type refCounter struct {
	img  *Image
	refs []uint64 // per cluster of the file
	res  *CheckResult
}

// ref records a reference to every cluster overlapping [off, off+n).
func (rc *refCounter) ref(what string, off, n int64) {
	img := rc.img
	if n <= 0 {
		return
	}
	for c := off >> img.clusterBits; c <= (off+n-1)>>img.clusterBits; c++ {
		if c >= int64(len(rc.refs)) {
			rc.res.corrupt("%s at 0x%x is beyond the end of the file", what, c<<img.clusterBits)
			return
		}
		rc.refs[c]++
	}
}

func (rc *refCounter) aligned(what string, off int64) bool {
	if off&(rc.img.clusterSize-1) != 0 {
		rc.res.corrupt("%s offset 0x%x is not cluster aligned", what, off)
		return false
	}
	return true
}

func (img *Image) check() (*CheckResult, error) {
	if !img.readOnly {
		if err := img.flushAll(); err != nil {
			return nil, err
		}
	}
	fileSize, err := img.f.size()
	if err != nil {
		return nil, err
	}
	res := &CheckResult{}
	rc := &refCounter{img: img, refs: make([]uint64, alignUp(fileSize, img.clusterSize)>>img.clusterBits), res: res}
	cs := img.clusterSize

	rc.ref("header", 0, cs)

	// Refcount structures.
	rc.ref("refcount table", img.refTableOffset, int64(len(img.refTable))*8)
	for ti, e := range img.refTable {
		if off := int64(e & refTableMask); off != 0 && rc.aligned(fmt.Sprintf("refcount block %d", ti), off) {
			rc.ref("refcount block", off, cs)
		}
	}

	// Active L1 table, with COPIED flags checked afterwards.
	rc.ref("L1 table", img.l1Offset, int64(len(img.l1))*8)
	if err := rc.walkL1(img.l1, "active"); err != nil {
		return nil, err
	}

	// Internal snapshots.
	if err := rc.walkSnapshots(); err != nil {
		return nil, err
	}

	// Persistent bitmaps, which QEMU only honours while the autoclear bit
	// is set (opening an image writable clears it).
	if img.hdr.AutoclearFeatures&1 != 0 {
		if err := rc.walkBitmaps(); err != nil {
			return nil, err
		}
	}

	// Compare with the stored refcounts.
	covered := make([]bool, len(rc.refs))
	for ti, e := range img.refTable {
		blk := int64(e & refTableMask)
		if blk == 0 || blk&(cs-1) != 0 {
			continue
		}
		b, err := img.loadRefBlock(blk)
		if err != nil {
			return nil, err
		}
		for bi := int64(0); bi < img.refsPerBlock; bi++ {
			c := int64(ti)*img.refsPerBlock + bi
			stored, _, _ := img.refGet(b, bi)
			var want uint64
			if c < int64(len(rc.refs)) {
				want = rc.refs[c]
				covered[c] = true
			}
			switch {
			case stored > want:
				if c < int64(len(rc.refs)) {
					res.Leaks++
				} else {
					res.LeaksPastEOF++
				}
				res.leaks = append(res.leaks, leak{off: c << img.clusterBits, excess: stored - want})
			case stored < want:
				res.corrupt("cluster 0x%x has refcount %d but %d references", c<<img.clusterBits, stored, want)
			}
		}
	}
	for c, n := range rc.refs {
		if n > 0 && !covered[c] {
			res.corrupt("cluster 0x%x has %d references but no refcount block", int64(c)<<img.clusterBits, n)
		}
	}

	if err := img.checkCopied(res); err != nil {
		return nil, err
	}
	return res, nil
}

// walkL1 records the references of an L1 table: its L2 tables and the data
// clusters they map.
func (rc *refCounter) walkL1(l1 []uint64, name string) error {
	img := rc.img
	for i, e := range l1 {
		l2 := int64(e & l1OffsetMask)
		if l2 == 0 || !rc.aligned(fmt.Sprintf("%s L2 table %d", name, i), l2) {
			continue
		}
		rc.ref(name+" L2 table", l2, img.clusterSize)
		if l2 >= int64(len(rc.refs))<<img.clusterBits {
			continue
		}
		t, err := img.loadL2(l2)
		if err != nil {
			return err
		}
		for j := 0; j < len(t); j += 8 {
			entry := binary.BigEndian.Uint64(t[j:])
			if entry&flagCompressed != 0 {
				off, size := img.compressedRange(entry)
				rc.ref(name+" compressed cluster", off, size)
				continue
			}
			if host := int64(entry & l2OffsetMask); host != 0 && rc.aligned(name+" data cluster", host) {
				rc.ref(name+" data cluster", host, img.clusterSize)
			}
		}
	}
	return nil
}

func (rc *refCounter) walkSnapshots() error {
	img := rc.img
	off := int64(img.hdr.SnapshotsOffset)
	start := off
	for i := uint32(0); i < img.hdr.NbSnapshots; i++ {
		var h [40]byte
		if err := img.readFull(h[:], off); err != nil {
			return err
		}
		be := binary.BigEndian
		l1Off := int64(be.Uint64(h[0:]))
		l1Size := int64(be.Uint32(h[8:]))
		idSize := int64(be.Uint16(h[12:]))
		nameSize := int64(be.Uint16(h[14:]))
		extra := int64(be.Uint32(h[36:]))
		off += alignUp(40+extra+idSize+nameSize, 8)

		name := fmt.Sprintf("snapshot %d", i)
		if l1Size > 32<<20/8 {
			rc.res.corrupt("%s: L1 table too large", name)
			continue
		}
		if !rc.aligned(name+" L1 table", l1Off) {
			continue
		}
		rc.ref(name+" L1 table", l1Off, l1Size*8)
		var l1 []uint64
		if err := img.readTable(&l1, l1Off, int(l1Size)); err != nil {
			return err
		}
		if err := rc.walkL1(l1, name); err != nil {
			return err
		}
	}
	rc.ref("snapshot table", start, off-start)
	return nil
}

func (rc *refCounter) walkBitmaps() error {
	img := rc.img
	var ext []byte
	for _, e := range img.hdr.extensions {
		if e.Type == extBitmaps {
			ext = e.Data
		}
	}
	if len(ext) < 24 {
		return nil
	}
	be := binary.BigEndian
	n := be.Uint32(ext[0:])
	dirSize := int64(be.Uint64(ext[8:]))
	dirOff := int64(be.Uint64(ext[16:]))
	if dirSize > 64<<20 {
		rc.res.corrupt("bitmap directory too large")
		return nil
	}
	rc.ref("bitmap directory", dirOff, dirSize)
	dir := make([]byte, dirSize)
	if err := img.readFull(dir, dirOff); err != nil {
		return err
	}
	for i, p := uint32(0), int64(0); i < n && p+24 <= dirSize; i++ {
		tOff := int64(be.Uint64(dir[p:]))
		tSize := int64(be.Uint32(dir[p+8:]))
		nameSize := int64(be.Uint16(dir[p+18:]))
		extra := int64(be.Uint32(dir[p+20:]))
		p += alignUp(24+extra+nameSize, 8)

		rc.ref("bitmap table", tOff, tSize*8)
		var t []uint64
		if err := img.readTable(&t, tOff, int(tSize)); err != nil {
			return err
		}
		for _, e := range t {
			// 0: all zeros, 1: all ones, otherwise a data cluster.
			if off := int64(e & 0x00fffffffffffe00); off != 0 {
				rc.ref("bitmap data", off, img.clusterSize)
			}
		}
	}
	return nil
}

// checkCopied verifies that the COPIED flags of the active tables match
// "refcount == 1", which writes rely on.
func (img *Image) checkCopied(res *CheckResult) error {
	for i, e := range img.l1 {
		l2 := int64(e & l1OffsetMask)
		if l2 == 0 || l2&(img.clusterSize-1) != 0 {
			continue
		}
		n, err := img.refcount(l2)
		if err != nil {
			return err
		}
		if (n == 1) != (e&flagCopied != 0) {
			res.corrupt("L2 table %d at 0x%x: COPIED flag does not match refcount %d", i, l2, n)
		}
		t, err := img.loadL2(l2)
		if err != nil {
			return err
		}
		for j := 0; j < len(t); j += 8 {
			entry := binary.BigEndian.Uint64(t[j:])
			host := int64(entry & l2OffsetMask)
			if entry&flagCompressed != 0 || host == 0 || host&(img.clusterSize-1) != 0 {
				continue
			}
			n, err := img.refcount(host)
			if err != nil {
				return err
			}
			if (n == 1) != (entry&flagCopied != 0) {
				res.corrupt("data cluster at 0x%x: COPIED flag does not match refcount %d", host, n)
			}
		}
	}
	return nil
}
