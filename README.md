# go-qcow2

Pure Go library for reading and writing QEMU qcow2 disk images, including backing files.

```go
import "github.com/takumin/go-qcow2"
```

## Features

- qcow2 v2 / v3, cluster sizes 512 B – 2 MiB, all refcount widths (1–64 bits)
- zlib / zstd compressed clusters (e.g. Ubuntu cloud images)
- zero clusters
- backing files (qcow2 or raw) and backing chains, relative names resolved like QEMU
- creating images, growing images, changing the backing file (`qemu-img rebase -u`)
- writes keep internal snapshots intact (copy-on-write for shared clusters)
- clusters freed by copy-on-write (or by deleting snapshots) are reused for new allocations,
  and on Linux their space is released with `fallocate(PUNCH_HOLE)` (disable with `NoPunchHole`)
- `Discard` (TRIM/UNMAP): the range reads as zeros and its clusters are released
- persistent dirty bitmaps: maintained on writes, listed, read as dirty extents,
  added, removed, cleared, enabled and disabled
- crash-consistent metadata updates, `Check` and `RepairLeaks`
- `*qcow2.Image` implements `io.ReaderAt` and `io.WriterAt`

Not supported: encryption, external data files, extended L2 entries (subclusters),
shrinking.

## Usage

Read a cloud image:

```go
img, err := qcow2.Open("ubuntu-26.04-server-cloudimg-amd64.img")
if err != nil {
	log.Fatal(err)
}
defer img.Close()

// Convert to raw.
out, _ := os.Create("disk.raw")
defer out.Close()
io.Copy(out, io.NewSectionReader(img, 0, img.Size()))
```

Create an overlay backed by the cloud image, grow it and write to it:

```go
img, err := qcow2.Create("vm.qcow2", qcow2.CreateOptions{
	BackingFile:   "ubuntu-26.04-server-cloudimg-amd64.img", // relative to vm.qcow2
	BackingFormat: "qcow2",
})
if err != nil {
	log.Fatal(err)
}
defer img.Close()

if err := img.Resize(20 << 30); err != nil {
	log.Fatal(err)
}
if _, err := img.WriteAt(data, offset); err != nil {
	log.Fatal(err)
}
```

Discard a range (like TRIM from the guest); it reads as zeros afterwards
and the clusters are released:

```go
err := img.Discard(offset, length)
```

Modify an existing image in place:

```go
img, err := qcow2.OpenFile("disk.qcow2", os.O_RDWR, nil)
```

## Dirty bitmaps

Persistent dirty bitmaps (as created by `qemu-img bitmap` or QEMU's
`block-dirty-bitmap-add`) are kept up to date: enabled bitmaps record every
range changed by `WriteAt`, `Discard` and `Resize`, and are stored on
`Close`. They can be used for incremental backups:

```go
for _, b := range img.Bitmaps() {
	fmt.Println(b.Name, b.Granularity, b.Enabled, b.InUse)
}
exts, err := img.DirtyExtents("backup-1") // ranges changed since the bitmap was cleared
err = img.AddBitmap("backup-2", 0, true)  // granularity 0: cluster size, at least 4 KiB
err = img.ClearBitmap("backup-1")
err = img.SetBitmapEnabled("backup-1", false)
err = img.RemoveBitmap("backup-1")
```

As in QEMU, enabled bitmaps are flagged "in use" on disk right before the
first change to the guest disk and the flag is cleared when `Close` stores
them. If the process dies before that, they stay flagged (`InUse`), their
contents can no longer be trusted and they can only be removed. Opening an
image writable without changing it keeps its bitmaps valid. Storing never
leaves a malformed bitmap: new tables and a new directory are written before
the header switches to them.

## Durability and crash consistency

Metadata updates are buffered in memory and written by `Flush` (also called
by `Close`) in dependency order — data and refcount blocks, refcount table,
L2 tables, L1 table, header — with a sync only where a later step depends
on an earlier one, like QEMU's writeback cache mode. On Linux the syncs use
`fdatasync`. A flush of in-place overwrites costs one sync, a flush after
allocating clusters two (three when a new L2 table was needed). Clusters
freed by a flush are released only after the change that dropped their last
reference is on disk, and the refcount decrement itself rides along with
the next flush.

If the process or the machine crashes at any point, the image is never
corrupt: it holds everything written before the last successful `Flush`
(ranges written afterwards may hold old or new data), and at worst some
clusters are leaked. Call `Flush` when writes must be durable, or set
`Options.WriteThrough` to flush after every call.

`Close` additionally truncates unreferenced clusters from the end of the
file.

### Checking and repairing leaks

```go
res, err := img.Check()          // like qemu-img check
n, err := img.RepairLeaks()      // like qemu-img check -r leaks
```

`Check` recomputes every refcount from the metadata (L1/L2 tables, refcount
structures, internal snapshots, persistent bitmaps) and reports leaks and
corruptions, including wrong COPIED flags. `RepairLeaks` lowers leaked
refcounts so the clusters get reused or punched out; it refuses to touch an
image with corruptions.

## Development

Tools (Go, just, linters) are pinned with [aqua](https://aquaproj.github.io/)
in `.aqua/`; recipes live in `Justfile` and `.just/`. CI runs the same recipes.

```sh
just                 # tools, format, lint, test, build
just format          # go mod tidy, gofmt, yamlfmt, pinact, just --fmt
just reviewdog       # golangci-lint, gosec, editorconfig-checker, yamllint, biome,
                     # jactionlint, ghalint, zizmor, Renovate config schema
just test            # go test -race -short (needs qemu-img, qemu-io, qemu-nbd)
just test crash      # full crash-consistency simulation
just coverage        # coverage report in coverage/
just audit           # govulncheck
just build           # compile for linux/darwin/windows × amd64/arm64
```

## Testing

Tests use `qemu-img` and `qemu-io` as the reference implementation (they are
skipped when not installed). Every written image is validated with
`qemu-img check` and compared byte by byte with `qemu-img convert -O raw`.

```sh
go test ./...

# Additionally exercise a real image (read, overlay + resize, in-place writes):
QCOW2_TEST_IMAGE=/path/to/ubuntu-26.04-server-cloudimg-amd64.img go test -run TestRealImage -v
```

`TestCrashConsistency` simulates crashes at every storage operation performed
while flushing (and at a sample of the others), both as a process crash and as
a power loss that keeps an arbitrary subset of the unsynced sectors, and checks
every resulting image with `qemu-img check` and against the last flushed data.
It also verifies that `Check` reports the same leaks as `qemu-img check` and
that `RepairLeaks` leaves an image `qemu-img check` finds clean.

`BenchmarkFlush` measures a 4 KiB write plus `Flush`; point
`QCOW2_BENCH_DIR` at a real disk when `/tmp` is a tmpfs.
