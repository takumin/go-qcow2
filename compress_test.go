package qcow2

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
)

// mixedData returns data whose clusters alternate between compressible
// text, random bytes and zeros.
func mixedData(rng *rand.Rand, n int, clusterSize int64) []byte {
	b := make([]byte, n)
	for c := 0; c < n; c += int(clusterSize) {
		chunk := b[c:min(c+int(clusterSize), n)]
		switch (c / int(clusterSize)) % 3 {
		case 0:
			for i := range chunk {
				chunk[i] = "the quick brown fox jumps over the lazy dog "[(c+i)%44]
			}
		case 1:
			for i := range chunk {
				chunk[i] = byte(rng.Uint32())
			}
		}
	}
	return b
}

func TestWriteCompressed(t *testing.T) {
	requireQemu(t)
	for _, c := range []struct {
		compression string
		clusterSize int64
	}{
		{"zlib", 64 << 10},
		{"zlib", 512},
		{"zstd", 4096},
		{"zlib", 2 << 20},
	} {
		t.Run(fmt.Sprintf("%s-%d", c.compression, c.clusterSize), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "c.qcow2")
			const size = 8<<20 + 1536 // ends with a partial cluster
			run(t, "qemu-img", "create", "-q", "-f", "qcow2",
				"-o", fmt.Sprintf("compression_type=%s,cluster_size=%d", c.compression, c.clusterSize), path, fmt.Sprint(size))
			rng := rand.New(rand.NewPCG(4, uint64(c.clusterSize)))
			img := openRW(t, path)
			want := mixedData(rng, size, c.clusterSize)
			if _, err := img.WriteCompressedAt(want, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := img.WriteCompressedAt(want[:100], 0); err == nil {
				t.Fatal("partial compressed write accepted")
			}
			closeImage(t, img)
			assertClean(t, path)
			assertEqual(t, "qemu view", qemuRaw(t, path), want)
			if !strings.Contains(run(t, "qemu-img", "map", "--output=json", path), `"compressed": true`) {
				t.Fatal("no compressed clusters")
			}
			plain := filepath.Join(t.TempDir(), "plain.qcow2")
			run(t, "qemu-img", "create", "-q", "-f", "qcow2", "-o", fmt.Sprintf("cluster_size=%d", c.clusterSize), plain, fmt.Sprint(size))
			img = openRW(t, plain)
			if _, err := img.WriteAt(want, 0); err != nil {
				t.Fatal(err)
			}
			closeImage(t, img)
			if fileSize(t, path) >= fileSize(t, plain) {
				t.Fatalf("compressed image is %d bytes, uncompressed %d", fileSize(t, path), fileSize(t, plain))
			}

			// Rewrite some compressed clusters partially, discard others and
			// compress over existing data.
			img = openRW(t, path)
			randomWrites(t, rng, img, want, 20, int(c.clusterSize))
			if err := img.Discard(c.clusterSize, 3*c.clusterSize); err != nil {
				t.Fatal(err)
			}
			clear(want[c.clusterSize : 4*c.clusterSize])
			again := mixedData(rng, int(min(4*c.clusterSize, size-2*c.clusterSize)), c.clusterSize)
			if _, err := img.WriteCompressedAt(again, 2*c.clusterSize); err != nil {
				t.Fatal(err)
			}
			copy(want[2*c.clusterSize:], again)
			assertEqual(t, "read back", readAll(t, img), want)
			closeImage(t, img)
			assertClean(t, path)
			assertEqual(t, "qemu view after rewrite", qemuRaw(t, path), want)
		})
	}
}

// TestWriteCompressedFreesSharedClusters checks that host clusters packed
// with several compressed clusters are freed when the last one goes.
func TestWriteCompressedFreesSharedClusters(t *testing.T) {
	requireQemu(t)
	path := filepath.Join(t.TempDir(), "c.qcow2")
	img, err := Create(path, CreateOptions{Size: 4 << 20, ClusterSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("compressible "), (4<<20)/13+1)[:4<<20]
	if _, err := img.WriteCompressedAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if err := img.Flush(); err != nil {
		t.Fatal(err)
	}
	packed := fileSize(t, path)
	if err := img.Discard(0, img.Size()); err != nil {
		t.Fatal(err)
	}
	// Freed clusters become available once the discard is on disk.
	if err := img.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := img.WriteCompressedAt(data, 0); err != nil {
		t.Fatal(err)
	}
	closeImage(t, img)
	assertClean(t, path)
	assertEqual(t, "qemu view", qemuRaw(t, path), data)
	if size := fileSize(t, path); size > packed {
		t.Fatalf("file grew from %d to %d bytes: freed compressed clusters were not reused", packed, size)
	}
}
