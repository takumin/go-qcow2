package qcow2

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

func (img *Image) l1Index(g int64) int64 { return g >> (img.clusterBits + img.l2Bits) }
func (img *Image) l2Index(g int64) int64 { return (g >> img.clusterBits) & (1<<img.l2Bits - 1) }

// l2Entry returns the L2 entry mapping guest offset g, or 0 if no L2 table
// is allocated for it.
func (img *Image) l2Entry(g int64) (uint64, error) {
	i := img.l1Index(g)
	if i >= int64(len(img.l1)) {
		return 0, nil
	}
	off := int64(img.l1[i] & l1OffsetMask)
	if off == 0 {
		return 0, nil
	}
	t, err := img.loadL2(off)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(t[img.l2Index(g)*8:]), nil
}

func (img *Image) loadL2(off int64) ([]byte, error) {
	if off&(img.clusterSize-1) != 0 {
		return nil, fmt.Errorf("%w: unaligned L2 table offset 0x%x", ErrCorrupt, off)
	}
	if t := img.l2Cache.get(off); t != nil {
		return t, nil
	}
	t := make([]byte, img.clusterSize)
	if err := img.readFull(t, off); err != nil {
		return nil, err
	}
	img.l2Cache.put(off, t, false)
	return t, nil
}

// writableL2 returns an L2 table for L1 index i that may be modified in
// place: it allocates a new table when none exists and copies the table
// when it is shared with a snapshot.
func (img *Image) writableL2(i int64) (int64, []byte, error) {
	e := img.l1[i]
	off := int64(e & l1OffsetMask)
	if off != 0 && e&flagCopied != 0 {
		t, err := img.loadL2(off)
		return off, t, err
	}

	var t []byte
	if off == 0 {
		t = make([]byte, img.clusterSize)
	} else {
		// Shared L2 table (snapshot). Refcounts of data clusters already
		// count one reference per L1 table reaching them, so copying the
		// table does not change them; the copy just must not claim
		// exclusive ownership.
		old, err := img.loadL2(off)
		if err != nil {
			return 0, nil, err
		}
		t = bytes.Clone(old)
		for j := 0; j < len(t); j += 8 {
			entry := binary.BigEndian.Uint64(t[j:])
			binary.BigEndian.PutUint64(t[j:], entry&^flagCopied)
		}
	}
	newOff, err := img.allocClusters(1)
	if err != nil {
		return 0, nil, err
	}
	img.l2Cache.put(newOff, t, true)
	img.l1[i] = uint64(newOff) | flagCopied
	img.l1Dirty[i] = struct{}{}
	if off != 0 {
		img.deferFree(off)
	}
	return newOff, t, nil
}

// writeCluster writes data (which must not cross a cluster boundary) at
// guest offset g.
func (img *Image) writeCluster(g int64, data []byte) error {
	l2Off, t, err := img.writableL2(img.l1Index(g))
	if err != nil {
		return err
	}
	idx := img.l2Index(g)
	entry := binary.BigEndian.Uint64(t[idx*8:])
	in := g & (img.clusterSize - 1)

	if entry&flagCompressed == 0 && entry&flagCopied != 0 {
		host := int64(entry & l2OffsetMask)
		if host != 0 {
			if host&(img.clusterSize-1) != 0 {
				return fmt.Errorf("%w: unaligned data cluster offset 0x%x", ErrCorrupt, host)
			}
			if img.hdr.Version < 3 || entry&flagZero == 0 {
				// Exclusively owned data cluster: overwrite in place.
				img.dataDirty = true
				_, err := img.f.WriteAt(data, host+in)
				return err
			}
			// Preallocated zero cluster we own: materialise it in place.
			buf := make([]byte, img.clusterSize)
			copy(buf[in:], data)
			img.dataDirty = true
			if _, err := img.f.WriteAt(buf, host); err != nil {
				return err
			}
			return img.setL2Entry(l2Off, t, idx, uint64(host)|flagCopied)
		}
	}

	// Allocate a new cluster holding the old contents merged with data.
	buf := make([]byte, img.clusterSize)
	if int64(len(data)) < img.clusterSize {
		base := g &^ (img.clusterSize - 1)
		if err := img.readFromEntry(entry, base, buf); err != nil {
			return err
		}
	}
	copy(buf[in:], data)
	newOff, err := img.allocClusters(1)
	if err != nil {
		return err
	}
	img.dataDirty = true
	if _, err := img.f.WriteAt(buf, newOff); err != nil {
		return err
	}
	if err := img.setL2Entry(l2Off, t, idx, uint64(newOff)|flagCopied); err != nil {
		return err
	}
	img.releaseEntry(entry)
	return nil
}

func (img *Image) setL2Entry(l2Off int64, t []byte, idx int64, v uint64) error {
	binary.BigEndian.PutUint64(t[idx*8:], v)
	img.l2Cache.markDirty(l2Off, t)
	return nil
}

// releaseEntry schedules dropping the references an L2 entry held on host
// clusters, once the L2 change has been written.
func (img *Image) releaseEntry(entry uint64) {
	if entry&flagCompressed != 0 {
		off, size := img.compressedRange(entry)
		for c := off &^ (img.clusterSize - 1); c < off+size; c += img.clusterSize {
			img.deferFree(c)
		}
		return
	}
	if host := int64(entry & l2OffsetMask); host != 0 {
		img.deferFree(host)
	}
}

// compressedRange returns the host offset and length of a compressed
// cluster's data.
func (img *Image) compressedRange(entry uint64) (int64, int64) {
	off := int64(entry & img.coffsetMask)
	sectors := int64((entry>>img.csizeShift)&img.csizeMask) + 1
	return off, sectors*512 - off&511
}

func (img *Image) decompress(entry uint64) ([]byte, error) {
	if img.decompBuf != nil && img.decompEntry == entry {
		return img.decompBuf, nil
	}
	off, size := img.compressedRange(entry)
	src := make([]byte, size)
	if err := img.readFull(src, off); err != nil {
		return nil, err
	}
	dst := make([]byte, img.clusterSize)
	var r io.Reader
	switch img.hdr.CompressionType {
	case compressionZlib:
		r = flate.NewReader(bytes.NewReader(src))
	case compressionZstd:
		d, err := zstd.NewReader(bytes.NewReader(src), zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, err
		}
		defer d.Close()
		r = d
	default:
		return nil, fmt.Errorf("%w: compression type %d", ErrUnsupported, img.hdr.CompressionType)
	}
	if _, err := io.ReadFull(r, dst); err != nil {
		return nil, fmt.Errorf("%w: decompressing cluster at 0x%x: %v", ErrCorrupt, off, err)
	}
	img.decompEntry, img.decompBuf = entry, dst
	return dst, nil
}
