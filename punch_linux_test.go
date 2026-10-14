//go:build linux

package qcow2

import (
	"bytes"
	"path/filepath"
	"testing"
)

// punchSlack is how far the released space may fall short of the punched
// range. Punching a hole can split extents, and the file system may then
// allocate blocks for its own extent tree, which count towards st_blocks.
const punchSlack = 64 << 10

func TestPunchHole(t *testing.T) {
	requireQemu(t)
	for _, noPunch := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "p.qcow2")
		img, err := Create(path, CreateOptions{Size: 1 << 20, NoPunchHole: noPunch})
		if err != nil {
			t.Fatal(err)
		}
		const n = 32
		off, err := img.allocClusters(n)
		if err != nil {
			t.Fatal(err)
		}
		data := make([]byte, n*img.clusterSize)
		for i := range data {
			data[i] = byte(i*7 + 1)
		}
		if _, err := img.f.WriteAt(data, off); err != nil {
			t.Fatal(err)
		}
		before := allocated(t, path)
		for i := int64(0); i < n; i++ {
			if err := img.addRefcount(off+i*img.clusterSize, -1); err != nil {
				t.Fatal(err)
			}
		}
		if !noPunch && img.noPunchHole {
			t.Skip("file system does not support hole punching")
		}
		after := allocated(t, path)
		size := fileSize(t, path)
		closeImage(t, img)
		qemuCheck(t, path)
		if size != off+n*img.clusterSize {
			t.Fatalf("file size changed to %d", size)
		}
		released := before - after
		if noPunch && released != 0 {
			t.Fatalf("NoPunchHole: %d bytes released", released)
		}
		if !noPunch && released < int64(len(data))-punchSlack {
			t.Fatalf("released %d bytes, want at least %d", released, int64(len(data))-punchSlack)
		}
	}
}

// TestDiscardReleasesSpace checks that discarded clusters are punched out
// of the file.
func TestDiscardReleasesSpace(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "d.qcow2")
	img, err := Create(path, CreateOptions{Size: 32 << 20})
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{0xa5}, 32<<20)
	if _, err := img.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if err := img.Flush(); err != nil {
		t.Fatal(err)
	}
	full := allocated(t, path)
	if err := img.Discard(4<<20, 24<<20); err != nil {
		t.Fatal(err)
	}
	if img.noPunchHole {
		t.Skip("file system does not support hole punching")
	}
	closeImage(t, img)
	after := allocated(t, path)
	t.Logf("allocated: %d -> %d bytes", full, after)
	if released := full - after; released < 24<<20-punchSlack {
		t.Fatalf("released %d bytes, want at least %d", released, 24<<20-punchSlack)
	}
	qemuCheck(t, path)
	clear(data[4<<20 : 28<<20])
	assertEqual(t, "qemu view", qemuRaw(t, path), data)
}
