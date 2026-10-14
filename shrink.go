package qcow2

import (
	"encoding/binary"
	"fmt"
)

// Shrink reduces the virtual disk to size bytes (rounded up to a multiple
// of 512), like "qemu-img resize --shrink". Data past the new end is
// discarded and its clusters are freed; internal snapshots keep theirs.
// The rest of the last cluster is zeroed, so that growing the disk again
// shows zeros there.
func (img *Image) Shrink(size int64) error {
	img.mu.Lock()
	defer img.mu.Unlock()
	if err := img.writable(); err != nil {
		return err
	}
	size = alignUp(size, 512)
	if size < 0 || size > img.size {
		return fmt.Errorf("qcow2: cannot shrink from %d to %d bytes", img.size, size)
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
	cs := img.clusterSize

	// The tail of the last cluster.
	if in := size & (cs - 1); in != 0 {
		entry, err := img.l2Entry(size)
		if err != nil {
			return err
		}
		if !img.readsZero(entry) {
			if err := img.writeCluster(size, make([]byte, cs-in)); err != nil {
				return err
			}
		}
	}

	// Clusters past the end: unmap them in the first L2 table concerned
	// and drop the whole L2 tables after it. The L1 table keeps its size.
	first := alignUp(size, cs)
	perL1 := cs << img.l2Bits
	if i := img.l1Index(first); first&(perL1-1) != 0 && i < int64(len(img.l1)) && img.l1[i]&l1OffsetMask != 0 {
		l2Off, t, err := img.writableL2(i)
		if err != nil {
			return err
		}
		for idx := img.l2Index(first); idx < perL1/cs; idx++ {
			if entry := binary.BigEndian.Uint64(t[idx*8:]); entry != 0 {
				if err := img.setL2Entry(l2Off, t, idx, 0); err != nil {
					return err
				}
				img.releaseEntry(entry)
			}
		}
	}
	for i := alignUp(first, perL1) / perL1; i < int64(len(img.l1)); i++ {
		l2 := int64(img.l1[i] & l1OffsetMask)
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
		img.l1[i] = 0
		img.l1Dirty[i] = struct{}{}
		if err := img.maybeFlush(); err != nil {
			return err
		}
	}

	// The new size reaches the disk after the tables (flush step 5).
	img.size = size
	img.hdr.Size = uint64(size)
	img.hdrDirty = true
	if err := img.resizeBitmaps(); err != nil {
		return err
	}
	if img.opts.WriteThrough {
		return img.flush()
	}
	return nil
}
