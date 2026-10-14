package qcow2

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepairDirty opens an image QEMU left dirty with lazy refcounts.
func TestRepairDirty(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "d.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", "-o", "lazy_refcounts=on,cluster_size=4096", path, "4M")
	// Kill qemu-io after the writes, before it stores the refcounts.
	err := exec.Command("qemu-io", "-c", "write -P 1 0 1M", "-c", "write -P 2 2M 300k", "-c", "sigraise 9", path).Run()
	if err == nil {
		t.Fatal("qemu-io survived SIGKILL")
	}
	if out, err := exec.Command("qemu-img", "check", path).CombinedOutput(); err == nil {
		t.Fatalf("qemu-img check found the dirty image clean:\n%s", out)
	}
	want := qemuRaw(t, path)

	img, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "read-only", readAll(t, img), want)
	closeImage(t, img)

	img = openRW(t, path)
	if img.hdr.IncompatibleFeatures&incompatDirty != 0 {
		t.Fatal("dirty flag still set after a writable open")
	}
	if _, err := img.WriteAt([]byte("after"), 3<<20); err != nil {
		t.Fatal(err)
	}
	copy(want[3<<20:], "after")
	closeImage(t, img)
	assertClean(t, path)
	assertEqual(t, "after repair", qemuRaw(t, path), want)
}

// corruptImage creates an image whose refcounts and COPIED flags are
// wrong in every fixable way and returns its guest contents.
func corruptImage(t *testing.T, path string) []byte {
	t.Helper()
	// Small clusters, so that there are several refcount blocks.
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", "-o", "cluster_size=512", path, "4M")
	run(t, "qemu-io", "-c", "write -P 1 0 1M", "-c", "write -c -P 2 1M 64k", path)
	run(t, "qemu-img", "snapshot", "-c", "s", path)
	run(t, "qemu-io", "-c", "write -P 3 512k 64k", path)
	want := qemuRaw(t, path)

	img := openRW(t, path)
	// Dropping a refcount to zero must not punch the data out.
	img.noPunchHole = true
	e0, err := img.l2Entry(0)
	if err != nil {
		t.Fatal(err)
	}
	e1, err := img.l2Entry(512 << 10)
	if err != nil {
		t.Fatal(err)
	}
	shared, owned := int64(e0&l2OffsetMask), int64(e1&l2OffsetMask)
	// Too low: a cluster shared with the snapshot drops to 1 (and the
	// COPIED flags no longer match), a cluster in use drops to 0. Too
	// high: the header.
	for _, c := range []struct {
		off   int64
		delta int64
	}{{shared, -1}, {owned, -1}, {0, 3}} {
		if err := img.addRefcount(c.off, c.delta); err != nil {
			t.Fatal(err)
		}
	}
	// A refcount block disappears from the refcount table.
	last := len(img.refTable) - 1
	for img.refTable[last] == 0 {
		last--
	}
	if last == 0 {
		t.Fatal("only one refcount block")
	}
	img.refTable[last] = 0
	img.refTableDirty[int64(last)] = struct{}{}
	closeUntruncated(t, img)
	return want
}

// closeUntruncated writes the changes of img and closes it without
// truncating the file, which relies on the refcounts.
func closeUntruncated(t *testing.T, img *Image) {
	t.Helper()
	if err := img.flushAll(); err != nil {
		t.Fatal(err)
	}
	img.closed = true
	if err := img.f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRepair(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "c.qcow2")
	want := corruptImage(t, path)
	if out, err := exec.Command("qemu-img", "check", path).CombinedOutput(); err == nil || !strings.Contains(string(out), "ERROR") {
		t.Fatalf("qemu-img check did not report corruption: %v\n%s", err, out)
	}

	img := openRW(t, path)
	res, err := img.Repair()
	if err != nil {
		t.Fatal(err)
	}
	if res.Corruptions == 0 || res.Leaks == 0 {
		t.Fatalf("Repair found %+v", res)
	}
	closeImage(t, img)
	assertClean(t, path)
	assertEqual(t, "after repair", qemuRaw(t, path), want)
	run(t, "qemu-img", "snapshot", "-a", "s", path)
	assertClean(t, path)
}

func TestRepairCorruptFlag(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "c.qcow2")
	want := corruptImage(t, path)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var b [8]byte
	if _, err := f.ReadAt(b[:], offIncompatibleFeatures); err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint64(b[:], binary.BigEndian.Uint64(b[:])|incompatCorrupt)
	if _, err := f.WriteAt(b[:], offIncompatibleFeatures); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := OpenFile(path, os.O_RDWR, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("opening a corrupt image writable: %v", err)
	}
	img, err := OpenFile(path, os.O_RDWR, &Options{Repair: true})
	if err != nil {
		t.Fatal(err)
	}
	closeImage(t, img)
	assertClean(t, path)
	assertEqual(t, "after repair", qemuRaw(t, path), want)
}

func TestRepairUnfixable(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "u.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", path, "4M")
	run(t, "qemu-io", "-c", "write -P 1 0 1M", path)
	img := openRW(t, path)
	// Point an L2 entry at an unaligned offset.
	l2 := int64(img.l1[0] & l1OffsetMask)
	t2, err := img.loadL2(l2)
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint64(t2, binary.BigEndian.Uint64(t2)+512)
	img.l2Cache.markDirty(l2, t2)
	closeImage(t, img)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	img = openRW(t, path)
	if _, err := img.Repair(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Repair of broken metadata: %v", err)
	}
	closeImage(t, img)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "file after failed repair", after, before)
}
