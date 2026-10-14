package qcow2

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Internal snapshots.
//
// A snapshot is a copy of the active L1 table; the L2 tables and data
// clusters it reaches are shared with the active image, and their
// refcounts count one reference per L1 table reaching them. Writes to
// shared clusters copy them first (see writableL2 and writeCluster).
//
// The snapshot table is never modified in place: a new table is written
// and the header switched to it, so a crash leaves either the old or the
// new list of snapshots, at worst with leaked clusters.

// ErrSnapshotNotFound is returned for operations on an unknown snapshot.
var ErrSnapshotNotFound = errors.New("qcow2: snapshot not found")

const (
	maxSnapshots          = 65536
	maxSnapshotTableSize  = 64 << 20
	maxSnapshotExtraData  = 1024
	snapshotExtraDataSize = 24 // vm_state_size_large, disk_size, icount
)

// SnapshotInfo describes an internal snapshot.
type SnapshotInfo struct {
	// ID is the unique identifier QEMU assigns ("1", "2", ...).
	ID   string
	Name string
	// Date is when the snapshot was taken.
	Date time.Time
	// VMClock is the guest clock at the time, zero for disk-only snapshots.
	VMClock time.Duration
	// VMStateSize is the size of the saved VM state, zero for disk-only
	// snapshots.
	VMStateSize int64
	// Size is the virtual disk size when the snapshot was taken.
	Size int64
}

type snapshot struct {
	l1Offset    int64
	l1Size      uint32
	id, name    string
	dateSec     uint32
	dateNsec    uint32
	vmClockNsec uint64
	vmStateSize uint32
	extra       []byte
}

// diskSize returns the virtual size recorded in the extra data, or
// fallback when it is missing.
func (sn *snapshot) diskSize(fallback int64) int64 {
	if len(sn.extra) >= 16 {
		return int64(binary.BigEndian.Uint64(sn.extra[8:]))
	}
	return fallback
}

func (sn *snapshot) info(fallbackSize int64) SnapshotInfo {
	vmState := int64(sn.vmStateSize)
	if len(sn.extra) >= 8 {
		vmState = max(vmState, int64(binary.BigEndian.Uint64(sn.extra)))
	}
	return SnapshotInfo{
		ID:          sn.id,
		Name:        sn.name,
		Date:        time.Unix(int64(sn.dateSec), int64(sn.dateNsec)),
		VMClock:     time.Duration(sn.vmClockNsec),
		VMStateSize: vmState,
		Size:        sn.diskSize(fallbackSize),
	}
}

// loadSnapshots reads the snapshot table referenced by the header.
func (img *Image) loadSnapshots() error {
	h := img.hdr
	if h.NbSnapshots == 0 {
		return nil
	}
	if h.NbSnapshots > maxSnapshots {
		return fmt.Errorf("%w: too many snapshots", ErrCorrupt)
	}
	off := int64(h.SnapshotsOffset)
	if off == 0 || off&(img.clusterSize-1) != 0 {
		return fmt.Errorf("%w: invalid snapshot table offset", ErrCorrupt)
	}
	be := binary.BigEndian
	var size int64
	for range h.NbSnapshots {
		var e [40]byte
		if err := img.readFull(e[:], off+size); err != nil {
			return err
		}
		sn := &snapshot{
			l1Offset:    int64(be.Uint64(e[0:])),
			l1Size:      be.Uint32(e[8:]),
			dateSec:     be.Uint32(e[16:]),
			dateNsec:    be.Uint32(e[20:]),
			vmClockNsec: be.Uint64(e[24:]),
			vmStateSize: be.Uint32(e[32:]),
		}
		idSize := int64(be.Uint16(e[12:]))
		nameSize := int64(be.Uint16(e[14:]))
		extraSize := int64(be.Uint32(e[36:]))
		if extraSize > maxSnapshotExtraData {
			return fmt.Errorf("%w: snapshot extra data too large", ErrCorrupt)
		}
		if h.Version >= 3 && extraSize < 16 {
			return fmt.Errorf("%w: snapshot extra data too small", ErrCorrupt)
		}
		rest := make([]byte, extraSize+idSize+nameSize)
		if err := img.readFull(rest, off+size+40); err != nil {
			return err
		}
		sn.extra = rest[:extraSize]
		sn.id = string(rest[extraSize : extraSize+idSize])
		sn.name = string(rest[extraSize+idSize:])
		size += alignUp(40+extraSize+idSize+nameSize, 8)
		if size > maxSnapshotTableSize {
			return fmt.Errorf("%w: snapshot table too large", ErrCorrupt)
		}
		if sn.l1Offset&(img.clusterSize-1) != 0 || int64(sn.l1Size) > 32<<20/8 {
			return fmt.Errorf("%w: invalid L1 table of snapshot %q", ErrCorrupt, sn.id)
		}
		img.snapshots = append(img.snapshots, sn)
	}
	img.snapshotTableSize = size
	return nil
}

// Snapshots lists the internal snapshots of the image.
func (img *Image) Snapshots() []SnapshotInfo {
	img.mu.Lock()
	defer img.mu.Unlock()
	infos := make([]SnapshotInfo, len(img.snapshots))
	for i, sn := range img.snapshots {
		infos[i] = sn.info(img.size)
	}
	return infos
}

// findSnapshot looks a snapshot up by ID first and then by name, like
// QEMU.
func (img *Image) findSnapshot(idOrName string) (int, error) {
	for i, sn := range img.snapshots {
		if sn.id == idOrName {
			return i, nil
		}
	}
	for i, sn := range img.snapshots {
		if sn.name == idOrName {
			return i, nil
		}
	}
	return 0, fmt.Errorf("%w: %q", ErrSnapshotNotFound, idOrName)
}

// CreateSnapshot takes a disk-only snapshot of the current contents, like
// "qemu-img snapshot -c". Names must be unique.
func (img *Image) CreateSnapshot(name string) (SnapshotInfo, error) {
	img.mu.Lock()
	defer img.mu.Unlock()
	if err := img.writable(); err != nil {
		return SnapshotInfo{}, err
	}
	if name == "" || len(name) > 0xffff {
		return SnapshotInfo{}, fmt.Errorf("qcow2: invalid snapshot name %q", name)
	}
	for _, sn := range img.snapshots {
		if sn.name == name {
			return SnapshotInfo{}, fmt.Errorf("qcow2: snapshot %q already exists", name)
		}
	}
	if len(img.snapshots) >= maxSnapshots {
		return SnapshotInfo{}, errors.New("qcow2: too many snapshots")
	}
	if err := img.flushAll(); err != nil {
		return SnapshotInfo{}, err
	}
	if err := img.checkL1Refcounts(img.l1); err != nil {
		return SnapshotInfo{}, err
	}

	// Share everything the active L1 table reaches with the snapshot.
	// The COPIED flags go first: a missing flag only costs a copy on the
	// next write, while a flag on a cluster with more than one reference
	// is a corruption. The raised refcounts reach the disk before the
	// snapshot does; a crash in between merely leaks clusters.
	if err := img.clearCopiedFlags(); err != nil {
		return SnapshotInfo{}, err
	}
	if err := img.addL1Refcounts(img.l1); err != nil {
		return SnapshotInfo{}, err
	}
	var l1Off int64
	if len(img.l1) > 0 {
		buf := make([]byte, alignUp(int64(len(img.l1))*8, img.clusterSize))
		for i, e := range img.l1 {
			binary.BigEndian.PutUint64(buf[i*8:], e)
		}
		var err error
		if l1Off, err = img.allocClusters(int64(len(buf)) / img.clusterSize); err != nil {
			return SnapshotInfo{}, err
		}
		img.dataDirty = true
		if _, err := img.f.WriteAt(buf, l1Off); err != nil {
			return SnapshotInfo{}, err
		}
	}

	now := time.Now()
	sn := &snapshot{
		l1Offset: l1Off,
		l1Size:   uint32(len(img.l1)),
		id:       img.newSnapshotID(),
		name:     name,
		dateSec:  uint32(now.Unix()),
		dateNsec: uint32(now.Nanosecond()),
		extra:    make([]byte, snapshotExtraDataSize),
	}
	binary.BigEndian.PutUint64(sn.extra[8:], uint64(img.size))
	binary.BigEndian.PutUint64(sn.extra[16:], ^uint64(0)) // no icount
	snaps := append(append([]*snapshot(nil), img.snapshots...), sn)
	if err := img.writeSnapshots(snaps); err != nil {
		return SnapshotInfo{}, err
	}
	return sn.info(img.size), nil
}

// newSnapshotID returns one more than the largest numeric ID in use.
func (img *Image) newSnapshotID() string {
	var maxID uint64
	for _, sn := range img.snapshots {
		if n, err := strconv.ParseUint(sn.id, 10, 64); err == nil {
			maxID = max(maxID, n)
		}
	}
	return strconv.FormatUint(maxID+1, 10)
}

// DeleteSnapshot deletes a snapshot, looked up by ID or name, like
// "qemu-img snapshot -d". Clusters only the snapshot used are freed.
func (img *Image) DeleteSnapshot(idOrName string) error {
	img.mu.Lock()
	defer img.mu.Unlock()
	if err := img.writable(); err != nil {
		return err
	}
	i, err := img.findSnapshot(idOrName)
	if err != nil {
		return err
	}
	sn := img.snapshots[i]
	var l1 []uint64
	if err := img.readTable(&l1, sn.l1Offset, int(sn.l1Size)); err != nil {
		return err
	}
	if err := img.flushAll(); err != nil {
		return err
	}
	snaps := append(append([]*snapshot(nil), img.snapshots[:i]...), img.snapshots[i+1:]...)
	if err := img.writeSnapshots(snaps); err != nil {
		return err
	}

	// Nothing on disk refers to the snapshot's structures any more.
	if err := img.releaseL1(l1, sn.l1Offset); err != nil {
		return err
	}
	if err := img.flushAll(); err != nil {
		return err
	}
	// Clusters the active image now owns alone may be written in place
	// again.
	if err := img.syncCopiedFlags(); err != nil {
		return err
	}
	return img.flushAll()
}

// ApplySnapshot reverts the disk to a snapshot, looked up by ID or name,
// like "qemu-img snapshot -a". The current contents are lost unless
// another snapshot holds them. The virtual size becomes the one of the
// snapshot; when it differs, the image must not have bitmaps. Enabled
// bitmaps record the whole disk as changed.
func (img *Image) ApplySnapshot(idOrName string) error {
	img.mu.Lock()
	defer img.mu.Unlock()
	if err := img.writable(); err != nil {
		return err
	}
	i, err := img.findSnapshot(idOrName)
	if err != nil {
		return err
	}
	sn := img.snapshots[i]
	size := sn.diskSize(img.size)
	if size != img.size && len(img.bitmaps) > 0 {
		return fmt.Errorf("%w: applying a snapshot of a different size to an image with bitmaps", ErrUnsupported)
	}
	if size < 0 || size > 1<<62 {
		return fmt.Errorf("%w: invalid size of snapshot %q", ErrCorrupt, sn.id)
	}
	var snapL1 []uint64
	if err := img.readTable(&snapL1, sn.l1Offset, int(sn.l1Size)); err != nil {
		return err
	}
	if err := img.flushAll(); err != nil {
		return err
	}
	if err := img.checkL1Refcounts(snapL1); err != nil {
		return err
	}
	if err := img.beforeGuestChange(); err != nil {
		return err
	}
	if err := img.markDirty(0, img.size); err != nil {
		return err
	}

	// The new active L1 table shares everything with the snapshot. Its
	// references are counted before anything points to it.
	if err := img.addL1Refcounts(snapL1); err != nil {
		return err
	}
	l1 := make([]uint64, max(int64(len(snapL1)), img.l1EntriesFor(size)))
	for j, e := range snapL1 {
		l1[j] = e &^ flagCopied
	}
	var l1Off int64
	if len(l1) > 0 {
		if l1Off, err = img.allocClusters(alignUp(int64(len(l1))*8, img.clusterSize) / img.clusterSize); err != nil {
			return err
		}
	}

	// Switch to it; flush writes the table and then the header, and only
	// then drops the references of the previous active table.
	oldL1, oldOff := img.l1, img.l1Offset
	img.l1, img.l1Offset, img.l1Moved = l1, l1Off, true
	clear(img.l1Dirty)
	img.size = size
	img.hdr.Size = uint64(size)
	img.hdr.L1Size = uint32(len(l1))
	img.hdr.L1TableOffset = uint64(l1Off)
	img.hdrDirty = true
	if err := img.releaseL1(oldL1, oldOff); err != nil {
		return err
	}
	return img.flushAll()
}

// clearCopiedFlags clears the COPIED flags of the active L1 and L2 entries
// and flushes.
func (img *Image) clearCopiedFlags() error {
	for i, e := range img.l1 {
		l2 := int64(e & l1OffsetMask)
		if l2 == 0 {
			continue
		}
		if e&flagCopied != 0 {
			img.l1[i] = e &^ flagCopied
			img.l1Dirty[int64(i)] = struct{}{}
		}
		t, err := img.loadL2(l2)
		if err != nil {
			return err
		}
		changed := false
		for j := 0; j < len(t); j += 8 {
			if entry := binary.BigEndian.Uint64(t[j:]); entry&flagCopied != 0 {
				binary.BigEndian.PutUint64(t[j:], entry&^flagCopied)
				changed = true
			}
		}
		if changed {
			img.l2Cache.markDirty(l2, t)
		}
		if err := img.maybeFlush(); err != nil {
			return err
		}
	}
	return img.flushAll()
}

// addL1Refcounts adds one reference to every L2 table and data cluster an
// L1 table reaches, which then are shared, and clears the COPIED flags in
// those L2 tables.
func (img *Image) addL1Refcounts(l1 []uint64) error {
	for _, e := range l1 {
		l2 := int64(e & l1OffsetMask)
		if l2 == 0 {
			continue
		}
		t, err := img.loadL2(l2)
		if err != nil {
			return err
		}
		changed := false
		for j := 0; j < len(t); j += 8 {
			entry := binary.BigEndian.Uint64(t[j:])
			if err := img.forEntryClusters(entry, func(c int64) error { return img.addRefcount(c, 1) }); err != nil {
				return err
			}
			if entry&flagCopied != 0 {
				binary.BigEndian.PutUint64(t[j:], entry&^flagCopied)
				changed = true
			}
		}
		if changed {
			img.l2Cache.markDirty(l2, t)
		}
		if err := img.addRefcount(l2, 1); err != nil {
			return err
		}
		if err := img.maybeFlush(); err != nil {
			return err
		}
	}
	return nil
}

// checkL1Refcounts fails if adding a reference to everything an L1 table
// reaches would overflow a refcount, so that nothing is changed then.
func (img *Image) checkL1Refcounts(l1 []uint64) error {
	if img.refOrder > 4 {
		// 2^32-1 references cannot be reached.
		return nil
	}
	add := make(map[int64]uint64)
	for _, e := range l1 {
		l2 := int64(e & l1OffsetMask)
		if l2 == 0 {
			continue
		}
		add[l2]++
		t, err := img.loadL2(l2)
		if err != nil {
			return err
		}
		for j := 0; j < len(t); j += 8 {
			_ = img.forEntryClusters(binary.BigEndian.Uint64(t[j:]), func(c int64) error {
				add[c]++
				return nil
			})
		}
	}
	for c, n := range add {
		v, err := img.refcount(c)
		if err != nil {
			return err
		}
		if img.maxRefcount()-v < n {
			return fmt.Errorf("qcow2: refcount overflow at 0x%x; the refcount width (%d bits) is too small", c, 1<<img.refOrder)
		}
	}
	return nil
}

// releaseL1 drops, once the change is on disk, the references an L1 table
// stored at off held: its own clusters, its L2 tables and their data
// clusters.
func (img *Image) releaseL1(l1 []uint64, off int64) error {
	for _, e := range l1 {
		l2 := int64(e & l1OffsetMask)
		if l2 == 0 {
			continue
		}
		t, err := img.loadL2(l2)
		if err != nil {
			return err
		}
		for j := 0; j < len(t); j += 8 {
			img.releaseEntry(binary.BigEndian.Uint64(t[j:]))
		}
		img.deferFree(l2)
		if err := img.maybeFlush(); err != nil {
			return err
		}
	}
	if off != 0 {
		for c := int64(0); c < alignUp(int64(len(l1))*8, img.clusterSize); c += img.clusterSize {
			img.deferFree(off + c)
		}
	}
	return nil
}

// forEntryClusters calls fn for every host cluster an L2 entry refers to.
func (img *Image) forEntryClusters(entry uint64, fn func(int64) error) error {
	if entry&flagCompressed != 0 {
		off, size := img.compressedRange(entry)
		for c := off &^ (img.clusterSize - 1); c < off+size; c += img.clusterSize {
			if err := fn(c); err != nil {
				return err
			}
		}
		return nil
	}
	if host := int64(entry & l2OffsetMask); host != 0 {
		return fn(host)
	}
	return nil
}

// writeSnapshots writes a new snapshot table, switches the header to it
// and frees the previous one. Everything the new table refers to must
// have been flushed.
func (img *Image) writeSnapshots(snaps []*snapshot) error {
	be := binary.BigEndian
	var table []byte
	for _, sn := range snaps {
		e := make([]byte, alignUp(int64(40+len(sn.extra)+len(sn.id)+len(sn.name)), 8))
		be.PutUint64(e[0:], uint64(sn.l1Offset))
		be.PutUint32(e[8:], sn.l1Size)
		be.PutUint16(e[12:], uint16(len(sn.id)))
		be.PutUint16(e[14:], uint16(len(sn.name)))
		be.PutUint32(e[16:], sn.dateSec)
		be.PutUint32(e[20:], sn.dateNsec)
		be.PutUint64(e[24:], sn.vmClockNsec)
		be.PutUint32(e[32:], sn.vmStateSize)
		be.PutUint32(e[36:], uint32(len(sn.extra)))
		copy(e[40:], sn.extra)
		copy(e[40+len(sn.extra):], sn.id)
		copy(e[40+len(sn.extra)+len(sn.id):], sn.name)
		table = append(table, e...)
	}
	if len(table) > maxSnapshotTableSize {
		return errors.New("qcow2: snapshot table too large")
	}
	var off int64
	if len(table) > 0 {
		buf := make([]byte, alignUp(int64(len(table)), img.clusterSize))
		copy(buf, table)
		var err error
		if off, err = img.allocClusters(int64(len(buf)) / img.clusterSize); err != nil {
			return err
		}
		img.dataDirty = true
		if _, err := img.f.WriteAt(buf, off); err != nil {
			return err
		}
	}
	if err := img.flushAll(); err != nil {
		return err
	}
	h := *img.hdr
	h.raw = bytes.Clone(img.hdr.raw)
	h.NbSnapshots = uint32(len(snaps))
	h.SnapshotsOffset = uint64(off)
	oldOff, oldSize := int64(img.hdr.SnapshotsOffset), img.snapshotTableSize
	if err := img.writeHeader(&h); err != nil {
		return err
	}
	img.snapshots = snaps
	img.snapshotTableSize = int64(len(table))
	if oldOff != 0 {
		for c := int64(0); c < alignUp(oldSize, img.clusterSize); c += img.clusterSize {
			img.deferFree(oldOff + c)
		}
	}
	return img.flushAll()
}
