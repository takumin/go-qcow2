package qcow2

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireQemu skips the test when the QEMU tools used as the reference
// implementation are missing, or fails it when QCOW2_REQUIRE_QEMU is set
// (as in CI), so that a broken environment cannot pass silently.
func requireQemu(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"qemu-img", "qemu-io", "qemu-nbd"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("QCOW2_REQUIRE_QEMU") != "" {
				t.Fatalf("%s not found", tool)
			}
			t.Skipf("%s not found", tool)
		}
	}
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// qemuCheck fails the test unless qemu-img check reports a clean image
// (no corruptions and no leaks).
func qemuCheck(t *testing.T, path string) {
	t.Helper()
	out, err := exec.Command("qemu-img", "check", path).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "No errors were found") {
		t.Fatalf("qemu-img check %s: %v\n%s", path, err, out)
	}
}

// qemuRaw returns the guest contents of an image as seen by QEMU.
func qemuRaw(t *testing.T, path string, extra ...string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.raw")
	args := append([]string{"convert", "-O", "raw"}, extra...)
	run(t, "qemu-img", append(args, path, out)...)
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// closeImage closes img and fails the test on error.
func closeImage(tb testing.TB, img *Image) {
	tb.Helper()
	if err := img.Close(); err != nil {
		tb.Fatal(err)
	}
}

// closeInto closes img and stores the error in *err unless it already holds
// one. Use as defer closeInto(img, &err) with a named error result; an
// image closed explicitly before returns ErrClosed, which is ignored.
func closeInto(img *Image, err *error) {
	if cerr := img.Close(); cerr != nil && !errors.Is(cerr, ErrClosed) && *err == nil {
		*err = cerr
	}
}

func readAll(t *testing.T, img *Image) []byte {
	t.Helper()
	buf := make([]byte, img.Size())
	if _, err := img.ReadAt(buf, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return buf
}

func assertEqual(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: length %d, want %d", what, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: first difference at offset %d (0x%x): got %#02x want %#02x", what, i, i, got[i], want[i])
		}
	}
}

// randomWrites performs n writes of random data at random offsets to img,
// mirroring them into shadow.
func randomWrites(t *testing.T, rng *rand.Rand, img *Image, shadow []byte, n, maxLen int) {
	t.Helper()
	for range n {
		l := 1 + rng.IntN(maxLen)
		off := rng.Int64N(int64(len(shadow) - l))
		if rng.IntN(4) == 0 {
			off &^= 511
		}
		data := make([]byte, l)
		for i := range data {
			data[i] = byte(rng.Uint32())
		}
		if _, err := img.WriteAt(data, off); err != nil {
			t.Fatalf("WriteAt(%d, %d): %v", l, off, err)
		}
		copy(shadow[off:], data)
	}
}

func TestCreateWriteRead(t *testing.T) {
	requireQemu(t)
	for _, tc := range []struct {
		version, refBits int
		cluster          int64
	}{
		{3, 16, 64 << 10},
		{2, 16, 64 << 10},
		{3, 16, 512}, // forces several refcount table relocations
		{3, 1, 4096},
		{3, 2, 512},
		{3, 8, 1024},
		{3, 32, 8192},
		{3, 64, 4096},
		{3, 16, 2 << 20},
	} {
		t.Run(fmt.Sprintf("v%d-rb%d-cs%d", tc.version, tc.refBits, tc.cluster), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.qcow2")
			img, err := Create(path, CreateOptions{Size: 48<<20 + 12345, ClusterSize: tc.cluster, Version: tc.version, RefcountBits: tc.refBits})
			if err != nil {
				t.Fatal(err)
			}
			size := img.Size()
			if size != 48<<20+12800 {
				t.Fatalf("size %d not rounded to 512", size)
			}
			rng := rand.New(rand.NewPCG(1, uint64(tc.cluster)))
			shadow := make([]byte, size)
			randomWrites(t, rng, img, shadow, 150, 200<<10)
			// Last bytes of the disk.
			if _, err := img.WriteAt([]byte("tail"), size-4); err != nil {
				t.Fatal(err)
			}
			copy(shadow[size-4:], "tail")
			if _, err := img.WriteAt([]byte("x"), size-1+1); err != ErrOutOfRange {
				t.Fatalf("write past end: %v", err)
			}
			assertEqual(t, "readback", readAll(t, img), shadow)
			if err := img.Close(); err != nil {
				t.Fatal(err)
			}

			qemuCheck(t, path)
			assertEqual(t, "qemu view", qemuRaw(t, path), shadow)

			img, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer closeImage(t, img)
			assertEqual(t, "reopened", readAll(t, img), shadow)
		})
	}
}

func TestReadQemuImage(t *testing.T) {
	requireQemu(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "q.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", path, "16M")
	run(t, "qemu-io", "-c", "write -P 0xab 0 1M", "-c", "write -P 0xcd 3M 65537",
		"-c", "write -z 512k 128k", "-c", "write -z -u 5M 1M", "-c", "write -P 0x11 15M 1M", path)

	img, err := OpenFile(path, os.O_RDWR, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := qemuRaw(t, path)
	assertEqual(t, "contents", readAll(t, img), want)

	// 512k-640k are preallocated zero clusters (zero flag with an offset),
	// 5M-6M are plain zero clusters.
	for _, off := range []int64{512<<10 + 100, 600 << 10, 5<<20 + 3} {
		if _, err := img.WriteAt([]byte("data"), off); err != nil {
			t.Fatal(err)
		}
		copy(want[off:], "data")
	}
	assertEqual(t, "after writes", readAll(t, img), want)
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	qemuCheck(t, path)
	assertEqual(t, "qemu view", qemuRaw(t, path), want)
}

func compressibleRaw(t *testing.T, path string, size int) []byte {
	t.Helper()
	rng := rand.New(rand.NewPCG(7, 7))
	words := []string{"qcow2 ", "cluster ", "refcount ", "backing ", "ubuntu ", "\x00\x00\x00\x00"}
	var b bytes.Buffer
	for b.Len() < size {
		if rng.IntN(50) == 0 {
			b.Write(make([]byte, 70000)) // some all-zero clusters
		}
		b.WriteString(words[rng.IntN(len(words))])
		if rng.IntN(10) == 0 {
			b.WriteByte(byte(rng.Uint32()))
		}
	}
	data := b.Bytes()[:size]
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCompressed(t *testing.T) {
	requireQemu(t)
	for _, ct := range []string{"zlib", "zstd"} {
		t.Run(ct, func(t *testing.T) {
			dir := t.TempDir()
			raw := filepath.Join(dir, "src.raw")
			want := compressibleRaw(t, raw, 8<<20+4096)
			path := filepath.Join(dir, "c.qcow2")
			run(t, "qemu-img", "convert", "-c", "-f", "raw", "-O", "qcow2", "-o", "compression_type="+ct, raw, path)
			if !strings.Contains(run(t, "qemu-img", "check", path), "compressed clusters") {
				t.Fatal("image has no compressed clusters")
			}

			img, err := OpenFile(path, os.O_RDWR, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertEqual(t, "compressed read", readAll(t, img), want)

			// Partial writes into compressed clusters require decompression
			// into a new cluster and freeing the compressed one.
			rng := rand.New(rand.NewPCG(3, 3))
			randomWrites(t, rng, img, want, 60, 100<<10)
			assertEqual(t, "after writes", readAll(t, img), want)
			if err := img.Close(); err != nil {
				t.Fatal(err)
			}
			qemuCheck(t, path)
			assertEqual(t, "qemu view", qemuRaw(t, path), want)
		})
	}
}

func TestBacking(t *testing.T) {
	requireQemu(t)
	for _, format := range []string{"qcow2", "raw"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			base := filepath.Join(dir, "base."+format)
			run(t, "qemu-img", "create", "-q", "-f", format, base, "20M")
			run(t, "qemu-io", "-f", format, "-c", "write -P 0x5a 0 20M", "-c", "write -P 0x33 1000000 3M", base)
			want := qemuRaw(t, base)

			// Overlay larger than its backing file; the name is relative.
			overlay := filepath.Join(dir, "overlay.qcow2")
			img, err := Create(overlay, CreateOptions{Size: 24 << 20, BackingFile: "base." + format})
			if err != nil {
				t.Fatal(err)
			}
			if name, f := img.BackingFile(); name != "base."+format || f != format {
				t.Fatalf("BackingFile() = %q, %q", name, f)
			}
			want = append(want, make([]byte, 4<<20)...)
			assertEqual(t, "initial", readAll(t, img), want)

			rng := rand.New(rand.NewPCG(5, 5))
			randomWrites(t, rng, img, want, 80, 150<<10)
			assertEqual(t, "after writes", readAll(t, img), want)
			if err := img.Close(); err != nil {
				t.Fatal(err)
			}

			qemuCheck(t, overlay)
			info := run(t, "qemu-img", "info", overlay)
			if !strings.Contains(info, "backing file format: "+format) {
				t.Fatalf("unexpected qemu-img info:\n%s", info)
			}
			assertEqual(t, "qemu view", qemuRaw(t, overlay), want)
			// The backing file must be untouched.
			run(t, "qemu-io", "-f", format, "-c", "read -P 0x5a 0 1000000", base)
		})
	}
}

func TestBackingChain(t *testing.T) {
	requireQemu(t)
	dir := t.TempDir()
	base := filepath.Join(dir, "base.raw")
	compressibleRaw(t, base, 6<<20)
	mid := filepath.Join(dir, "mid.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", "-b", "base.raw", "-F", "raw", "-o", "cluster_size=4096", mid)
	run(t, "qemu-io", "-c", "write -P 0x77 1M 100k", "-c", "write -z 2M 64k", mid)

	top := filepath.Join(dir, "top.qcow2")
	img, err := Create(top, CreateOptions{BackingFile: "mid.qcow2"})
	if err != nil {
		t.Fatal(err)
	}
	want := qemuRaw(t, mid)
	assertEqual(t, "chain", readAll(t, img), want)
	randomWrites(t, rand.New(rand.NewPCG(9, 9)), img, want, 40, 70<<10)
	closeImage(t, img)
	qemuCheck(t, top)
	assertEqual(t, "qemu view", qemuRaw(t, top), want)
}

func TestSnapshotCOW(t *testing.T) {
	requireQemu(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "s.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", path, "8M")
	run(t, "qemu-io", "-c", "write -P 0x42 0 8M", path)
	run(t, "qemu-img", "snapshot", "-c", "snap1", path)
	before := qemuRaw(t, path)

	img, err := OpenFile(path, os.O_RDWR, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Clone(before)
	randomWrites(t, rand.New(rand.NewPCG(11, 11)), img, want, 50, 100<<10)
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	qemuCheck(t, path)
	assertEqual(t, "active", qemuRaw(t, path), want)
	assertEqual(t, "snapshot", qemuRaw(t, path, "-l", "snapshot.name=snap1"), before)
}

func TestResize(t *testing.T) {
	requireQemu(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "r.qcow2")
	img, err := Create(path, CreateOptions{Size: 1 << 20, ClusterSize: 512})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 1<<20)
	rng := rand.New(rand.NewPCG(13, 13))
	randomWrites(t, rng, img, want, 20, 20<<10)
	// 512-byte clusters map 32 KiB per L2 table: this needs a larger L1.
	if err := img.Resize(40<<20 + 7); err != nil {
		t.Fatal(err)
	}
	want = append(want, make([]byte, 39<<20+512)...)
	randomWrites(t, rng, img, want, 40, 50<<10)
	assertEqual(t, "after resize", readAll(t, img), want)
	closeImage(t, img)
	qemuCheck(t, path)
	assertEqual(t, "qemu view", qemuRaw(t, path), want)

	// Growing an overlay that is smaller than its backing file must not
	// expose the backing data.
	base := filepath.Join(dir, "base.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", base, "4M")
	run(t, "qemu-io", "-c", "write -P 0x99 0 4M", base)
	ov := filepath.Join(dir, "ov.qcow2")
	img, err = Create(ov, CreateOptions{Size: 1<<20 + 512, BackingFile: "base.qcow2", BackingFormat: "qcow2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := img.Resize(6 << 20); err != nil {
		t.Fatal(err)
	}
	want = make([]byte, 6<<20)
	for i := range 1<<20 + 512 {
		want[i] = 0x99
	}
	assertEqual(t, "grown overlay", readAll(t, img), want)
	closeImage(t, img)
	qemuCheck(t, ov)
	assertEqual(t, "qemu grown overlay", qemuRaw(t, ov), want)
}

func TestSetBackingFile(t *testing.T) {
	requireQemu(t)
	dir := t.TempDir()
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", filepath.Join(dir, "a.qcow2"), "4M")
	run(t, "qemu-io", "-c", "write -P 0x61 0 4M", filepath.Join(dir, "a.qcow2"))
	run(t, "qemu-img", "convert", "-O", "raw", filepath.Join(dir, "a.qcow2"), filepath.Join(dir, "b.raw"))

	path := filepath.Join(dir, "o.qcow2")
	img, err := Create(path, CreateOptions{BackingFile: "a.qcow2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := img.WriteAt([]byte("hello"), 100); err != nil {
		t.Fatal(err)
	}
	if err := img.SetBackingFile("b.raw", ""); err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{0x61}, 4<<20)
	copy(want[100:], "hello")
	assertEqual(t, "rebased", readAll(t, img), want)
	closeImage(t, img)

	qemuCheck(t, path)
	info := run(t, "qemu-img", "info", path)
	if !strings.Contains(info, "backing file: b.raw") || !strings.Contains(info, "backing file format: raw") {
		t.Fatalf("unexpected qemu-img info:\n%s", info)
	}
	assertEqual(t, "qemu view", qemuRaw(t, path), want)

	img, err = OpenFile(path, os.O_RDWR, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := img.SetBackingFile("", ""); err != nil {
		t.Fatal(err)
	}
	closeImage(t, img)
	if strings.Contains(run(t, "qemu-img", "info", path), "backing file") {
		t.Fatal("backing file not removed")
	}
}

func TestReadOnly(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "ro.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", path, "1M")
	img, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeImage(t, img)
	if _, err := img.WriteAt([]byte{1}, 0); err != ErrReadOnly {
		t.Fatalf("got %v, want ErrReadOnly", err)
	}
	buf := make([]byte, 10)
	if n, err := img.ReadAt(buf, 1<<20-5); n != 5 || err != io.EOF {
		t.Fatalf("ReadAt at end = %d, %v", n, err)
	}
}

func TestNotQcow2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.raw")
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrNotQcow2) {
		t.Fatalf("got %v", err)
	}
}

func TestAllocReuse(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "a.qcow2")
	img, err := Create(path, CreateOptions{Size: 1 << 20, ClusterSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	cs := img.clusterSize
	first, err := img.allocClusters(10)
	if err != nil {
		t.Fatal(err)
	}
	// Free clusters 2..6 (a hole of 5).
	for i := int64(2); i < 7; i++ {
		if err := img.addRefcount(first+i*cs, -1); err != nil {
			t.Fatal(err)
		}
	}
	end := img.fileEnd
	// A run of 6 does not fit into the hole.
	big, err := img.allocClusters(6)
	if err != nil {
		t.Fatal(err)
	}
	if big != end {
		t.Fatalf("6-cluster run at 0x%x, want 0x%x (end of file)", big, end)
	}
	// Runs that fit are taken from the hole, lowest offset first.
	for _, want := range []int64{2, 5} {
		n := int64(3)
		if want == 5 {
			n = 2
		}
		off, err := img.allocClusters(n)
		if err != nil {
			t.Fatal(err)
		}
		if off != first+want*cs {
			t.Fatalf("got 0x%x, want 0x%x", off, first+want*cs)
		}
	}
	if img.fileEnd != end+6*cs {
		t.Fatalf("file grew to 0x%x", img.fileEnd)
	}
	// Release everything again so that qemu-img check sees no leaks.
	for i := int64(0); i < 10; i++ {
		if err := img.addRefcount(first+i*cs, -1); err != nil {
			t.Fatal(err)
		}
	}
	for i := int64(0); i < 6; i++ {
		if err := img.addRefcount(big+i*cs, -1); err != nil {
			t.Fatal(err)
		}
	}
	closeImage(t, img)
	qemuCheck(t, path)
}

// TestClusterReuseAfterSnapshotDelete checks that copy-on-write allocations
// reuse the clusters freed when QEMU deletes a snapshot instead of growing
// the file.
func TestClusterReuseAfterSnapshotDelete(t *testing.T) {
	requireQemu(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "s.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", path, "8M")
	run(t, "qemu-io", "-c", "write -P 0x42 0 8M", path)

	rng := rand.New(rand.NewPCG(17, 17))
	var want []byte
	rewrite := func() int64 {
		run(t, "qemu-img", "snapshot", "-c", "snap", path)
		img, err := OpenFile(path, os.O_RDWR, nil)
		if err != nil {
			t.Fatal(err)
		}
		want = make([]byte, img.Size())
		for i := range want {
			want[i] = byte(rng.Uint32())
		}
		// Every cluster is shared with the snapshot, so this rewrites all of them.
		if _, err := img.WriteAt(want, 0); err != nil {
			t.Fatal(err)
		}
		if err := img.Close(); err != nil {
			t.Fatal(err)
		}
		run(t, "qemu-img", "snapshot", "-d", "snap", path)
		qemuCheck(t, path)
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}

	size1 := rewrite()
	for i := range 3 {
		if size := rewrite(); size > size1 {
			t.Fatalf("round %d: file grew from %d to %d bytes", i+2, size1, size)
		}
	}
	assertEqual(t, "qemu view", qemuRaw(t, path), want)
}

func TestTruncateOnClose(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "t.qcow2")
	img, err := Create(path, CreateOptions{Size: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, img.Size())
	for i := range 8 << 20 {
		want[i] = byte(i % 251)
	}
	if _, err := img.WriteAt(want[:8<<20], 0); err != nil {
		t.Fatal(err)
	}
	if err := img.Flush(); err != nil {
		t.Fatal(err)
	}
	full := fileSize(t, path)
	// The last 3 MiB of data sit at the end of the file.
	if err := img.Discard(5<<20, 3<<20); err != nil {
		t.Fatal(err)
	}
	clear(want[5<<20 : 8<<20])
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	got := fileSize(t, path)
	t.Logf("file size %d -> %d", full, got)
	if full-got != 3<<20 {
		t.Fatalf("file shrank by %d bytes, want %d", full-got, 3<<20)
	}
	qemuCheck(t, path)
	assertEqual(t, "qemu view", qemuRaw(t, path), want)

	// Nothing to cut: the size stays.
	img, err = OpenFile(path, os.O_RDWR, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	if s := fileSize(t, path); s != got {
		t.Fatalf("size changed to %d", s)
	}
}
