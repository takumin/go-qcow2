package qcow2

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"testing"
)

func TestShrink(t *testing.T) {
	requireQemu(t)
	for _, backing := range []bool{false, true} {
		t.Run(fmt.Sprintf("backing=%v", backing), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "s.qcow2")
			// 4 KiB clusters: an L2 table maps 2 MiB, so the shrink crosses
			// several of them, and freed clusters are whole file system
			// blocks.
			opts := CreateOptions{Size: 16 << 20, ClusterSize: 4096}
			want := make([]byte, opts.Size)
			if backing {
				base := filepath.Join(dir, "base.qcow2")
				run(t, "qemu-img", "create", "-q", "-f", "qcow2", base, "16M")
				run(t, "qemu-io", "-c", "write -P 0x77 0 16M", base)
				opts.BackingFile = "base.qcow2"
				copy(want, bytes.Repeat([]byte{0x77}, 16<<20))
			}
			img, err := Create(path, opts)
			if err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewPCG(14, 14))
			randomWrites(t, rng, img, want, 60, 512<<10)
			if _, err := img.WriteCompressedAt(bytes.Repeat([]byte("q"), 4096), 12<<20); err != nil {
				t.Fatal(err)
			}
			copy(want[12<<20:], bytes.Repeat([]byte("q"), 4096))
			if _, err := img.CreateSnapshot("s"); err != nil {
				t.Fatal(err)
			}
			snap := bytes.Clone(want)
			randomWrites(t, rng, img, want, 30, 512<<10)
			if err := img.AddBitmap("b", 4096, true); err != nil {
				t.Fatal(err)
			}
			// Not a multiple of the cluster size: rounded up to 512 bytes
			// and the tail of the last cluster zeroed.
			const small = 5<<20 + 300
			if err := img.Shrink(small); err != nil {
				t.Fatal(err)
			}
			if img.Size() != alignUp(small, 512) {
				t.Fatalf("size %d", img.Size())
			}
			want = want[:alignUp(small, 512)]
			assertEqual(t, "after shrink", readAll(t, img), want)
			if err := img.Resize(1 << 20); err == nil {
				t.Fatal("Resize shrank the image")
			}
			closeImage(t, img)
			assertClean(t, path)
			assertEqual(t, "qemu view", qemuRaw(t, path), want)
			// qemu-img convert -l stops at the current virtual size.
			assertEqual(t, "snapshot", qemuRaw(t, path, "-l", "snapshot.name=s"), snap[:len(want)])
			copyPath := filepath.Join(dir, "copy.qcow2")
			run(t, "cp", path, copyPath)
			// QEMU applies no snapshots to images with enabled bitmaps.
			run(t, "qemu-img", "bitmap", "--remove", copyPath, "b")
			run(t, "qemu-img", "snapshot", "-a", "s", copyPath)
			assertEqual(t, "snapshot applied by qemu", qemuRaw(t, copyPath), snap)

			// Growing again shows zeros.
			img = openRW(t, path)
			if err := img.Resize(16 << 20); err != nil {
				t.Fatal(err)
			}
			want = append(want, make([]byte, 16<<20-len(want))...)
			closeImage(t, img)
			assertClean(t, path)
			assertEqual(t, "grown again", qemuRaw(t, path), want)

			// Without the snapshot the clusters are freed. Clusters still in
			// use may lie near the end of the file, so it is the space used
			// that shrinks.
			usedBefore, usageKnown := diskUsage(t, path)
			img = openRW(t, path)
			if err := img.DeleteSnapshot("s"); err != nil {
				t.Fatal(err)
			}
			if err := img.Shrink(1 << 20); err != nil {
				t.Fatal(err)
			}
			closeImage(t, img)
			assertClean(t, path)
			assertEqual(t, "shrunk to 1 MiB", qemuRaw(t, path), want[:1<<20])
			if used, _ := diskUsage(t, path); usageKnown && used > usedBefore/4 {
				t.Fatalf("file uses %d bytes after shrinking to 1 MiB, %d before", used, usedBefore)
			}
		})
	}
}
