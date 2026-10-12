package qcow2

import (
	"bytes"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

// randomDiscards discards n random ranges (some cluster aligned, some not)
// and zeroes them in shadow.
func randomDiscards(t *testing.T, rng *rand.Rand, img *Image, shadow []byte, n int) {
	t.Helper()
	cs := img.ClusterSize()
	for range n {
		l := 1 + rng.Int64N(6*cs)
		off := rng.Int64N(int64(len(shadow)) - l)
		if rng.IntN(2) == 0 {
			off &^= cs - 1
			l = (l + cs - 1) &^ (cs - 1)
			l = min(l, int64(len(shadow))-off)
		}
		if err := img.Discard(off, l); err != nil {
			t.Fatalf("Discard(%d, %d): %v", off, l, err)
		}
		clear(shadow[off : off+l])
	}
}

func TestDiscard(t *testing.T) {
	requireQemu(t)
	for _, tc := range []struct {
		name    string
		version int
		backing bool
	}{
		{"v3", 3, false},
		{"v2", 2, false},
		{"v3-backing", 3, true},
		{"v2-backing", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := CreateOptions{Size: 16<<20 + 1536, Version: tc.version, ClusterSize: 4096}
			want := make([]byte, opts.Size)
			if tc.backing {
				base := filepath.Join(dir, "base.qcow2")
				run(t, "qemu-img", "create", "-q", "-f", "qcow2", base, "16M")
				run(t, "qemu-io", "-c", "write -P 0xee 0 16M", base)
				opts.BackingFile = "base.qcow2"
				copy(want, bytes.Repeat([]byte{0xee}, 16<<20))
			}
			path := filepath.Join(dir, "d.qcow2")
			img, err := Create(path, opts)
			if err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewPCG(31, uint64(tc.version)))
			randomWrites(t, rng, img, want, 100, 64<<10)
			for range 3 {
				randomDiscards(t, rng, img, want, 40)
				assertEqual(t, "after discard", readAll(t, img), want)
				randomWrites(t, rng, img, want, 50, 64<<10)
			}
			randomDiscards(t, rng, img, want, 40)
			// The tail, including the partial last cluster.
			if err := img.Discard(img.Size()-10000, 10000); err != nil {
				t.Fatal(err)
			}
			clear(want[len(want)-10000:])
			assertEqual(t, "readback", readAll(t, img), want)

			if err := img.Discard(1, img.Size()); err != ErrOutOfRange {
				t.Fatalf("out of range discard: %v", err)
			}
			if err := img.Close(); err != nil {
				t.Fatal(err)
			}
			qemuCheck(t, path)
			assertEqual(t, "qemu view", qemuRaw(t, path), want)
		})
	}
}

func TestDiscardEverything(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "e.qcow2")
	img, err := Create(path, CreateOptions{Size: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, img.Size())
	randomWrites(t, rand.New(rand.NewPCG(1, 2)), img, want, 50, 1<<20)
	if err := img.Discard(0, img.Size()); err != nil {
		t.Fatal(err)
	}
	// Discarding an empty image again must not allocate anything.
	end := img.fileEnd
	if err := img.Discard(0, img.Size()); err != nil {
		t.Fatal(err)
	}
	if img.fileEnd != end {
		t.Fatalf("second discard grew the file")
	}
	img.Close()
	qemuCheck(t, path)
	assertEqual(t, "qemu view", qemuRaw(t, path), make([]byte, len(want)))
	out := run(t, "qemu-img", "map", "--output=json", path)
	if bytes.Contains([]byte(out), []byte(`"data": true`)) {
		t.Fatalf("data still mapped after discard:\n%s", out)
	}
}

func TestDiscardSnapshot(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "s.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", path, "8M")
	run(t, "qemu-io", "-c", "write -P 0x42 0 8M", path)
	run(t, "qemu-img", "snapshot", "-c", "snap", path)
	before := qemuRaw(t, path)

	img, err := OpenFile(path, os.O_RDWR, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Clone(before)
	rng := rand.New(rand.NewPCG(41, 41))
	randomWrites(t, rng, img, want, 30, 100<<10)
	randomDiscards(t, rng, img, want, 30)
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	qemuCheck(t, path)
	assertEqual(t, "active", qemuRaw(t, path), want)
	assertEqual(t, "snapshot", qemuRaw(t, path, "-l", "snapshot.name=snap"), before)
}

func TestDiscardCompressed(t *testing.T) {
	requireQemu(t)
	dir := t.TempDir()
	raw := filepath.Join(dir, "src.raw")
	want := compressibleRaw(t, raw, 8<<20)
	path := filepath.Join(dir, "c.qcow2")
	run(t, "qemu-img", "convert", "-c", "-f", "raw", "-O", "qcow2", raw, path)

	img, err := OpenFile(path, os.O_RDWR, nil)
	if err != nil {
		t.Fatal(err)
	}
	randomDiscards(t, rand.New(rand.NewPCG(43, 43)), img, want, 40)
	assertEqual(t, "readback", readAll(t, img), want)
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	qemuCheck(t, path)
	assertEqual(t, "qemu view", qemuRaw(t, path), want)
}

func TestDiscardReadOnly(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "ro.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", path, "1M")
	img, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	if err := img.Discard(0, 4096); err != ErrReadOnly {
		t.Fatalf("got %v, want ErrReadOnly", err)
	}
}
