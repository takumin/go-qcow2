package qcow2

import (
	"encoding/binary"
	"fmt"
)

// Repair recomputes the refcount of every cluster from the metadata and
// fixes those that are wrong, then the COPIED flags of the active tables,
// like "qemu-img check -r all". It also clears the dirty and corrupt
// flags of the header. It returns what Check found before the repair, and
// fails without changing anything when the metadata itself is broken
// (invalid offsets or tables), which needs a manual rescue.
//
// The repair is crash safe: refcounts that are too low are raised first,
// refcounts that are too high are lowered next and COPIED flags are set
// only once the refcounts they rely on are on disk.
func (img *Image) Repair() (*CheckResult, error) {
	img.mu.Lock()
	defer img.mu.Unlock()
	if err := img.writable(); err != nil {
		return nil, err
	}
	return img.repair()
}

func (img *Image) repair() (*CheckResult, error) {
	res, rc, err := img.check()
	if err != nil {
		return nil, err
	}
	if res.unfixable > 0 {
		return res, fmt.Errorf("%w: %d corruptions cannot be repaired (first: %s)", ErrCorrupt, res.unfixable, res.Errors[0])
	}

	// While refcounts are wrong, allocations (of refcount blocks) must not
	// pick clusters that are in use.
	img.inUse = rc.refs
	defer func() { img.inUse = nil }()
	for _, u := range res.under {
		if u.excess > uint64(img.maxRefcount()) {
			return res, fmt.Errorf("qcow2: cluster 0x%x has more references than the refcount width allows", u.off)
		}
		if err := img.addRefcount(u.off, int64(u.excess)); err != nil {
			return res, err
		}
	}
	for _, l := range res.leaks {
		if err := img.addRefcount(l.off, -int64(l.excess)); err != nil {
			return res, err
		}
	}
	if err := img.flushAll(); err != nil {
		return res, err
	}
	img.inUse = nil

	if err := img.syncCopiedFlags(); err != nil {
		return res, err
	}
	if err := img.flushAll(); err != nil {
		return res, err
	}
	if flags := img.hdr.IncompatibleFeatures; flags&(incompatDirty|incompatCorrupt) != 0 {
		flags &^= incompatDirty | incompatCorrupt
		if err := img.writeUint64(offIncompatibleFeatures, flags); err != nil {
			return res, err
		}
		if err := img.f.Sync(); err != nil {
			return res, err
		}
		img.hdr.IncompatibleFeatures = flags
		if len(img.hdr.raw) >= offIncompatibleFeatures+8 {
			binary.BigEndian.PutUint64(img.hdr.raw[offIncompatibleFeatures:], flags)
		}
	}
	return res, nil
}

// syncCopiedFlags makes the COPIED flags of the active L1 and L2 entries
// say whether their cluster has a refcount of exactly 1. Refcounts must be
// on disk before the flags that rely on them are written.
func (img *Image) syncCopiedFlags() error {
	copied := func(entry uint64, host int64) (uint64, error) {
		n, err := img.refcount(host)
		if err != nil {
			return 0, err
		}
		if n == 1 {
			return entry | flagCopied, nil
		}
		return entry &^ flagCopied, nil
	}
	for i, e := range img.l1 {
		l2 := int64(e & l1OffsetMask)
		if l2 == 0 {
			continue
		}
		ne, err := copied(e, l2)
		if err != nil {
			return err
		}
		if ne != e {
			img.l1[i] = ne
			img.l1Dirty[int64(i)] = struct{}{}
		}
		t, err := img.loadL2(l2)
		if err != nil {
			return err
		}
		changed := false
		for j := 0; j < len(t); j += 8 {
			entry := binary.BigEndian.Uint64(t[j:])
			host := int64(entry & l2OffsetMask)
			if entry&flagCompressed != 0 || host == 0 {
				continue
			}
			ne, err := copied(entry, host)
			if err != nil {
				return err
			}
			if ne != entry {
				binary.BigEndian.PutUint64(t[j:], ne)
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
	return nil
}
