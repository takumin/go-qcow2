package qcow2

import (
	"bytes"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

func TestRebase(t *testing.T) {
	requireQemu(t)
	dir := t.TempDir()
	base1 := filepath.Join(dir, "base1.qcow2")
	base2 := filepath.Join(dir, "base2.raw")
	top := filepath.Join(dir, "top.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", base1, "6M")
	run(t, "qemu-io", "-c", "write -P 1 0 6M", base1)
	// The new backing file agrees with the old one in places, is smaller,
	// and has a hole.
	run(t, "qemu-img", "create", "-q", "-f", "raw", base2, "5M")
	run(t, "qemu-io", "-f", "raw", "-c", "write -P 1 0 2M", "-c", "write -P 2 3M 1M", base2)
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", "base1.qcow2", "-o", "cluster_size=4096", top, "8M")
	run(t, "qemu-io", "-c", "write -P 3 1M 1536k", "-c", "write -z 5M 64k", "-c", "write -c -P 4 7M 64k", top)
	want := qemuRaw(t, top)

	img := openRW(t, top)
	if err := img.Rebase("base2.raw", ""); err != nil {
		t.Fatal(err)
	}
	if name, format := img.BackingFile(); name != "base2.raw" || format != "raw" {
		t.Fatalf("backing file %q (%s)", name, format)
	}
	assertEqual(t, "after rebase", readAll(t, img), want)
	closeImage(t, img)
	assertClean(t, top)
	assertEqual(t, "qemu view after rebase", qemuRaw(t, top), want)
	// Only what differs was copied: the first 1 MiB agrees.
	for _, e := range qemuMap(t, top, 0, 1<<20) {
		if e.Depth != 1 {
			t.Fatalf("range %+v copied although both backing files agree", e)
		}
	}

	// Without a backing file at all.
	img = openRW(t, top)
	if err := img.Rebase("", ""); err != nil {
		t.Fatal(err)
	}
	closeImage(t, img)
	assertClean(t, top)
	assertEqual(t, "after flattening", qemuRaw(t, top), want)
	if name := qemuInfo(t, top)["backing-filename"]; name != nil {
		t.Fatalf("backing file %v left", name)
	}
}

func TestCommit(t *testing.T) {
	requireQemu(t)
	for _, format := range []string{"qcow2", "raw"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			base := filepath.Join(dir, "base."+format)
			top := filepath.Join(dir, "top.qcow2")
			run(t, "qemu-img", "create", "-q", "-f", format, base, "4M")
			run(t, "qemu-io", "-f", format, "-c", "write -P 1 0 4M", base)
			img, err := Create(top, CreateOptions{Size: 6 << 20, BackingFile: "base." + format, ClusterSize: 4096})
			if err != nil {
				t.Fatal(err)
			}
			want := readAll(t, img)
			rng := rand.New(rand.NewPCG(9, 9))
			randomWrites(t, rng, img, want, 30, 64<<10)
			if err := img.WriteZeroes(1<<20, 300<<10, false); err != nil {
				t.Fatal(err)
			}
			clear(want[1<<20 : 1<<20+300<<10])
			compressed := bytes.Repeat([]byte("c"), 16<<10)
			if _, err := img.WriteCompressedAt(compressed, 3<<20); err != nil {
				t.Fatal(err)
			}
			copy(want[3<<20:], compressed)
			if err := img.Commit(); err != nil {
				t.Fatal(err)
			}
			// The image still reads the same through the reopened backing
			// file, and keeps working.
			assertEqual(t, "after commit", readAll(t, img), want)
			if _, err := img.WriteAt([]byte("x"), 0); err != nil {
				t.Fatal(err)
			}
			closeImage(t, img)

			if format == "qcow2" {
				assertClean(t, base)
			}
			got := qemuRaw(t, base, "-f", format)
			assertEqual(t, "backing file after commit", got, want)

			img, err = Open(top)
			if err != nil {
				t.Fatal(err)
			}
			if err := img.Commit(); err != ErrReadOnly {
				t.Fatalf("Commit on a read-only image: %v", err)
			}
			closeImage(t, img)
		})
	}
}

func TestCommitWithoutBacking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.qcow2")
	img, err := Create(path, CreateOptions{Size: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer closeImage(t, img)
	if err := img.Commit(); err == nil {
		t.Fatal("Commit without backing file succeeded")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
