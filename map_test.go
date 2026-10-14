package qcow2

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// qemuMap returns the layout of an image as reported by qemu-img map,
// converted to MapEntry.
func qemuMap(t *testing.T, path string, off, length int64) []MapEntry {
	t.Helper()
	out := run(t, "qemu-img", "map", "--output=json",
		"--start-offset", strconv.FormatInt(off, 10), "--max-length", strconv.FormatInt(length, 10), path)
	var raw []struct {
		Start, Length, Depth int64
		Present, Zero, Data  bool
		Compressed           bool
		Offset               *int64
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("parsing qemu-img map output: %v\n%s", err, out)
	}
	var es []MapEntry
	for _, r := range raw {
		e := MapEntry{
			Offset: r.Start, Length: r.Length, Depth: int(r.Depth),
			Present: r.Present, Zero: r.Zero, Data: r.Data, Compressed: r.Compressed, HostOffset: -1,
		}
		if r.Offset != nil {
			e.HostOffset = *r.Offset
		}
		es = append(es, e)
	}
	return es
}

func assertMap(t *testing.T, path string, off, length int64) {
	t.Helper()
	img, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeImage(t, img)
	got, err := img.Map(off, length)
	if err != nil {
		t.Fatal(err)
	}
	want := qemuMap(t, path, off, length)
	// Whether holes of the image file inside data clusters are reported
	// depends on a heuristic in QEMU; compare without them. TestMapSparseCopy
	// checks that the holes reported are right.
	if fmt.Sprint(withoutHostHoles(got)) != fmt.Sprint(withoutHostHoles(want)) {
		t.Fatalf("Map(%d, %d) of %s:\n got  %v\n want %v", off, length, filepath.Base(path), got, want)
	}
}

// withoutHostHoles drops the zero flag of data entries and merges them
// again.
func withoutHostHoles(es []MapEntry) []MapEntry {
	var out []MapEntry
	for _, e := range es {
		if e.Data && !e.Compressed {
			e.Zero = false
		}
		out = appendMapEntry(out, e)
	}
	return out
}

func TestMap(t *testing.T) {
	requireQemu(t)
	dir := t.TempDir()
	base := filepath.Join(dir, "base.raw")
	mid := filepath.Join(dir, "mid.qcow2")
	top := filepath.Join(dir, "top.qcow2")

	// A raw base with a hole, smaller than the overlays and not cluster
	// aligned.
	run(t, "qemu-img", "create", "-f", "raw", base, "1000000")
	run(t, "qemu-io", "-f", "raw", "-c", "write -P 1 0 64k", "-c", "write -P 1 700k 10k", base)

	run(t, "qemu-img", "create", "-f", "qcow2", "-F", "raw", "-b", base, mid, "3M")
	run(t, "qemu-io",
		"-c", "write -P 2 64k 64k", // data
		"-c", "write -z 128k 64k", // unallocated zero cluster
		"-c", "write -P 3 256k 128k",
		"-c", "write -z 256k 64k", // preallocated zero cluster
		"-c", "write -P 6 1100k 64k", // data past the end of the base
		mid)

	run(t, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", mid, top, "4000000")
	run(t, "qemu-io",
		"-c", "write -P 4 384k 64k",
		"-c", "write -c -P 5 512k 64k", // compressed
		"-c", "write -c -P 5 576k 64k",
		"-c", "write -P 7 3900000 20000", // partial last cluster
		top)

	for _, path := range []string{mid, top} {
		img, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		size := img.Size()
		closeImage(t, img)
		assertMap(t, path, 0, size)
		assertMap(t, path, 100000, 1200000)
		assertMap(t, path, 3*65536+512, 512)
		assertMap(t, path, size-4096, 4096)
	}

	// Without a backing file.
	img, err := Open(top)
	if err != nil {
		t.Fatal(err)
	}
	closeImage(t, img)
	img, err = OpenFile(top, os.O_RDONLY, &Options{NoBacking: true})
	if err != nil {
		t.Fatal(err)
	}
	defer closeImage(t, img)
	es, err := img.Map(0, img.Size())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if e.Depth != 0 {
			t.Fatalf("entry %v reaches into the backing file", e)
		}
	}
	if _, err := img.Map(0, img.Size()+1); err != ErrOutOfRange {
		t.Fatalf("Map past the end: %v, want ErrOutOfRange", err)
	}
}

// TestMapSparseCopy copies an image by reading only the ranges Map does
// not report as zeros.
func TestMapSparseCopy(t *testing.T) {
	requireQemu(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.qcow2")
	run(t, "qemu-img", "create", "-f", "qcow2", path, "8M")
	run(t, "qemu-io", "-c", "write -P 9 1M 300k", "-c", "write -z 2M 1M", "-c", "write -P 8 5000k 1k",
		"-c", "write -P 7 6M 128k", "-c", "write -z 6200k 4k", path)
	img, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeImage(t, img)
	es, err := img.Map(0, img.Size())
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, img.Size())
	for _, e := range es {
		if e.Zero {
			continue
		}
		if _, err := img.ReadAt(got[e.Offset:e.Offset+e.Length], e.Offset); err != nil && err != io.EOF {
			t.Fatal(err)
		}
	}
	assertEqual(t, "sparse copy", got, qemuRaw(t, path))
}
