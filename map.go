package qcow2

import (
	"errors"
	"io"
	"os"
)

// MapEntry describes how a range of the guest disk is stored, like an
// entry of "qemu-img map --output=json".
type MapEntry struct {
	Offset, Length int64
	// Depth is the layer of the backing chain the range comes from: 0 for
	// the image itself, 1 for its backing file and so on. For a range no
	// layer provides, it is the last layer consulted.
	Depth int
	// Present is set when some layer provides the range: it is allocated
	// there, is a zero cluster or lies within a raw backing file.
	Present bool
	// Zero is set when the range reads as zeros.
	Zero bool
	// Data is set when the range is stored as data; zero clusters, holes
	// and ranges no layer provides have no data.
	Data bool
	// Compressed is set for compressed clusters.
	Compressed bool
	// HostOffset is the offset of the range in the file of layer Depth,
	// or -1 when it has none (compressed and unallocated zero clusters,
	// ranges no layer provides, backing files of unknown type).
	HostOffset int64
}

// mapper is implemented by backing files that can describe their layout.
type mapper interface {
	mapRange(off, length int64) ([]MapEntry, error)
}

// Map describes how the guest range [off, off+length) is stored across
// the backing chain, merging adjacent ranges stored alike. It can be used
// to copy an image sparsely: ranges with Zero set need not be read.
func (img *Image) Map(off, length int64) ([]MapEntry, error) {
	img.mu.Lock()
	defer img.mu.Unlock()
	if img.closed {
		return nil, ErrClosed
	}
	if off < 0 || length < 0 || off > img.size || length > img.size-off {
		return nil, ErrOutOfRange
	}
	return img.mapLocked(off, length)
}

func (img *Image) mapRange(off, length int64) ([]MapEntry, error) {
	return img.Map(off, length)
}

func (img *Image) mapLocked(off, length int64) ([]MapEntry, error) {
	var entries []MapEntry
	cs := img.clusterSize
	end := off + length
	// [unalloc, g) is a run of clusters not allocated in this image,
	// resolved through the backing file in one go.
	unalloc := int64(-1)
	holes := hostExtents{img: img}
	flushUnalloc := func(g int64) error {
		if unalloc < 0 {
			return nil
		}
		es, err := img.mapBacking(unalloc, g-unalloc)
		if err != nil {
			return err
		}
		for _, e := range es {
			entries = appendMapEntry(entries, e)
		}
		unalloc = -1
		return nil
	}
	for g := off; g < end; {
		n := min((g&^(cs-1))+cs, end) - g
		i := img.l1Index(g)
		if i >= int64(len(img.l1)) || img.l1[i]&l1OffsetMask == 0 {
			// No L2 table: the rest of its range is unallocated.
			n = min((i+1)<<(img.clusterBits+img.l2Bits), end) - g
		}
		entry, err := img.l2Entry(g)
		if err != nil {
			return nil, err
		}
		host := int64(entry & l2OffsetMask)
		in := g & (cs - 1)
		var e MapEntry
		switch {
		case entry&flagCompressed != 0:
			e = MapEntry{Present: true, Data: true, Compressed: true, HostOffset: -1}
		case img.hdr.Version >= 3 && entry&flagZero != 0:
			e = MapEntry{Present: true, Zero: true, HostOffset: -1}
			if host != 0 {
				e.HostOffset = host + in
			}
		case host != 0:
			if err := flushUnalloc(g); err != nil {
				return nil, err
			}
			for h, hend := host+in, host+in+n; h < hend; {
				data, next, err := holes.extent(h)
				if err != nil {
					return nil, err
				}
				next = min(next, hend)
				entries = appendMapEntry(entries, MapEntry{
					Offset: g + h - host - in, Length: next - h,
					Present: true, Zero: !data, Data: true, HostOffset: h,
				})
				h = next
			}
			g += n
			continue
		default:
			if unalloc < 0 {
				unalloc = g
			}
			g += n
			continue
		}
		if err := flushUnalloc(g); err != nil {
			return nil, err
		}
		e.Offset, e.Length = g, n
		entries = appendMapEntry(entries, e)
		g += n
	}
	if err := flushUnalloc(end); err != nil {
		return nil, err
	}
	return entries, nil
}

// hostExtents tells data from holes in the image file, remembering the
// last region found so that a run of clusters costs few system calls.
type hostExtents struct {
	img         *Image
	start, next int64
	data        bool
	size        int64
}

func (h *hostExtents) extent(off int64) (bool, int64, error) {
	if off >= h.start && off < h.next {
		return h.data, h.next, nil
	}
	fs, ok := h.img.f.(fileStorage)
	if !ok {
		return true, off + h.img.clusterSize, nil
	}
	if h.size == 0 {
		size, err := fs.size()
		if err != nil {
			return false, 0, err
		}
		h.size = size
	}
	data, next, err := fileExtent(fs.File, h.size, off)
	if err != nil {
		return false, 0, err
	}
	h.start, h.next, h.data = off, next, data
	return data, next, nil
}

// mapBacking describes a range not allocated in the image.
func (img *Image) mapBacking(off, length int64) ([]MapEntry, error) {
	absent := func(off, length int64) MapEntry {
		return MapEntry{Offset: off, Length: length, Zero: true, HostOffset: -1}
	}
	if img.backing == nil {
		return []MapEntry{absent(off, length)}, nil
	}
	bsize := img.backing.Size()
	n := max(0, min(length, bsize-off))
	var entries []MapEntry
	if n > 0 {
		var es []MapEntry
		if m, ok := img.backing.(mapper); ok {
			var err error
			if es, err = m.mapRange(off, n); err != nil {
				return nil, err
			}
		} else {
			es = []MapEntry{{Offset: off, Length: n, Present: true, Data: true, HostOffset: -1}}
		}
		for _, e := range es {
			e.Depth++
			entries = append(entries, e)
		}
	}
	if n < length {
		entries = append(entries, absent(off+n, length-n))
	}
	return entries, nil
}

// appendMapEntry appends e, merging it into the last entry when both are
// stored alike and contiguously.
func appendMapEntry(entries []MapEntry, e MapEntry) []MapEntry {
	if k := len(entries); k > 0 {
		last := &entries[k-1]
		if last.Offset+last.Length == e.Offset && last.Depth == e.Depth &&
			last.Present == e.Present && last.Zero == e.Zero && last.Data == e.Data &&
			last.Compressed == e.Compressed &&
			(last.HostOffset < 0 && e.HostOffset < 0 || last.HostOffset >= 0 && last.HostOffset+last.Length == e.HostOffset) {
			last.Length += e.Length
			return entries
		}
	}
	return append(entries, e)
}

// mapRange reports the data and holes of a raw image, as far as the file
// system tells them apart.
func (r *Raw) mapRange(off, length int64) ([]MapEntry, error) {
	var entries []MapEntry
	end := off + length
	for g := off; g < end; {
		data, next, err := fileExtent(r.f, r.size, g)
		if err != nil {
			return nil, err
		}
		next = min(next, end)
		entries = appendMapEntry(entries, MapEntry{
			Offset: g, Length: next - g, Present: true, Zero: !data, Data: data, HostOffset: g,
		})
		g = next
	}
	return entries, nil
}

// extent reports whether off lies in data or in a hole of f, whose size is
// size, and where that region ends. Without file system support
// everything is data.
func fileExtent(f *os.File, size, off int64) (data bool, next int64, err error) {
	dataOff, err := seekData(f, off)
	switch {
	case errors.Is(err, errSeekUnsupported):
		return true, max(size, off+1), nil
	case errors.Is(err, io.EOF):
		// No data after off.
		return false, max(size, off+1), nil
	case err != nil:
		return false, 0, err
	case dataOff > off:
		return false, dataOff, nil
	}
	holeOff, err := seekHole(f, off)
	if errors.Is(err, errSeekUnsupported) {
		return true, max(size, off+1), nil
	}
	if err != nil {
		return false, 0, err
	}
	return true, max(holeOff, off+1), nil
}
