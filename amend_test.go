package qcow2

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"testing"
)

func TestAmendRefcountBits(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "a.qcow2")
	img, err := Create(path, CreateOptions{Size: 16 << 20, ClusterSize: 512})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, img.Size())
	rng := rand.New(rand.NewPCG(12, 12))
	randomWrites(t, rng, img, want, 50, 128<<10)
	if _, err := img.CreateSnapshot("s"); err != nil {
		t.Fatal(err)
	}
	snap := bytes.Clone(want)
	randomWrites(t, rng, img, want, 20, 128<<10)
	closeImage(t, img)

	for _, rb := range []int{64, 2, 1, 8, 16} {
		img = openRW(t, path)
		err := img.Amend(AmendOptions{RefcountBits: rb})
		if rb == 1 {
			// The snapshot shares clusters: 2 references.
			if err == nil {
				t.Fatal("1-bit refcounts accepted with shared clusters")
			}
			closeImage(t, img)
			continue
		}
		if err != nil {
			t.Fatalf("refcount bits %d: %v", rb, err)
		}
		randomWrites(t, rng, img, want, 10, 64<<10)
		closeImage(t, img)
		assertClean(t, path)
		if got := fmt.Sprint(qemuInfo(t, path)["format-specific"]); !bytes.Contains([]byte(got), []byte(fmt.Sprintf("refcount-bits:%d", rb))) {
			t.Fatalf("qemu-img info after amending to %d bits: %s", rb, got)
		}
		assertEqual(t, fmt.Sprintf("active with %d bits", rb), qemuRaw(t, path), want)
		assertEqual(t, fmt.Sprintf("snapshot with %d bits", rb), qemuRaw(t, path, "-l", "snapshot.name=s"), snap)
	}
}

func TestAmendVersion(t *testing.T) {
	requireQemu(t)
	for _, backing := range []bool{false, true} {
		t.Run(fmt.Sprintf("backing=%v", backing), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "a.qcow2")
			opts := CreateOptions{Size: 8 << 20, ClusterSize: 4096, RefcountBits: 32}
			want := make([]byte, opts.Size)
			if backing {
				base := filepath.Join(dir, "base.qcow2")
				run(t, "qemu-img", "create", "-q", "-f", "qcow2", base, "8M")
				run(t, "qemu-io", "-c", "write -P 0x5a 0 8M", base)
				opts.BackingFile = "base.qcow2"
				copy(want, bytes.Repeat([]byte{0x5a}, 8<<20))
			}
			img, err := Create(path, opts)
			if err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewPCG(13, 13))
			randomWrites(t, rng, img, want, 40, 128<<10)
			// Zero clusters of every kind, some shared with a snapshot.
			if err := img.WriteZeroes(1<<20, 1<<20, false); err != nil {
				t.Fatal(err)
			}
			if err := img.WriteZeroes(3<<20, 1<<20, true); err != nil {
				t.Fatal(err)
			}
			clear(want[1<<20 : 2<<20])
			clear(want[3<<20 : 4<<20])
			if _, err := img.CreateSnapshot("s"); err != nil {
				t.Fatal(err)
			}
			snap := bytes.Clone(want)
			if err := img.WriteZeroes(5<<20, 1<<20, false); err != nil {
				t.Fatal(err)
			}
			clear(want[5<<20 : 6<<20])
			if err := img.AddBitmap("b", 0, true); err != nil {
				t.Fatal(err)
			}
			if err := img.Amend(AmendOptions{Version: 2}); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("downgrade with bitmaps: %v", err)
			}
			if err := img.RemoveBitmap("b"); err != nil {
				t.Fatal(err)
			}
			if err := img.Amend(AmendOptions{Version: 2}); err != nil {
				t.Fatal(err)
			}
			if img.Version() != 2 {
				t.Fatalf("version %d after downgrade", img.Version())
			}
			assertEqual(t, "after downgrade", readAll(t, img), want)
			closeImage(t, img)
			assertClean(t, path)
			if compat := fmt.Sprint(qemuInfo(t, path)["format-specific"]); !bytes.Contains([]byte(compat), []byte("compat:0.10")) {
				t.Fatalf("qemu-img info after downgrade: %s", compat)
			}
			assertEqual(t, "v2 active", qemuRaw(t, path), want)
			assertEqual(t, "v2 snapshot", qemuRaw(t, path, "-l", "snapshot.name=s"), snap)

			// And back, with lazy refcounts.
			img = openRW(t, path)
			lazy := true
			if err := img.Amend(AmendOptions{Version: 3, LazyRefcounts: &lazy}); err != nil {
				t.Fatal(err)
			}
			randomWrites(t, rng, img, want, 10, 64<<10)
			closeImage(t, img)
			assertClean(t, path)
			info := fmt.Sprint(qemuInfo(t, path)["format-specific"])
			for _, s := range []string{"compat:1.1", "lazy-refcounts:true"} {
				if !bytes.Contains([]byte(info), []byte(s)) {
					t.Fatalf("qemu-img info after upgrade lacks %s: %s", s, info)
				}
			}
			assertEqual(t, "v3 active", qemuRaw(t, path), want)
			assertEqual(t, "v3 snapshot", qemuRaw(t, path, "-l", "snapshot.name=s"), snap)
		})
	}
}

// TestAmendUpgradeQemuV2 upgrades a version 2 image with a snapshot QEMU
// created, whose snapshot entry lacks the virtual size.
func TestAmendUpgradeQemuV2(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "v2.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", "-o", "compat=0.10", path, "4M")
	run(t, "qemu-io", "-c", "write -P 7 0 1M", path)
	run(t, "qemu-img", "snapshot", "-c", "s", path)
	run(t, "qemu-io", "-c", "write -P 8 0 512k", path)
	want, snap := qemuRaw(t, path), qemuRaw(t, path, "-l", "snapshot.name=s")
	img := openRW(t, path)
	if err := img.Amend(AmendOptions{Version: 3}); err != nil {
		t.Fatal(err)
	}
	closeImage(t, img)
	assertClean(t, path)
	assertEqual(t, "active", qemuRaw(t, path), want)
	assertEqual(t, "snapshot", qemuRaw(t, path, "-l", "snapshot.name=s"), snap)
	img = openRW(t, path)
	if err := img.ApplySnapshot("s"); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "applied", readAll(t, img), snap)
	closeImage(t, img)
	assertClean(t, path)
}
