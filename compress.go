package qcow2

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/klauspost/compress/flate"
	"github.com/klauspost/compress/zstd"
)

// QEMU inflates zlib clusters with a 4 KiB window (windowBits -12), so
// matches must not reach further back.
const zlibWindow = 4096

// WriteCompressedAt writes whole clusters compressed, like
// "qemu-img convert -c". off must be a multiple of the cluster size and
// len(p) too, except that a write ending at the end of the virtual disk
// may end with a partial cluster. Clusters that do not get smaller are
// stored uncompressed. Compressed clusters are packed into shared host
// clusters, and a later write to one of them stores it uncompressed again.
func (img *Image) WriteCompressedAt(p []byte, off int64) (int, error) {
	img.mu.Lock()
	defer img.mu.Unlock()
	if err := img.writable(); err != nil {
		return 0, err
	}
	if off < 0 || off > img.size || int64(len(p)) > img.size-off {
		return 0, ErrOutOfRange
	}
	cs := img.clusterSize
	if off&(cs-1) != 0 || (int64(len(p))&(cs-1) != 0 && off+int64(len(p)) != img.size) {
		return 0, errors.New("qcow2: compressed writes must cover whole clusters")
	}
	if err := img.beforeGuestChange(); err != nil {
		return 0, err
	}
	if err := img.markDirty(off, off+int64(len(p))); err != nil {
		return 0, err
	}
	buf := make([]byte, cs)
	done := 0
	for done < len(p) {
		n := int(min(cs, int64(len(p)-done)))
		g := off + int64(done)
		copy(buf, p[done:done+n])
		clear(buf[n:])
		if err := img.writeCompressedCluster(g, buf); err != nil {
			return done, err
		}
		if err := img.maybeFlush(); err != nil {
			return done, err
		}
		done += n
	}
	if img.opts.WriteThrough {
		if err := img.flush(); err != nil {
			return 0, err
		}
	}
	return done, nil
}

// writeCompressedCluster stores the cluster at guest offset g, whose
// contents are data (one full cluster), compressed when that saves space.
func (img *Image) writeCompressedCluster(g int64, data []byte) error {
	c, err := img.compress(data)
	if err != nil {
		return err
	}
	if int64(len(c)) >= img.clusterSize {
		return img.writeCluster(g, data)
	}
	l2Off, t, err := img.writableL2(img.l1Index(g))
	if err != nil {
		return err
	}
	host, err := img.allocBytes(int64(len(c)))
	if err != nil {
		return err
	}
	img.dataDirty = true
	if _, err := img.f.WriteAt(c, host); err != nil {
		return err
	}
	// The size field counts the 512-byte sectors touched, minus one.
	sectors := (host+int64(len(c))-1)>>9 - host>>9
	entry := flagCompressed | uint64(sectors)<<img.csizeShift | uint64(host)
	idx := img.l2Index(g)
	old := binary.BigEndian.Uint64(t[idx*8:])
	if err := img.setL2Entry(l2Off, t, idx, entry); err != nil {
		return err
	}
	img.releaseEntry(old)
	return nil
}

// allocBytes returns n bytes (less than a cluster) of host space for
// compressed data, packed after the previous allocation when it fits into
// the same cluster, and takes a reference to the cluster.
func (img *Image) allocBytes(n int64) (int64, error) {
	cs := img.clusterSize
	var off int64
	if pos := img.compPos; pos != 0 && pos&(cs-1)+n <= cs {
		if err := img.addRefcount(pos&^(cs-1), 1); err != nil {
			return 0, err
		}
		off = pos
	} else {
		c, err := img.allocClusters(1)
		if err != nil {
			return 0, err
		}
		off = c
	}
	if uint64(off+n) > img.coffsetMask {
		return 0, fmt.Errorf("%w: compressed cluster beyond the addressable range", ErrUnsupported)
	}
	img.compPos = off + n
	if img.compPos&(cs-1) == 0 {
		img.compPos = 0
	}
	return off, nil
}

// compress compresses one cluster with the image's compression type.
func (img *Image) compress(data []byte) ([]byte, error) {
	switch img.hdr.CompressionType {
	case compressionZlib:
		var b bytes.Buffer
		if img.zlibWriter == nil {
			w, err := flate.NewWriterWindow(&b, zlibWindow)
			if err != nil {
				return nil, err
			}
			img.zlibWriter = w
		} else {
			img.zlibWriter.Reset(&b)
		}
		if _, err := img.zlibWriter.Write(data); err != nil {
			return nil, err
		}
		if err := img.zlibWriter.Close(); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	case compressionZstd:
		if img.zstdEncoder == nil {
			e, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
			if err != nil {
				return nil, err
			}
			img.zstdEncoder = e
		}
		return img.zstdEncoder.EncodeAll(data, nil), nil
	default:
		return nil, fmt.Errorf("%w: compression type %d", ErrUnsupported, img.hdr.CompressionType)
	}
}
