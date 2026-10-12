package qcow2

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

// benchDir returns where benchmark images live. Syncs are only meaningful
// on a real disk, so set QCOW2_BENCH_DIR when /tmp is a tmpfs.
func benchDir(b *testing.B) string {
	if d := os.Getenv("QCOW2_BENCH_DIR"); d != "" {
		dir, err := os.MkdirTemp(d, "bench")
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { os.RemoveAll(dir) })
		return dir
	}
	return b.TempDir()
}

// BenchmarkFlush measures a 4 KiB write followed by Flush.
func BenchmarkFlush(b *testing.B) {
	for _, bc := range []struct {
		name     string
		allocate bool
	}{
		{"overwrite", false}, // in-place writes to allocated clusters
		{"allocate", true},   // every write allocates a new cluster
		{"allocate-discard", true},
	} {
		b.Run(bc.name, func(b *testing.B) {
			path := filepath.Join(benchDir(b), "b.qcow2")
			img, err := Create(path, CreateOptions{Size: 64 << 30})
			if err != nil {
				b.Fatal(err)
			}
			defer img.Close()
			buf := make([]byte, 4096)
			if !bc.allocate {
				if _, err := img.WriteAt(buf, 0); err != nil {
					b.Fatal(err)
				}
			}
			rng := rand.New(rand.NewPCG(1, 1))
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				off := int64(0)
				if bc.allocate {
					off = int64(i) * img.ClusterSize()
				} else {
					off = rng.Int64N(img.ClusterSize()/4096) * 4096
				}
				if _, err := img.WriteAt(buf, off); err != nil {
					b.Fatal(err)
				}
				if bc.name == "allocate-discard" && i > 0 {
					// Frees the previous cluster.
					if err := img.Discard(off-img.ClusterSize(), img.ClusterSize()); err != nil {
						b.Fatal(err)
					}
				}
				if err := img.Flush(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
