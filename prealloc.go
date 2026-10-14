package qcow2

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Preallocation modes of CreateOptions.Preallocation, as in qemu-img.
const (
	// PreallocOff allocates clusters when they are first written.
	PreallocOff = "off"
	// PreallocMetadata allocates the L2 tables and maps every cluster to
	// a host cluster, so that writes never change the metadata. The file
	// is sparse.
	PreallocMetadata = "metadata"
	// PreallocFalloc is PreallocMetadata plus reserving the space of the
	// data clusters in the file system (fallocate on Linux; elsewhere they
	// are written with zeros).
	PreallocFalloc = "falloc"
	// PreallocFull is PreallocMetadata plus writing zeros to the data
	// clusters.
	PreallocFull = "full"
)

var errAllocUnsupported = errors.New("qcow2: fallocate not supported")

// preallocate maps every guest cluster of a new image to a host cluster,
// placing each L2 table right before the data clusters it maps.
func (img *Image) preallocate(mode string) error {
	cs := img.clusterSize
	perL2 := int64(1) << img.l2Bits
	clusters := (img.size + cs - 1) / cs
	for i := range int64(len(img.l1)) {
		n := min(perL2, clusters-i*perL2)
		l2, err := img.allocClusters(1)
		if err != nil {
			return err
		}
		data, err := img.allocClusters(n)
		if err != nil {
			return err
		}
		t := make([]byte, cs)
		for j := range n {
			binary.BigEndian.PutUint64(t[j*8:], uint64(data+j*cs)|flagCopied)
		}
		img.l2Cache.put(l2, t, true)
		img.l1[i] = uint64(l2) | flagCopied
		if err := img.fillClusters(mode, data, n*cs); err != nil {
			return err
		}
		if err := img.maybeFlush(); err != nil {
			return err
		}
	}
	// Data clusters at the end of the file must exist for QEMU.
	if size, err := img.f.size(); err != nil {
		return err
	} else if size < img.fileEnd {
		return img.f.Truncate(img.fileEnd)
	}
	return nil
}

// fillClusters reserves or zeroes the space of n bytes of data clusters
// at off, as the preallocation mode requires.
func (img *Image) fillClusters(mode string, off, n int64) error {
	switch mode {
	case PreallocMetadata:
		return nil
	case PreallocFalloc:
		if a, ok := img.f.(interface{ allocate(off, n int64) error }); ok {
			err := a.allocate(off, n)
			if !errors.Is(err, errAllocUnsupported) {
				return err
			}
		}
	}
	img.dataDirty = true
	zeros := make([]byte, min(n, 1<<20))
	for done := int64(0); done < n; {
		m := min(int64(len(zeros)), n-done)
		if _, err := img.f.WriteAt(zeros[:m], off+done); err != nil {
			return err
		}
		done += m
	}
	return nil
}

func validPrealloc(mode string) error {
	switch mode {
	case "", PreallocOff, PreallocMetadata, PreallocFalloc, PreallocFull:
		return nil
	}
	return fmt.Errorf("qcow2: invalid preallocation mode %q", mode)
}
