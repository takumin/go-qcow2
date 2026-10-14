package qcow2

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	qcowMagic = 0x514649fb

	v2HeaderLength = 72
	v3HeaderLength = 112 // up to and including compression_type + padding

	// Header field offsets (shared by v2 and v3).
	offSize                 = 24
	offL1Size               = 36
	offL1TableOffset        = 40
	offRefcountTableOffset  = 48
	offIncompatibleFeatures = 72
	offAutoclearFeatures    = 88

	extEnd                = 0x00000000
	extBackingFormat      = 0xe2792aca
	extFeatureNameTable   = 0x6803f857
	extBitmaps            = 0x23852875
	extFullDiskEncryption = 0x0537be77
	extExternalDataFile   = 0x44415441

	incompatDirty           = 1 << 0
	incompatCorrupt         = 1 << 1
	incompatExternalData    = 1 << 2
	incompatCompressionType = 1 << 3
	incompatExtendedL2      = 1 << 4

	compatLazyRefcounts = 1 << 0

	compressionZlib = 0
	compressionZstd = 1

	minClusterBits = 9
	maxClusterBits = 21

	maxBackingFileName = 1023
)

type headerExtension struct {
	Type uint32
	Data []byte
}

// header is the in-memory representation of the qcow2 image header
// (cluster 0): the fixed fields, the header extensions and the backing
// file name.
type header struct {
	Version               uint32
	BackingFileOffset     uint64
	BackingFileSize       uint32
	ClusterBits           uint32
	Size                  uint64
	CryptMethod           uint32
	L1Size                uint32
	L1TableOffset         uint64
	RefcountTableOffset   uint64
	RefcountTableClusters uint32
	NbSnapshots           uint32
	SnapshotsOffset       uint64

	// Version 3 only.
	IncompatibleFeatures uint64
	CompatibleFeatures   uint64
	AutoclearFeatures    uint64
	RefcountOrder        uint32
	HeaderLength         uint32
	CompressionType      uint8

	// raw keeps the original fixed header bytes so that fields unknown to
	// this package survive a header rewrite.
	raw []byte

	// extensions holds every extension except the backing format one,
	// which is kept in backingFormat.
	extensions    []headerExtension
	backingFile   string
	backingFormat string
}

// parseHeader decodes cluster 0 of an image. buf must contain at least the
// whole fixed header; extensions and the backing file name are read from
// it when present.
func parseHeader(buf []byte) (*header, error) {
	if len(buf) < v2HeaderLength {
		return nil, ErrNotQcow2
	}
	be := binary.BigEndian
	if be.Uint32(buf[0:]) != qcowMagic {
		return nil, ErrNotQcow2
	}
	h := &header{
		Version:               be.Uint32(buf[4:]),
		BackingFileOffset:     be.Uint64(buf[8:]),
		BackingFileSize:       be.Uint32(buf[16:]),
		ClusterBits:           be.Uint32(buf[20:]),
		Size:                  be.Uint64(buf[24:]),
		CryptMethod:           be.Uint32(buf[32:]),
		L1Size:                be.Uint32(buf[36:]),
		L1TableOffset:         be.Uint64(buf[40:]),
		RefcountTableOffset:   be.Uint64(buf[48:]),
		RefcountTableClusters: be.Uint32(buf[56:]),
		NbSnapshots:           be.Uint32(buf[60:]),
		SnapshotsOffset:       be.Uint64(buf[64:]),
		RefcountOrder:         4,
		HeaderLength:          v2HeaderLength,
	}
	switch h.Version {
	case 2:
	case 3:
		if len(buf) < 104 {
			return nil, fmt.Errorf("%w: truncated v3 header", ErrCorrupt)
		}
		h.IncompatibleFeatures = be.Uint64(buf[72:])
		h.CompatibleFeatures = be.Uint64(buf[80:])
		h.AutoclearFeatures = be.Uint64(buf[88:])
		h.RefcountOrder = be.Uint32(buf[96:])
		h.HeaderLength = be.Uint32(buf[100:])
		if h.HeaderLength < 104 || h.HeaderLength%8 != 0 {
			return nil, fmt.Errorf("%w: invalid header length %d", ErrCorrupt, h.HeaderLength)
		}
		if int(h.HeaderLength) > len(buf) {
			return nil, fmt.Errorf("%w: header length %d exceeds cluster", ErrCorrupt, h.HeaderLength)
		}
		if h.HeaderLength > 104 {
			h.CompressionType = buf[104]
		}
	default:
		return nil, fmt.Errorf("%w: version %d", ErrUnsupported, h.Version)
	}
	if h.ClusterBits < minClusterBits || h.ClusterBits > maxClusterBits {
		return nil, fmt.Errorf("%w: cluster bits %d", ErrUnsupported, h.ClusterBits)
	}
	if h.RefcountOrder > 6 {
		return nil, fmt.Errorf("%w: refcount order %d", ErrCorrupt, h.RefcountOrder)
	}
	h.raw = append([]byte(nil), buf[:h.HeaderLength]...)

	// Header extensions follow the fixed header and end with a zero type.
	// Images without an end marker inside the buffer are tolerated (qemu
	// does too, as long as nothing overlaps).
	off := int(h.HeaderLength)
	for off+8 <= len(buf) {
		typ := be.Uint32(buf[off:])
		l := int(be.Uint32(buf[off+4:]))
		off += 8
		if typ == extEnd {
			break
		}
		if off+l > len(buf) {
			return nil, fmt.Errorf("%w: header extension 0x%08x overflows cluster", ErrCorrupt, typ)
		}
		data := append([]byte(nil), buf[off:off+l]...)
		off += align(l, 8)
		if typ == extBackingFormat {
			h.backingFormat = string(data)
			continue
		}
		h.extensions = append(h.extensions, headerExtension{Type: typ, Data: data})
	}

	if h.BackingFileOffset != 0 {
		if h.BackingFileSize > maxBackingFileName {
			return nil, fmt.Errorf("%w: backing file name too long", ErrCorrupt)
		}
		end := h.BackingFileOffset + uint64(h.BackingFileSize)
		if end > uint64(len(buf)) {
			return nil, fmt.Errorf("%w: backing file name outside cluster 0", ErrCorrupt)
		}
		h.backingFile = string(buf[h.BackingFileOffset:end])
	}
	return h, nil
}

func (h *header) hasExtension(typ uint32) bool {
	for _, e := range h.extensions {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// marshal encodes the full header (fixed part, extensions and backing file
// name). The result must fit into the first cluster.
func (h *header) marshal(clusterSize int) ([]byte, error) {
	hl := int(h.HeaderLength)
	if h.Version == 2 {
		hl = v2HeaderLength
	}

	var ext []byte
	putExt := func(typ uint32, data []byte) {
		var hdr [8]byte
		binary.BigEndian.PutUint32(hdr[0:], typ)
		binary.BigEndian.PutUint32(hdr[4:], uint32(len(data)))
		ext = append(ext, hdr[:]...)
		ext = append(ext, data...)
		ext = append(ext, make([]byte, align(len(data), 8)-len(data))...)
	}
	if h.backingFormat != "" {
		putExt(extBackingFormat, []byte(h.backingFormat))
	}
	for _, e := range h.extensions {
		putExt(e.Type, e.Data)
	}
	putExt(extEnd, nil)

	if h.backingFile != "" {
		if len(h.backingFile) > maxBackingFileName {
			return nil, errors.New("qcow2: backing file name too long")
		}
		h.BackingFileOffset = uint64(hl + len(ext))
		h.BackingFileSize = uint32(len(h.backingFile))
	} else {
		h.BackingFileOffset = 0
		h.BackingFileSize = 0
	}

	buf := make([]byte, hl, hl+len(ext)+len(h.backingFile))
	copy(buf, h.raw)
	be := binary.BigEndian
	be.PutUint32(buf[0:], qcowMagic)
	be.PutUint32(buf[4:], h.Version)
	be.PutUint64(buf[8:], h.BackingFileOffset)
	be.PutUint32(buf[16:], h.BackingFileSize)
	be.PutUint32(buf[20:], h.ClusterBits)
	be.PutUint64(buf[24:], h.Size)
	be.PutUint32(buf[32:], h.CryptMethod)
	be.PutUint32(buf[36:], h.L1Size)
	be.PutUint64(buf[40:], h.L1TableOffset)
	be.PutUint64(buf[48:], h.RefcountTableOffset)
	be.PutUint32(buf[56:], h.RefcountTableClusters)
	be.PutUint32(buf[60:], h.NbSnapshots)
	be.PutUint64(buf[64:], h.SnapshotsOffset)
	if h.Version >= 3 {
		be.PutUint64(buf[72:], h.IncompatibleFeatures)
		be.PutUint64(buf[80:], h.CompatibleFeatures)
		be.PutUint64(buf[88:], h.AutoclearFeatures)
		be.PutUint32(buf[96:], h.RefcountOrder)
		be.PutUint32(buf[100:], uint32(hl))
		if hl > 104 {
			buf[104] = h.CompressionType
		}
	}
	buf = append(buf, ext...)
	buf = append(buf, h.backingFile...)
	if len(buf) > clusterSize {
		return nil, fmt.Errorf("qcow2: header (%d bytes) does not fit into the first cluster (%d bytes)", len(buf), clusterSize)
	}
	h.raw = append(h.raw[:0], buf[:hl]...)
	return buf, nil
}

func align(n, a int) int {
	return (n + a - 1) / a * a
}
