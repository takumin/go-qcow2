package qcow2

import (
	"bytes"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// memStorage is a Storage in memory that counts punched bytes.
type memStorage struct {
	mu      sync.Mutex
	data    []byte
	punched int64
	closed  bool
}

func (m *memStorage) ReadAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *memStorage) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if end := off + int64(len(p)); end > int64(len(m.data)) {
		m.data = append(m.data, make([]byte, end-int64(len(m.data)))...)
	}
	return copy(m.data[off:], p), nil
}

func (m *memStorage) Size() (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.data)), nil
}

func (m *memStorage) Truncate(size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if size < int64(len(m.data)) {
		m.data = m.data[:size]
	} else {
		m.data = append(m.data, make([]byte, size-int64(len(m.data)))...)
	}
	return nil
}

func (m *memStorage) Sync() error { return nil }

func (m *memStorage) Close() error {
	m.closed = true
	return nil
}

func (m *memStorage) PunchHole(off, length int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.data[off:min(off+length, int64(len(m.data)))])
	m.punched += length
	return nil
}

func TestStorage(t *testing.T) {
	requireQemu(t)
	dir := t.TempDir()
	base := filepath.Join(dir, "base.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", base, "4M")
	run(t, "qemu-io", "-c", "write -P 0x11 0 4M", base)
	baseData, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}

	// The backing file is read from memory too.
	opener := func(path, format string) (Backing, error) {
		if filepath.Base(path) != "base.qcow2" || format != "qcow2" {
			t.Errorf("BackingOpener(%q, %q)", path, format)
		}
		return OpenReaderAt(bytes.NewReader(baseData), int64(len(baseData)), nil)
	}

	m := &memStorage{}
	img, err := CreateStorage(m, CreateOptions{Size: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, img.Size())
	randomWrites(t, rand.New(rand.NewPCG(8, 8)), img, want, 30, 64<<10)
	closeImage(t, img)
	if !m.closed {
		t.Fatal("Close did not close the storage")
	}
	if _, err := CreateStorage(m, CreateOptions{Size: 1 << 20}); err == nil {
		t.Fatal("CreateStorage accepted a non-empty storage")
	}

	// Record the backing file and modify the image through OpenStorage.
	m.closed = false
	img, err = OpenStorage(m, os.O_RDWR, &Options{BackingOpener: opener})
	if err != nil {
		t.Fatal(err)
	}
	if err := img.SetBackingFile("/nonexistent/base.qcow2", "qcow2"); err != nil {
		t.Fatal(err)
	}
	// Unallocated clusters now show the backing file.
	want = readAll(t, img)
	if err := img.Discard(0, 1<<20); err != nil {
		t.Fatal(err)
	}
	clear(want[:1<<20])
	if _, err := img.WriteAt([]byte("hello"), 2<<20); err != nil {
		t.Fatal(err)
	}
	copy(want[2<<20:], "hello")
	closeImage(t, img)
	if m.punched == 0 {
		t.Fatal("no clusters punched through HolePuncher")
	}

	path := filepath.Join(dir, "mem.qcow2")
	if err := os.WriteFile(path, m.data, 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "qemu-img", "rebase", "-u", "-F", "qcow2", "-b", base, path)
	qemuCheck(t, path)
	assertEqual(t, "qemu view", qemuRaw(t, path), want)

	// Read-only through an io.ReaderAt.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	img, err = OpenReaderAt(bytes.NewReader(data), int64(len(data)), &Options{BackingOpener: opener})
	if err != nil {
		t.Fatal(err)
	}
	defer closeImage(t, img)
	assertEqual(t, "OpenReaderAt", readAll(t, img), want)
	if _, err := img.WriteAt([]byte{1}, 0); err != ErrReadOnly {
		t.Fatalf("WriteAt on OpenReaderAt image: %v", err)
	}
}
