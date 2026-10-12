package qcow2

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// qemuDirty returns the dirty extents of a bitmap as QEMU sees them, by
// exporting it with qemu-nbd and mapping it with qemu-img.
func qemuDirty(t *testing.T, path, name string) []Extent {
	t.Helper()
	// UNIX socket paths are limited to 108 bytes.
	dir, err := os.MkdirTemp("/tmp", "qn")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	nbd := exec.Command("qemu-nbd", "-r", "-f", "qcow2", "-B", name, "--socket="+sock, path)
	if err := nbd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		nbd.Process.Kill()
		nbd.Wait()
	}()
	for i := 0; ; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if i == 200 {
			t.Fatal("qemu-nbd did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	out := run(t, "qemu-img", "map", "--output=json", "--image-opts",
		"driver=nbd,server.type=unix,server.path="+sock+",x-dirty-bitmap=qemu:dirty-bitmap:"+name)
	var m []struct {
		Start, Length int64
		Data          bool
	}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var exts []Extent
	for _, e := range m {
		// In this mode dirty areas are reported as "data": false.
		if e.Data {
			continue
		}
		if n := len(exts); n > 0 && exts[n-1].Offset+exts[n-1].Length == e.Start {
			exts[n-1].Length += e.Length
		} else {
			exts = append(exts, Extent{e.Start, e.Length})
		}
	}
	return exts
}

// dirtyModel tracks the expected contents of a bitmap.
type dirtyModel struct {
	gran, size int64
	bits       []bool
}

func newDirtyModel(gran, size int64) *dirtyModel {
	return &dirtyModel{gran: gran, size: size, bits: make([]bool, (size+gran-1)/gran)}
}

func (m *dirtyModel) mark(off, n int64) {
	for i := off / m.gran; i <= (off+n-1)/m.gran; i++ {
		m.bits[i] = true
	}
}

func (m *dirtyModel) extents() []Extent {
	var exts []Extent
	for i := 0; i < len(m.bits); {
		if !m.bits[i] {
			i++
			continue
		}
		j := i
		for j < len(m.bits) && m.bits[j] {
			j++
		}
		exts = append(exts, Extent{int64(i) * m.gran, min(int64(j)*m.gran, m.size) - int64(i)*m.gran})
		i = j
	}
	return exts
}

func assertExtents(t *testing.T, what string, got, want []Extent) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s:\n got  %v\n want %v", what, got, want)
	}
}

func dirtyExtents(t *testing.T, img *Image, name string) []Extent {
	t.Helper()
	exts, err := img.DirtyExtents(name)
	if err != nil {
		t.Fatal(err)
	}
	return exts
}

func TestBitmapQemuInterop(t *testing.T) {
	requireQemu(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "b.qcow2")
	const size = 16 << 20
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", path, "16M")
	run(t, "qemu-img", "bitmap", "--add", path, "b0")
	run(t, "qemu-img", "bitmap", "--add", "-g", "4096", path, "b1")
	run(t, "qemu-io", "-c", "write 1M 4k", "-c", "write 5000000 100000", path)
	run(t, "qemu-img", "bitmap", "--disable", path, "b1")
	run(t, "qemu-io", "-c", "write 9M 1", path)

	b0 := newDirtyModel(65536, size)
	b1 := newDirtyModel(4096, size)
	for _, r := range [][2]int64{{1 << 20, 4096}, {5000000, 100000}} {
		b0.mark(r[0], r[1])
		b1.mark(r[0], r[1])
	}
	b0.mark(9<<20, 1)

	// QEMU's bitmaps as read by this package.
	img, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []BitmapInfo{{Name: "b0", Granularity: 65536, Enabled: true}, {Name: "b1", Granularity: 4096}}
	if got := img.Bitmaps(); !slices.Equal(got, want) {
		t.Fatalf("Bitmaps() = %+v, want %+v", got, want)
	}
	assertExtents(t, "b0 (qemu writes)", dirtyExtents(t, img, "b0"), b0.extents())
	assertExtents(t, "b1 (qemu writes)", dirtyExtents(t, img, "b1"), b1.extents())
	img.Close()

	// Writes through this package, read back by QEMU.
	img, err = OpenFile(path, os.O_RDWR, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := img.WriteAt(make([]byte, 70000), 12345678); err != nil {
		t.Fatal(err)
	}
	b0.mark(12345678, 70000)
	if err := img.Discard(3<<20, 200000); err != nil {
		t.Fatal(err)
	}
	b0.mark(3<<20, 200000)
	if err := img.AddBitmap("b2", 512, true); err != nil {
		t.Fatal(err)
	}
	b2 := newDirtyModel(512, size)
	if _, err := img.WriteAt([]byte("x"), size-1); err != nil {
		t.Fatal(err)
	}
	b0.mark(size-1, 1)
	b2.mark(size-1, 1)
	assertExtents(t, "b0 (in memory)", dirtyExtents(t, img, "b0"), b0.extents())
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	qemuCheck(t, path)
	if info := run(t, "qemu-img", "info", path); strings.Contains(info, "in-use") {
		t.Fatalf("bitmap left in use:\n%s", info)
	}
	assertExtents(t, "b0 (qemu view)", qemuDirty(t, path, "b0"), b0.extents())
	assertExtents(t, "b1 (qemu view)", qemuDirty(t, path, "b1"), b1.extents())
	assertExtents(t, "b2 (qemu view)", qemuDirty(t, path, "b2"), b2.extents())

	// QEMU keeps recording into the bitmaps we wrote.
	run(t, "qemu-io", "-c", "write 7M 1", path)
	b0.mark(7<<20, 1)
	b2.mark(7<<20, 1)
	img, err = OpenFile(path, os.O_RDWR, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertExtents(t, "b0 (reopened)", dirtyExtents(t, img, "b0"), b0.extents())
	assertExtents(t, "b2 (reopened)", dirtyExtents(t, img, "b2"), b2.extents())

	// Management operations.
	if err := img.ClearBitmap("b0"); err != nil {
		t.Fatal(err)
	}
	if err := img.SetBitmapEnabled("b2", false); err != nil {
		t.Fatal(err)
	}
	if err := img.SetBitmapEnabled("b1", true); err != nil {
		t.Fatal(err)
	}
	if err := img.RemoveBitmap("b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := img.WriteAt([]byte("y"), 0); err != nil {
		t.Fatal(err)
	}
	b0 = newDirtyModel(65536, size)
	b0.mark(0, 1)
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	qemuCheck(t, path)
	info := run(t, "qemu-img", "info", "--output=json", path)
	for _, s := range []string{`"name": "b0"`, `"name": "b2"`} {
		if !strings.Contains(info, s) {
			t.Fatalf("%s missing:\n%s", s, info)
		}
	}
	if strings.Contains(info, `"name": "b1"`) {
		t.Fatalf("b1 not removed:\n%s", info)
	}
	assertExtents(t, "b0 after clear", qemuDirty(t, path, "b0"), b0.extents())
	assertExtents(t, "b2 after disable", qemuDirty(t, path, "b2"), b2.extents())

	// Remove everything: the extension disappears.
	img, err = OpenFile(path, os.O_RDWR, nil)
	if err != nil {
		t.Fatal(err)
	}
	img.RemoveBitmap("b0")
	img.RemoveBitmap("b2")
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	qemuCheck(t, path)
	if info := run(t, "qemu-img", "info", path); strings.Contains(info, "bitmaps") {
		t.Fatalf("bitmaps not removed:\n%s", info)
	}
}

func TestBitmapInUseAfterCrash(t *testing.T) {
	requireQemu(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "c.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", path, "4M")
	run(t, "qemu-img", "bitmap", "--add", path, "on")
	run(t, "qemu-img", "bitmap", "--add", path, "off")
	run(t, "qemu-img", "bitmap", "--disable", path, "off")
	initial, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	crash := func(write bool) {
		t.Helper()
		st := newCrashStorage(initial, 0)
		img, err := openStorage(st, path, false, Options{}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if write {
			if _, err := img.WriteAt([]byte("data"), 100); err != nil {
				t.Fatal(err)
			}
		}
		// The process dies without Close.
		if err := os.WriteFile(path, st.cur, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Opening writable without changes keeps the bitmaps usable.
	crash(false)
	qemuCheck(t, path)
	if info := run(t, "qemu-img", "info", path); strings.Contains(info, "in-use") {
		t.Fatalf("bitmap in use without writes:\n%s", info)
	}

	// After a write, the enabled bitmap is in use; the disabled one is not
	// affected by guest writes and stays valid.
	crash(true)
	qemuCheck(t, path)
	img, err := OpenFile(path, os.O_RDWR, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []BitmapInfo{{Name: "on", Granularity: 65536, Enabled: true, InUse: true}, {Name: "off", Granularity: 65536}}
	if got := img.Bitmaps(); !slices.Equal(got, want) {
		t.Fatalf("Bitmaps() = %+v, want %+v", got, want)
	}
	if _, err := img.DirtyExtents("on"); !errors.Is(err, ErrBitmapInUse) {
		t.Fatalf("DirtyExtents of in-use bitmap: %v", err)
	}
	if err := img.Resize(8 << 20); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Resize with in-use bitmap: %v", err)
	}
	if err := img.RemoveBitmap("on"); err != nil {
		t.Fatal(err)
	}
	if err := img.Resize(8 << 20); err != nil {
		t.Fatal(err)
	}
	if err := img.Close(); err != nil {
		t.Fatal(err)
	}
	qemuCheck(t, path)
	info := run(t, "qemu-img", "info", "--output=json", path)
	if strings.Contains(info, `"name": "on"`) || !strings.Contains(info, `"name": "off"`) {
		t.Fatalf("unexpected bitmaps:\n%s", info)
	}
	// The disabled bitmap grew with the disk and stays clean.
	if exts := qemuDirty(t, path, "off"); len(exts) != 0 {
		t.Fatalf("off: %v", exts)
	}
}
