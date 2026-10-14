package qcow2

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openRW(t *testing.T, path string) *Image {
	t.Helper()
	img, err := OpenFile(path, os.O_RDWR, nil)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// assertClean fails unless both qemu-img check and Check find the image
// clean.
func assertClean(t *testing.T, path string) {
	t.Helper()
	qemuCheck(t, path)
	img, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeImage(t, img)
	res, err := img.Check()
	if err != nil {
		t.Fatal(err)
	}
	if res.Leaks != 0 || res.Corruptions != 0 {
		t.Fatalf("Check: %+v", res)
	}
}

func TestSnapshotLifecycle(t *testing.T) {
	requireQemu(t)
	for _, c := range []struct {
		version, refBits int
		clusterSize      int64
	}{
		{3, 16, 64 << 10},
		{2, 16, 4096},
		{3, 4, 512},
		{3, 64, 64 << 10},
	} {
		t.Run(fmt.Sprintf("v%d-refbits%d-cluster%d", c.version, c.refBits, c.clusterSize), func(t *testing.T) {
			testSnapshotLifecycle(t, c.version, c.refBits, c.clusterSize)
		})
	}
}

func testSnapshotLifecycle(t *testing.T, version, refBits int, clusterSize int64) {
	path := filepath.Join(t.TempDir(), "s.qcow2")
	rng := rand.New(rand.NewPCG(5, uint64(clusterSize)))
	img, err := Create(path, CreateOptions{Size: 4 << 20, Version: version, RefcountBits: refBits, ClusterSize: clusterSize})
	if err != nil {
		t.Fatal(err)
	}
	state := make([]byte, img.Size())
	randomWrites(t, rng, img, state, 40, 64<<10)
	state1 := bytes.Clone(state)
	sn1, err := img.CreateSnapshot("s1")
	if err != nil {
		t.Fatal(err)
	}
	randomWrites(t, rng, img, state, 40, 64<<10)
	if err := img.Discard(1<<20, 1<<20); err != nil {
		t.Fatal(err)
	}
	clear(state[1<<20 : 2<<20])
	state2 := bytes.Clone(state)
	if _, err := img.CreateSnapshot("s2"); err != nil {
		t.Fatal(err)
	}
	if _, err := img.CreateSnapshot("s2"); err == nil {
		t.Fatal("duplicate snapshot name accepted")
	}
	randomWrites(t, rng, img, state, 40, 64<<10)
	closeImage(t, img)

	assertClean(t, path)
	list := run(t, "qemu-img", "snapshot", "-l", path)
	if !strings.Contains(list, "s1") || !strings.Contains(list, "s2") {
		t.Fatalf("qemu-img snapshot -l:\n%s", list)
	}
	assertEqual(t, "active", qemuRaw(t, path), state)
	assertEqual(t, "s1", qemuRaw(t, path, "-l", "snapshot.name=s1"), state1)
	assertEqual(t, "s2", qemuRaw(t, path, "-l", "snapshot.name=s2"), state2)

	img = openRW(t, path)
	infos := img.Snapshots()
	if len(infos) != 2 || infos[0] != sn1 || infos[1].Name != "s2" || infos[1].ID != "2" || infos[1].Size != 4<<20 {
		t.Fatalf("Snapshots() = %+v (created %+v)", infos, sn1)
	}
	if err := img.ApplySnapshot("s1"); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "applied s1", readAll(t, img), state1)
	state = bytes.Clone(state1)
	randomWrites(t, rng, img, state, 40, 64<<10)
	closeImage(t, img)

	assertClean(t, path)
	assertEqual(t, "active after apply", qemuRaw(t, path), state)
	assertEqual(t, "s1 after apply", qemuRaw(t, path, "-l", "snapshot.name=s1"), state1)
	assertEqual(t, "s2 after apply", qemuRaw(t, path, "-l", "snapshot.name=s2"), state2)

	img = openRW(t, path)
	if err := img.DeleteSnapshot("1"); err != nil { // by ID
		t.Fatal(err)
	}
	if err := img.DeleteSnapshot("s1"); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("deleting a deleted snapshot: %v", err)
	}
	closeImage(t, img)
	assertClean(t, path)
	assertEqual(t, "s2 after delete", qemuRaw(t, path, "-l", "snapshot.name=s2"), state2)

	img = openRW(t, path)
	if err := img.DeleteSnapshot("s2"); err != nil {
		t.Fatal(err)
	}
	if n := len(img.Snapshots()); n != 0 {
		t.Fatalf("%d snapshots left", n)
	}
	closeImage(t, img)
	assertClean(t, path)
	assertEqual(t, "active after delete", qemuRaw(t, path), state)

	// The image owns its clusters again, so rewriting them allocates
	// nothing.
	size := fileSize(t, path)
	img = openRW(t, path)
	es, err := img.Map(0, img.Size())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if e.Depth == 0 && e.Data {
			for i := e.Offset; i < e.Offset+e.Length; i++ {
				state[i] = byte(rng.Uint32())
			}
			if _, err := img.WriteAt(state[e.Offset:e.Offset+e.Length], e.Offset); err != nil {
				t.Fatal(err)
			}
		}
	}
	closeImage(t, img)
	if got := fileSize(t, path); got > size {
		t.Fatalf("file grew from %d to %d bytes after the snapshots were deleted", size, got)
	}
	assertClean(t, path)
	assertEqual(t, "final", qemuRaw(t, path), state)
}

// TestSnapshotQemuInterop applies and deletes snapshots QEMU created, with
// compressed and zero clusters, and lets QEMU apply ours.
func TestSnapshotQemuInterop(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "s.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", path, "4M")
	run(t, "qemu-io", "-c", "write -P 1 0 1M", "-c", "write -c -P 2 1M 64k", "-c", "write -c -P 3 1088k 64k",
		"-c", "write -z 2M 128k", path)
	run(t, "qemu-img", "snapshot", "-c", "q1", path)
	q1 := qemuRaw(t, path)
	run(t, "qemu-io", "-c", "write -P 4 512k 1M", "-c", "write -z 64k 64k", path)

	img := openRW(t, path)
	if _, err := img.CreateSnapshot("g1"); err != nil {
		t.Fatal(err)
	}
	g1 := readAll(t, img)
	if err := img.ApplySnapshot("q1"); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "applied q1", readAll(t, img), q1)
	if err := img.DeleteSnapshot("q1"); err != nil {
		t.Fatal(err)
	}
	closeImage(t, img)
	assertClean(t, path)
	assertEqual(t, "active", qemuRaw(t, path), q1)

	run(t, "qemu-img", "snapshot", "-a", "g1", path)
	assertClean(t, path)
	assertEqual(t, "qemu applied g1", qemuRaw(t, path), g1)
	run(t, "qemu-img", "snapshot", "-d", "g1", path)
	assertClean(t, path)
}

func TestSnapshotDifferentSize(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "s.qcow2")
	img, err := Create(path, CreateOptions{Size: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	small := make([]byte, img.Size())
	randomWrites(t, rand.New(rand.NewPCG(1, 2)), img, small, 10, 64<<10)
	if _, err := img.CreateSnapshot("small"); err != nil {
		t.Fatal(err)
	}
	if err := img.Resize(64 << 20); err != nil {
		t.Fatal(err)
	}
	if _, err := img.WriteAt([]byte("tail"), 63<<20); err != nil {
		t.Fatal(err)
	}
	if err := img.ApplySnapshot("small"); err != nil {
		t.Fatal(err)
	}
	if img.Size() != 1<<20 {
		t.Fatalf("size %d after applying a 1 MiB snapshot", img.Size())
	}
	closeImage(t, img)
	assertClean(t, path)
	assertEqual(t, "applied", qemuRaw(t, path), small)

	img = openRW(t, path)
	if err := img.AddBitmap("b", 0, true); err != nil {
		t.Fatal(err)
	}
	if err := img.Resize(2 << 20); err != nil {
		t.Fatal(err)
	}
	if err := img.ApplySnapshot("small"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("applying a snapshot of another size with bitmaps: %v", err)
	}
	closeImage(t, img)
	assertClean(t, path)
}

// TestSnapshotRefcountOverflow checks that a snapshot that would overflow
// a narrow refcount fails without changing the image.
func TestSnapshotRefcountOverflow(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "s.qcow2")
	img, err := Create(path, CreateOptions{Size: 1 << 20, RefcountBits: 2})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, img.Size())
	randomWrites(t, rand.New(rand.NewPCG(3, 3)), img, want, 10, 64<<10)
	for _, name := range []string{"a", "b"} {
		if _, err := img.CreateSnapshot(name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := img.CreateSnapshot("c"); err == nil {
		t.Fatal("snapshot overflowing 2-bit refcounts accepted")
	}
	closeImage(t, img)
	assertClean(t, path)
	assertEqual(t, "active", qemuRaw(t, path), want)
}

func TestSnapshotReadOnly(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "s.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", path, "1M")
	run(t, "qemu-img", "snapshot", "-c", "a", path)
	img, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeImage(t, img)
	if infos := img.Snapshots(); len(infos) != 1 || infos[0].Name != "a" || infos[0].ID != "1" {
		t.Fatalf("Snapshots() = %+v", infos)
	}
	if _, err := img.CreateSnapshot("b"); err != ErrReadOnly {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if err := img.ApplySnapshot("a"); err != ErrReadOnly {
		t.Fatalf("ApplySnapshot: %v", err)
	}
	if err := img.DeleteSnapshot("a"); err != ErrReadOnly {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
}
