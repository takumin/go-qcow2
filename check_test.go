package qcow2

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checkClean(t *testing.T, img *Image) {
	t.Helper()
	res, err := img.Check()
	if err != nil {
		t.Fatal(err)
	}
	if res.Leaks != 0 || res.LeaksPastEOF != 0 || res.Corruptions != 0 {
		t.Fatalf("Check: %d leaks, %d past EOF, %d corruptions: %v", res.Leaks, res.LeaksPastEOF, res.Corruptions, res.Errors)
	}
}

func TestCheckQemuImages(t *testing.T) {
	requireQemu(t)
	for _, tc := range []struct {
		name  string
		setup []string // shell commands; %s is replaced by the image path
	}{
		{"plain", []string{"qemu-io -c 'write -P 1 0 3M' -c 'write -z 5M 1M' %s"}},
		{"snapshots", []string{"qemu-io -c 'write -P 1 0 3M' %s", "qemu-img snapshot -c a %s", "qemu-io -c 'write -P 2 1M 3M' %s", "qemu-img snapshot -c b %s", "qemu-io -c 'write -P 3 2M 1M' %s"}},
		{"bitmaps", []string{"qemu-io -c 'write -P 1 0 3M' %s", "qemu-img bitmap --add %s b0", "qemu-img bitmap --add -g 65536 %s b1"}},
		{"v2", []string{"-o compat=0.10", "qemu-io -c 'write -P 1 0 3M' %s", "qemu-img snapshot -c a %s", "qemu-io -c 'write -P 2 1M 1M' %s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "c.qcow2")
			create := []string{"create", "-q", "-f", "qcow2"}
			cmds := tc.setup
			if strings.HasPrefix(cmds[0], "-o ") {
				create = append(create, "-o", strings.TrimPrefix(cmds[0], "-o "))
				cmds = cmds[1:]
			}
			run(t, "qemu-img", append(create, path, "8M")...)
			for _, c := range cmds {
				run(t, "sh", "-c", strings.ReplaceAll(c, "%s", path))
			}
			qemuCheck(t, path)

			img, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			checkClean(t, img)
			img.Close()

			// Opening writable keeps the bitmaps valid.
			img, err = OpenFile(path, os.O_RDWR, nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := img.Check()
			if err != nil {
				t.Fatal(err)
			}
			qemuLeaks, err := qemuLeakCount(path)
			if err != nil {
				t.Fatal(err)
			}
			if res.Leaks != 0 || qemuLeaks != 0 || res.Corruptions != 0 {
				t.Fatalf("writable Check: %+v, qemu-img check: %d leaks", res, qemuLeaks)
			}
			if _, err := img.RepairLeaks(); err != nil {
				t.Fatal(err)
			}
			checkClean(t, img)
			want := qemuRaw(t, path)
			if err := img.Close(); err != nil {
				t.Fatal(err)
			}
			qemuCheck(t, path)
			assertEqual(t, "after repair", qemuRaw(t, path), want)
		})
	}
}

func TestCheckLeaksAndCorruption(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "l.qcow2")
	img, err := Create(path, CreateOptions{Size: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := img.WriteAt(make([]byte, 1<<20), 0); err != nil {
		t.Fatal(err)
	}
	// Five clusters with a refcount but no reference.
	leaked, err := img.allocClusters(5)
	if err != nil {
		t.Fatal(err)
	}
	if err := img.Flush(); err != nil {
		t.Fatal(err)
	}
	// Leaked clusters must be written for qemu to see them inside the file.
	if _, err := img.f.WriteAt(make([]byte, 5*img.clusterSize), leaked); err != nil {
		t.Fatal(err)
	}
	res, err := img.Check()
	if err != nil {
		t.Fatal(err)
	}
	if res.Leaks != 5 || res.Corruptions != 0 {
		t.Fatalf("Check: %+v", res)
	}
	if n, err := qemuLeakCount(path); err != nil || n != 5 {
		t.Fatalf("qemu-img check: %d leaks, %v", n, err)
	}
	if n, err := img.RepairLeaks(); err != nil || n != 5 {
		t.Fatalf("RepairLeaks = %d, %v", n, err)
	}
	checkClean(t, img)
	qemuCheck(t, path)

	// Drop the refcount of a cluster that is in use.
	entry, err := img.l2Entry(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := img.addRefcount(int64(entry&l2OffsetMask), -1); err != nil {
		t.Fatal(err)
	}
	res, err = img.Check()
	if err != nil {
		t.Fatal(err)
	}
	// Refcount too low, and the COPIED flag no longer matches.
	if res.Corruptions != 2 {
		t.Fatalf("Check: %+v", res)
	}
	if _, err := img.RepairLeaks(); err == nil {
		t.Fatal("RepairLeaks accepted a corrupt image")
	}
	if _, err := qemuLeakCount(path); err == nil || !strings.Contains(err.Error(), "exit status 2") {
		t.Fatalf("qemu-img check did not report corruption: %v", err)
	}
	img.Close()
}

// TestFlushSyncs pins down how many syncs a flush needs.
func TestFlushSyncs(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "s.qcow2")
	img, err := Create(path, CreateOptions{Size: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := img.WriteAt(make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}
	img.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st := newCrashStorage(data, 0)
	img, err = openStorage(st, path, false, Options{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	cs := img.ClusterSize()
	for _, tc := range []struct {
		name  string
		op    func() error
		syncs int
	}{
		{"nothing", func() error { return nil }, 0},
		{"overwrite", func() error { _, err := img.WriteAt([]byte{1}, 10); return err }, 1},
		{"allocate", func() error { _, err := img.WriteAt([]byte{1}, cs); return err }, 2},
		{"discard", func() error { return img.Discard(cs, cs) }, 1},
		{"allocate after discard", func() error { _, err := img.WriteAt([]byte{1}, 2*cs); return err }, 2},
		// An L2 table maps 512 MiB.
		{"new L2 table", func() error { _, err := img.WriteAt([]byte{1}, 600<<20); return err }, 3},
	} {
		if err := tc.op(); err != nil {
			t.Fatal(err)
		}
		before := st.syncs
		if err := img.Flush(); err != nil {
			t.Fatal(err)
		}
		if got := st.syncs - before; got != tc.syncs {
			t.Errorf("%s: %d syncs, want %d", tc.name, got, tc.syncs)
		}
	}
}
