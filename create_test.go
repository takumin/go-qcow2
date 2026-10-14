package qcow2

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// qemuInfo returns the output of qemu-img info --output=json.
func qemuInfo(t *testing.T, path string) map[string]any {
	t.Helper()
	var info map[string]any
	if err := json.Unmarshal([]byte(run(t, "qemu-img", "info", "--output=json", path)), &info); err != nil {
		t.Fatal(err)
	}
	return info
}

func TestCreatePreallocation(t *testing.T) {
	requireQemu(t)
	for _, c := range []struct {
		mode        string
		version     int
		clusterSize int64
	}{
		{PreallocMetadata, 3, 64 << 10},
		{PreallocMetadata, 2, 512},
		{PreallocFalloc, 3, 4096},
		{PreallocFull, 3, 64 << 10},
	} {
		t.Run(fmt.Sprintf("%s-v%d-%d", c.mode, c.version, c.clusterSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "p.qcow2")
			// Not a multiple of the cluster size nor of an L2 table's range.
			const size = 70<<20 + 1536
			img, err := Create(path, CreateOptions{Size: size, Version: c.version, ClusterSize: c.clusterSize, Preallocation: c.mode})
			if err != nil {
				t.Fatal(err)
			}
			closeImage(t, img)
			assertClean(t, path)
			assertEqual(t, "contents", qemuRaw(t, path), make([]byte, alignUp(size, 512)))

			es := qemuMap(t, path, 0, alignUp(size, 512))
			if len(es) == 0 || es[0].Depth != 0 || !es[0].Data || es[0].HostOffset < 0 {
				t.Fatalf("qemu-img map: %v", es)
			}
			for _, e := range es {
				if e.Depth != 0 || !e.Present || e.HostOffset < 0 {
					t.Fatalf("unallocated range %v", e)
				}
			}
			if fileSize(t, path) < size {
				t.Fatalf("file is %d bytes", fileSize(t, path))
			}
			used, ok := diskUsage(t, path)
			if ok && (c.mode == PreallocMetadata && used >= size/2 || c.mode != PreallocMetadata && used < size) {
				t.Fatalf("%s preallocation uses %d bytes on disk", c.mode, used)
			}

			// Writes overwrite in place.
			before := fileSize(t, path)
			img = openRW(t, path)
			want := make([]byte, img.Size())
			randomWrites(t, rand.New(rand.NewPCG(2, 2)), img, want, 50, 256<<10)
			closeImage(t, img)
			if after := fileSize(t, path); after != before {
				t.Fatalf("file size changed from %d to %d", before, after)
			}
			assertClean(t, path)
			assertEqual(t, "after writes", qemuRaw(t, path), want)
		})
	}
}

func TestCreateFeatures(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "z.qcow2")
	img, err := Create(path, CreateOptions{Size: 4 << 20, CompressionType: "zstd", LazyRefcounts: true})
	if err != nil {
		t.Fatal(err)
	}
	want := mixedData(rand.New(rand.NewPCG(6, 6)), 4<<20, img.ClusterSize())
	if _, err := img.WriteCompressedAt(want, 0); err != nil {
		t.Fatal(err)
	}
	closeImage(t, img)
	assertClean(t, path)
	assertEqual(t, "qemu view", qemuRaw(t, path), want)
	info := fmt.Sprint(qemuInfo(t, path)["format-specific"])
	for _, s := range []string{"compression-type:zstd", "lazy-refcounts:true"} {
		if !strings.Contains(info, s) {
			t.Errorf("qemu-img info lacks %s: %s", s, info)
		}
	}

	for _, opts := range []CreateOptions{
		{Size: 1 << 20, Version: 2, CompressionType: "zstd"},
		{Size: 1 << 20, Version: 2, LazyRefcounts: true},
		{Size: 1 << 20, CompressionType: "lz4"},
		{Size: 1 << 20, Preallocation: "sparse"},
		{Size: 1 << 20, Preallocation: PreallocFull, BackingFile: path},
	} {
		p := filepath.Join(t.TempDir(), "bad.qcow2")
		if img, err := Create(p, opts); err == nil {
			closeImage(t, img)
			t.Errorf("Create(%+v) succeeded", opts)
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("Create(%+v) left a file behind", opts)
		}
	}
}
