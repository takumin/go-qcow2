package qcow2

import "encoding/binary"

// Discard makes the guest range [off, off+length) read as zeros and
// releases the clusters it no longer needs, like a TRIM/UNMAP from the
// guest. It is WriteZeroes with unmap set.
func (img *Image) Discard(off, length int64) error {
	return img.WriteZeroes(off, length, true)
}

// WriteZeroes makes the guest range [off, off+length) read as zeros
// without writing zeros where the metadata can express them.
//
// With unmap, clusters fully inside the range are unmapped: their L2 entry
// becomes unallocated, or a zero cluster when a backing file would
// otherwise show through (version 3). The freed host clusters are reused
// by later writes and, on Linux, punched out of the file. Without unmap,
// clusters the image owns keep their host cluster and become preallocated
// zero clusters (version 3), so that later writes to them need no
// allocation; other clusters become zero clusters.
//
// Parts of clusters at the edges of the range are overwritten with zeros
// unless they already read as zeros. Version 2 images cannot express zero
// clusters: there whole clusters are unmapped (with unmap and no backing
// file) or overwritten with zeros.
//
// Data shared with internal snapshots is left intact.
func (img *Image) WriteZeroes(off, length int64, unmap bool) error {
	img.mu.Lock()
	defer img.mu.Unlock()
	if img.closed {
		return ErrClosed
	}
	if img.readOnly {
		return ErrReadOnly
	}
	if off < 0 || length < 0 || off > img.size || length > img.size-off {
		return ErrOutOfRange
	}
	cs := img.clusterSize
	end := off + length
	if err := img.beforeGuestChange(); err != nil {
		return err
	}
	if err := img.markDirty(off, end); err != nil {
		return err
	}
	var zeros []byte
	for g := off; g < end; {
		base := g &^ (cs - 1)
		clusterEnd := min(base+cs, img.size)
		n := min(clusterEnd, end) - g
		whole := g == base && g+n == clusterEnd

		if whole && img.backing == nil {
			// Skip the rest of an L1 range without an L2 table: it already
			// reads as zeros.
			i := img.l1Index(g)
			if i >= int64(len(img.l1)) || img.l1[i]&l1OffsetMask == 0 {
				next := (i + 1) << (img.clusterBits + img.l2Bits)
				if skip := min(next, end&^(cs-1)); skip > g {
					g = skip
					continue
				}
			}
		}

		entry, err := img.l2Entry(g)
		if err != nil {
			return err
		}
		zero := img.readsZero(entry)
		v2 := img.hdr.Version < 3
		switch {
		case !whole, v2 && (img.backing != nil || !unmap):
			if !zero {
				if zeros == nil {
					zeros = make([]byte, cs)
				}
				err = img.writeCluster(g, zeros[:n])
			}
		case !unmap && entry&(flagCompressed|flagCopied) == flagCopied && entry&l2OffsetMask != 0:
			// Owned: keep the host cluster as a preallocated zero cluster.
			if entry&flagZero == 0 {
				err = img.zeroOwnedCluster(g, entry)
			}
		case !zero || unmap && entry&l2OffsetMask != 0:
			err = img.unmapCluster(g, !unmap)
		}
		if err != nil {
			return err
		}
		if err := img.maybeFlush(); err != nil {
			return err
		}
		g += n
	}
	if img.opts.WriteThrough {
		return img.flush()
	}
	return nil
}

// readsZero reports whether a cluster described by entry reads as zeros
// without looking at its data.
func (img *Image) readsZero(entry uint64) bool {
	switch {
	case entry&flagCompressed != 0:
		return false
	case img.hdr.Version >= 3 && entry&flagZero != 0:
		return true
	case entry&l2OffsetMask != 0:
		return false
	default:
		return img.backing == nil
	}
}

// unmapCluster drops the data cluster mapped at guest offset g, leaving a
// cluster that reads as zeros: a zero cluster when zeroFlag is set or a
// backing file would show through, otherwise an unallocated one.
func (img *Image) unmapCluster(g int64, zeroFlag bool) error {
	var target uint64
	if zeroFlag || img.backing != nil {
		target = flagZero
	}
	l2Off, t, err := img.writableL2(img.l1Index(g))
	if err != nil {
		return err
	}
	idx := img.l2Index(g)
	entry := binary.BigEndian.Uint64(t[idx*8:])
	if err := img.setL2Entry(l2Off, t, idx, target); err != nil {
		return err
	}
	img.releaseEntry(entry)
	return nil
}

// zeroOwnedCluster turns the data cluster mapped at guest offset g, which
// the image owns, into a preallocated zero cluster.
func (img *Image) zeroOwnedCluster(g int64, entry uint64) error {
	l2Off, t, err := img.writableL2(img.l1Index(g))
	if err != nil {
		return err
	}
	return img.setL2Entry(l2Off, t, img.l2Index(g), entry|flagZero)
}
