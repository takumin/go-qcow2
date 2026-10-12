package qcow2

import (
	"bytes"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRealImage exercises a real-world image such as an Ubuntu cloud image:
//
//	QCOW2_TEST_IMAGE=/path/to/ubuntu-26.04-server-cloudimg-amd64.img go test -run TestRealImage -v
//
// The image itself is never modified.
func TestRealImage(t *testing.T) {
	src := os.Getenv("QCOW2_TEST_IMAGE")
	if src == "" {
		t.Skip("QCOW2_TEST_IMAGE not set")
	}
	requireQemu(t)
	src, err := filepath.Abs(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	t.Run("read", func(t *testing.T) {
		img, err := Open(src)
		if err != nil {
			t.Fatal(err)
		}
		defer closeImage(t, img)
		t.Logf("version %d, cluster size %d, virtual size %d", img.Version(), img.ClusterSize(), img.Size())
		compareWithQemu(t, img, src)
	})

	t.Run("overlay", func(t *testing.T) {
		path := filepath.Join(dir, "overlay.qcow2")
		img, err := Create(path, CreateOptions{BackingFile: src, BackingFormat: "qcow2"})
		if err != nil {
			t.Fatal(err)
		}
		writeSome(t, img, 1)
		discardSome(t, img, 4)
		// Growing like a cloud VM disk.
		if err := img.Resize(img.Size() + 1<<30); err != nil {
			t.Fatal(err)
		}
		if _, err := img.WriteAt([]byte("end of grown disk"), img.Size()-17); err != nil {
			t.Fatal(err)
		}
		if err := img.Close(); err != nil {
			t.Fatal(err)
		}
		qemuCheck(t, path)
		img, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer closeImage(t, img)
		compareWithQemu(t, img, path)
		// Backing file untouched.
		run(t, "qemu-img", "check", src)
	})

	t.Run("write-in-place", func(t *testing.T) {
		path := filepath.Join(dir, "copy.qcow2")
		copyFile(t, src, path)
		img, err := OpenFile(path, os.O_RDWR, nil)
		if err != nil {
			t.Fatal(err)
		}
		writeSome(t, img, 2)
		discardSome(t, img, 3)
		if err := img.Close(); err != nil {
			t.Fatal(err)
		}
		qemuCheck(t, path)
		img, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer closeImage(t, img)
		compareWithQemu(t, img, path)
	})
}

func writeSome(t *testing.T, img *Image, seed uint64) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed))
	for range 300 {
		data := make([]byte, 1+rng.IntN(256<<10))
		for i := range data {
			data[i] = byte(rng.Uint32())
		}
		off := rng.Int64N(img.Size() - int64(len(data)))
		if _, err := img.WriteAt(data, off); err != nil {
			t.Fatal(err)
		}
	}
}

func discardSome(t *testing.T, img *Image, seed uint64) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed))
	for range 100 {
		l := 1 + rng.Int64N(4<<20)
		if err := img.Discard(rng.Int64N(img.Size()-l), l); err != nil {
			t.Fatal(err)
		}
	}
}

// compareWithQemu compares img with qemu-img's raw conversion of path
// without loading either fully into memory.
func compareWithQemu(t *testing.T, img *Image, path string) {
	t.Helper()
	raw := filepath.Join(t.TempDir(), "qemu.raw")
	if out, err := exec.Command("qemu-img", "convert", "-O", "raw", path, raw).CombinedOutput(); err != nil {
		t.Fatalf("qemu-img convert: %v\n%s", err, out)
	}
	f, err := os.Open(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }() // read-only
	if fi, _ := f.Stat(); fi.Size() != img.Size() {
		t.Fatalf("size %d, qemu %d", img.Size(), fi.Size())
	}
	const chunk = 8 << 20
	a, b := make([]byte, chunk), make([]byte, chunk)
	for off := int64(0); off < img.Size(); off += chunk {
		n := int(min(chunk, img.Size()-off))
		if _, err := img.ReadAt(a[:n], off); err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(f, b[:n]); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a[:n], b[:n]) {
			assertEqual(t, "chunk", a[:n], b[:n])
		}
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }() // read-only
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
