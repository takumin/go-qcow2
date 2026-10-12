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
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
)

var errCrash = errors.New("simulated crash")

// repairedImages counts crashed images on which RepairLeaks had work to do.
var repairedImages atomic.Int64

// crashSlots bounds the crash runs in flight across all scenarios.
var crashSlots = make(chan struct{}, runtime.GOMAXPROCS(0))

const sectorSize = 512

// crashStorage keeps an image in memory and models a volatile write cache:
// cur is what the process sees, durable what has been synced. A crash can
// be injected before the n-th mutating operation.
type crashStorage struct {
	cur, durable []byte
	dirty        map[int64]struct{} // sectors written since the last sync
	ops, crashAt int
	crashed      bool
	inFlush      int
	syncs        int
	flushOps     []int // operations performed while flushing
}

func (s *crashStorage) flushing(b bool) {
	if b {
		s.inFlush++
	} else {
		s.inFlush--
	}
}

func newCrashStorage(data []byte, crashAt int) *crashStorage {
	return &crashStorage{
		cur:     bytes.Clone(data),
		durable: bytes.Clone(data),
		dirty:   make(map[int64]struct{}),
		crashAt: crashAt,
	}
}

func (s *crashStorage) op() error {
	if s.crashed {
		return errCrash
	}
	s.ops++
	if s.inFlush > 0 {
		s.flushOps = append(s.flushOps, s.ops)
	}
	if s.crashAt > 0 && s.ops >= s.crashAt {
		s.crashed = true
		return errCrash
	}
	return nil
}

func (s *crashStorage) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(s.cur)) {
		return 0, io.EOF
	}
	n := copy(p, s.cur[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (s *crashStorage) markDirty(off, n int64) {
	for sec := off / sectorSize; sec <= (off+n-1)/sectorSize; sec++ {
		s.dirty[sec] = struct{}{}
	}
}

func (s *crashStorage) WriteAt(p []byte, off int64) (int, error) {
	if err := s.op(); err != nil {
		return 0, err
	}
	if end := off + int64(len(p)); end > int64(len(s.cur)) {
		s.cur = append(s.cur, make([]byte, end-int64(len(s.cur)))...)
	}
	copy(s.cur[off:], p)
	s.markDirty(off, int64(len(p)))
	return len(p), nil
}

func (s *crashStorage) punchHole(off, n int64) error {
	if err := s.op(); err != nil {
		return err
	}
	end := min(off+n, int64(len(s.cur)))
	if off < end {
		clear(s.cur[off:end])
		s.markDirty(off, end-off)
	}
	return nil
}

func (s *crashStorage) Truncate(n int64) error {
	if err := s.op(); err != nil {
		return err
	}
	if n < int64(len(s.cur)) {
		s.cur = s.cur[:n]
	} else {
		s.cur = append(s.cur, make([]byte, n-int64(len(s.cur)))...)
	}
	return nil
}

func (s *crashStorage) Sync() error {
	if err := s.op(); err != nil {
		return err
	}
	s.syncs++
	if len(s.durable) > len(s.cur) {
		s.durable = s.durable[:len(s.cur)]
	} else {
		s.durable = append(s.durable, make([]byte, len(s.cur)-len(s.durable))...)
	}
	for sec := range s.dirty {
		off := sec * sectorSize
		if off < int64(len(s.cur)) {
			copy(s.durable[off:min(off+sectorSize, int64(len(s.cur)))], s.cur[off:])
		}
	}
	clear(s.dirty)
	return nil
}

func (s *crashStorage) size() (int64, error) { return int64(len(s.cur)), nil }
func (s *crashStorage) Close() error         { return nil }

// powerLoss returns what the disk may hold after a power failure: the
// synced state plus an arbitrary subset of the unsynced sectors.
func (s *crashStorage) powerLoss(rng *rand.Rand) []byte {
	d := bytes.Clone(s.durable)
	if rng.IntN(2) == 0 {
		// The file size change made it to disk.
		if len(s.cur) < len(d) {
			d = d[:len(s.cur)]
		} else {
			d = append(d, make([]byte, len(s.cur)-len(d))...)
		}
	}
	for sec := range s.dirty {
		off := sec * sectorSize
		if off >= int64(len(s.cur)) || rng.IntN(2) == 0 {
			continue
		}
		end := min(off+sectorSize, int64(len(s.cur)))
		if end > int64(len(d)) {
			d = append(d, make([]byte, end-int64(len(d)))...)
		}
		copy(d[off:end], s.cur[off:end])
	}
	return d
}

// crashRecorder tracks what must survive a crash: the guest contents at
// the last successful flush and the ranges touched since then.
type crashRecorder struct {
	// bitmaps maps each bitmap to the contents it may have after a crash
	// unless it is flagged in use.
	bitmaps map[string][][]Extent
	initial []byte // guest contents before the workload

	shadow       []byte
	flushed      []byte
	sizes        []int64 // acceptable virtual sizes after a crash
	touched      [][2]int64
	flushedCount int
}

func (r *crashRecorder) touch(off, end int64) { r.touched = append(r.touched, [2]int64{off, end}) }

func (r *crashRecorder) flushedOK() {
	r.flushed = bytes.Clone(r.shadow)
	r.sizes = []int64{int64(len(r.shadow))}
	r.touched = nil
	r.flushedCount++
}

// runCrashWorkload runs a deterministic sequence of writes, discards,
// flushes and one resize, stopping at the first error.
func runCrashWorkload(img *Image, seed uint64, rec *crashRecorder) error {
	rng := rand.New(rand.NewPCG(seed, 99))
	cs := img.ClusterSize()
	for step := range 100 {
		size := int64(len(rec.shadow))
		switch k := rng.IntN(20); {
		case step == 50:
			newSize := size + 2<<20
			rec.touch(size, newSize)
			rec.sizes = append(rec.sizes, newSize)
			if err := img.Resize(newSize); err != nil {
				return err
			}
			rec.shadow = append(rec.shadow, make([]byte, newSize-size)...)
		case k < 12:
			l := 1 + rng.Int64N(128<<10)
			off := rng.Int64N(size - l)
			data := make([]byte, l)
			for i := range data {
				data[i] = byte(rng.Uint32())
			}
			rec.touch(off, off+l)
			if _, err := img.WriteAt(data, off); err != nil {
				return err
			}
			copy(rec.shadow[off:], data)
		case k < 17:
			l := 1 + rng.Int64N(16*cs)
			off := rng.Int64N(size - l)
			rec.touch(off, off+l)
			if err := img.Discard(off, l); err != nil {
				return err
			}
			clear(rec.shadow[off : off+l])
		default:
			if err := img.Flush(); err != nil {
				return err
			}
			rec.flushedOK()
		}
	}
	if err := img.Close(); err != nil {
		return err
	}
	rec.flushedOK()
	return nil
}

type crashScenario struct {
	name string
	// setup creates the initial image in dir and returns its path.
	setup func(t *testing.T, dir string) string
}

var crashScenarios = []crashScenario{
	{"compressed-snapshot", func(t *testing.T, dir string) string {
		raw := filepath.Join(dir, "src.raw")
		compressibleRaw(t, raw, 4<<20)
		path := filepath.Join(dir, "img.qcow2")
		run(t, "qemu-img", "convert", "-c", "-f", "raw", "-O", "qcow2", "-o", "cluster_size=4096", raw, path)
		run(t, "qemu-img", "snapshot", "-c", "snap", path)
		return path
	}},
	{"overlay-512-rb64", func(t *testing.T, dir string) string {
		compressibleRaw(t, filepath.Join(dir, "base.raw"), 4<<20)
		path := filepath.Join(dir, "img.qcow2")
		img, err := Create(path, CreateOptions{Size: 3 << 20, ClusterSize: 512, RefcountBits: 64, BackingFile: "base.raw"})
		if err != nil {
			t.Fatal(err)
		}
		if err := img.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}},
	{"bitmaps", func(t *testing.T, dir string) string {
		path := filepath.Join(dir, "img.qcow2")
		run(t, "qemu-img", "create", "-q", "-f", "qcow2", "-o", "cluster_size=4096", path, "4M")
		run(t, "qemu-img", "bitmap", "--add", "-g", "4096", path, "enabled")
		run(t, "qemu-img", "bitmap", "--add", "-g", "65536", path, "disabled")
		// Fully allocated, so that guest writes overwrite in place, with a
		// clean enabled bitmap, so that any unrecorded change shows.
		run(t, "qemu-io", "-c", "write -P 0x21 0 4M", path)
		run(t, "qemu-img", "bitmap", "--disable", path, "disabled")
		run(t, "qemu-img", "bitmap", "--clear", path, "enabled")
		return path
	}},
	{"v2-snapshot", func(t *testing.T, dir string) string {
		path := filepath.Join(dir, "img.qcow2")
		run(t, "qemu-img", "create", "-q", "-f", "qcow2", "-o", "compat=0.10,cluster_size=1024", path, "4M")
		run(t, "qemu-io", "-c", "write -P 0x3c 0 3M", path)
		run(t, "qemu-img", "snapshot", "-c", "snap", path)
		return path
	}},
}

// TestCrashConsistency crashes a workload at every storage operation
// performed while flushing metadata and at a sample of the others, both as
// a process crash (all completed writes survive) and as a power loss
// (an arbitrary subset of the unsynced sectors survives).
func TestCrashConsistency(t *testing.T) {
	requireQemu(t)
	for _, sc := range crashScenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := sc.setup(t, dir)
			initial, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			guest := qemuRaw(t, path)
			initialBitmaps := bitmapContents(t, path)
			var allowedBitmaps map[string][][]Extent
			newRecorder := func() *crashRecorder {
				rec := &crashRecorder{shadow: bytes.Clone(guest), bitmaps: allowedBitmaps, initial: guest}
				rec.flushedOK()
				return rec
			}

			// A full run tells which operations exist.
			st := newCrashStorage(initial, 0)
			img, err := openStorage(st, path, false, Options{}, 0)
			if err != nil {
				t.Fatal(err)
			}
			rec := newRecorder()
			if err := runCrashWorkload(img, 1, rec); err != nil {
				t.Fatal(err)
			}
			// Bitmaps hold their initial contents until Close stores the
			// final ones.
			full := filepath.Join(dir, "full.qcow2")
			if err := os.WriteFile(full, st.cur, 0o644); err != nil {
				t.Fatal(err)
			}
			finalBitmaps := bitmapContents(t, full)
			allowedBitmaps = map[string][][]Extent{}
			for name, exts := range initialBitmaps {
				allowedBitmaps[name] = [][]Extent{exts, finalBitmaps[name]}
			}
			if len(initialBitmaps) > 0 {
				t.Logf("bitmaps: initial %v, final %v", initialBitmaps, finalBitmaps)
			}
			rec.bitmaps = map[string][][]Extent{}
			for name, exts := range finalBitmaps {
				rec.bitmaps[name] = [][]Extent{exts}
			}
			verifyCrashImage(t, dir, "full", st.cur, rec, "complete run")

			points := map[int]bool{}
			flushOps := st.flushOps
			if testing.Short() {
				flushOps = flushOps[:0:0]
				for i := 0; i < len(st.flushOps); i += 20 {
					flushOps = append(flushOps, st.flushOps[i])
				}
			}
			for _, op := range flushOps {
				points[op] = true
			}
			// The operations right after a metadata update, e.g. the first
			// guest write after flagging bitmaps in use.
			inFlush := map[int]bool{}
			for _, op := range st.flushOps {
				inFlush[op] = true
			}
			for _, op := range st.flushOps {
				if !inFlush[op+1] {
					for d := 2; d <= 4 && op+d <= st.ops; d++ {
						points[op+d] = true
					}
				}
			}
			rng := rand.New(rand.NewPCG(5, 5))
			for range 40 {
				points[1+rng.IntN(st.ops)] = true
			}
			t.Logf("%d storage operations, %d while flushing, %d flushes; crashing at %d points",
				st.ops, len(st.flushOps), rec.flushedCount, len(points))

			work := make(chan int)
			errs := make(chan error, runtime.GOMAXPROCS(0))
			for w := range runtime.GOMAXPROCS(0) {
				go func() {
					var err error
					for crashAt := range work {
						if err == nil {
							crashSlots <- struct{}{}
							err = crashAndVerify(t, dir, fmt.Sprint("w", w), initial, path, crashAt, newRecorder)
							<-crashSlots
						}
					}
					errs <- err
				}()
			}
			for p := range points {
				work <- p
			}
			close(work)
			for range runtime.GOMAXPROCS(0) {
				if err := <-errs; err != nil {
					t.Error(err)
				}
			}
			t.Logf("leaks repaired in %d crashed images so far", repairedImages.Load())
		})
	}
}

func crashAndVerify(t *testing.T, dir, worker string, initial []byte, path string, crashAt int, newRecorder func() *crashRecorder) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("crash at op %d: %v", crashAt, r)
		}
	}()
	st := newCrashStorage(initial, crashAt)
	img, err := openStorage(st, path, false, Options{}, 0)
	if err != nil {
		return err
	}
	rec := newRecorder()
	if err := runCrashWorkload(img, 1, rec); !errors.Is(err, errCrash) {
		return fmt.Errorf("crash at %d: workload returned %v", crashAt, err)
	}
	rng := rand.New(rand.NewPCG(uint64(crashAt), 7))
	images := [][]byte{st.cur, st.powerLoss(rng), st.powerLoss(rng)}
	for i, data := range images {
		what := fmt.Sprintf("crash at op %d (%s)", crashAt, []string{"process", "power loss 1", "power loss 2"}[i])
		if err := checkCrashImage(dir, worker, data, rec, what); err != nil {
			return err
		}
	}
	return nil
}

// verifyCrashImage is checkCrashImage for the test goroutine.
func verifyCrashImage(t *testing.T, dir, worker string, data []byte, rec *crashRecorder, what string) {
	t.Helper()
	if err := checkCrashImage(dir, worker, data, rec, what); err != nil {
		t.Fatal(err)
	}
}

// checkCrashImage checks that a crashed image is consistent according to
// qemu-img check (leaks are fine) and holds the data of the last flush
// outside the ranges touched since.
func checkCrashImage(dir, worker string, data []byte, rec *crashRecorder, what string) error {
	path := filepath.Join(dir, "crashed-"+worker+".qcow2")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	qemuLeaks, err := qemuLeakCount(path)
	if err != nil {
		return fmt.Errorf("%s: %v", what, err)
	}
	got, err := qemuRawErr(path)
	if err != nil {
		return fmt.Errorf("%s: %v", what, err)
	}
	if err := checkAndRepair(path, qemuLeaks, got); err != nil {
		return fmt.Errorf("%s: %v", what, err)
	}
	img, err := Open(path)
	if err != nil {
		return fmt.Errorf("%s: %v", what, err)
	}
	ours := make([]byte, img.Size())
	_, err = img.ReadAt(ours, 0)
	err = errors.Join(err, img.Close())
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %v", what, err)
	}
	if !bytes.Equal(ours, got) {
		return fmt.Errorf("%s: qcow2 package and qemu disagree", what)
	}
	if err := checkCrashBitmaps(path, rec.bitmaps, rec.initial, got); err != nil {
		return fmt.Errorf("%s: %v", what, err)
	}

	sizeOK := false
	for _, s := range rec.sizes {
		sizeOK = sizeOK || int64(len(got)) == s
	}
	if !sizeOK {
		return fmt.Errorf("%s: virtual size %d, want one of %v", what, len(got), rec.sizes)
	}
	touched := make([]bool, max(len(got), len(rec.flushed)))
	for _, r := range rec.touched {
		for i := r[0]; i < r[1] && i < int64(len(touched)); i++ {
			touched[i] = true
		}
	}
	for i := range min(len(got), len(rec.flushed)) {
		if !touched[i] && got[i] != rec.flushed[i] {
			return fmt.Errorf("%s: flushed data lost at offset %d: got %#02x want %#02x", what, i, got[i], rec.flushed[i])
		}
	}
	return nil
}

// qemuLeakCount runs qemu-img check and returns the number of leaked
// clusters; any other problem is an error.
func qemuLeakCount(path string) (int64, error) {
	out, err := exec.Command("qemu-img", "check", path).CombinedOutput()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	// Exit status 3: only leaked clusters.
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		return 0, fmt.Errorf("qemu-img check: %v\n%s", err, out)
	}
	var n int64
	for _, line := range bytes.Split(out, []byte("\n")) {
		if _, err := fmt.Sscanf(string(line), "%d leaked clusters were found on the image.", &n); err == nil {
			return n, nil
		}
	}
	return 0, fmt.Errorf("no leak count in qemu-img check output:\n%s", out)
}

func qemuRawErr(path string) ([]byte, error) {
	raw := path + ".raw"
	// Best effort: the file is missing when the conversion failed.
	defer func() { _ = os.Remove(raw) }()
	if out, err := exec.Command("qemu-img", "convert", "-O", "raw", path, raw).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("qemu-img convert: %v\n%s", err, out)
	}
	return os.ReadFile(raw)
}

// checkAndRepair verifies that Check agrees with qemu-img check, then
// repairs the leaks and verifies that qemu-img check is clean and the
// guest data unchanged.
func checkAndRepair(path string, qemuLeaks int64, data []byte) (err error) {
	img, err := OpenFile(path, os.O_RDWR, nil)
	if err != nil {
		return err
	}
	defer closeInto(img, &err)
	res, err := img.Check()
	if err != nil {
		return err
	}
	if res.Leaks != qemuLeaks || res.Corruptions != 0 {
		return fmt.Errorf("Check: %d leaks, %d corruptions %v; qemu-img check: %d leaks", res.Leaks, res.Corruptions, res.Errors, qemuLeaks)
	}
	if res.Leaks+res.LeaksPastEOF == 0 {
		return nil
	}
	repairedImages.Add(1)
	n, err := img.RepairLeaks()
	if err != nil {
		return err
	}
	if n != res.Leaks+res.LeaksPastEOF {
		return fmt.Errorf("RepairLeaks repaired %d clusters, want %d", n, res.Leaks+res.LeaksPastEOF)
	}
	if err := img.Close(); err != nil {
		return err
	}
	if out, err := exec.Command("qemu-img", "check", path).CombinedOutput(); err != nil {
		return fmt.Errorf("after RepairLeaks: qemu-img check: %v\n%s", err, out)
	}
	after, err := qemuRawErr(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(after, data) {
		return errors.New("RepairLeaks changed guest data")
	}
	return nil
}

// bitmapContents returns the dirty extents of every bitmap of an image.
func bitmapContents(t *testing.T, path string) map[string][]Extent {
	t.Helper()
	img, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeImage(t, img)
	m := map[string][]Extent{}
	for _, b := range img.Bitmaps() {
		m[b.Name] = dirtyExtents(t, img, b.Name)
	}
	return m
}

// checkCrashBitmaps verifies that every bitmap of a crashed image is either
// flagged in use or holds one of its allowed contents, and that an enabled
// bitmap not in use marks every byte in which the guest disk differs from
// its initial contents (all of a grown range counts as changed). Only
// enabled bitmaps may be in use: guest writes never touch the others.
func checkCrashBitmaps(path string, allowed map[string][][]Extent, initial, got []byte) (err error) {
	img, err := Open(path)
	if err != nil {
		return err
	}
	defer closeInto(img, &err)
	infos := img.Bitmaps()
	if len(infos) != len(allowed) {
		return fmt.Errorf("%d bitmaps, want %d", len(infos), len(allowed))
	}
	for _, b := range infos {
		if b.InUse {
			if !b.Enabled {
				return fmt.Errorf("disabled bitmap %q in use", b.Name)
			}
			continue
		}
		exts, err := img.DirtyExtents(b.Name)
		if err != nil {
			return err
		}
		ok := false
		for _, want := range allowed[b.Name] {
			ok = ok || slices.Equal(exts, want)
		}
		if !ok {
			return fmt.Errorf("bitmap %q holds %v, want one of %v", b.Name, exts, allowed[b.Name])
		}
		if !b.Enabled {
			continue
		}
		dirty := make([]bool, len(got))
		for _, e := range exts {
			for i := e.Offset; i < e.Offset+e.Length && i < int64(len(dirty)); i++ {
				dirty[i] = true
			}
		}
		for i := range got {
			if (i >= len(initial) || got[i] != initial[i]) && !dirty[i] {
				return fmt.Errorf("bitmap %q misses a change at offset %d", b.Name, i)
			}
		}
	}
	return nil
}
