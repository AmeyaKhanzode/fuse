# Mini-UnionFS — Design Document

## 1. Overview

Mini-UnionFS is a userspace union filesystem that merges two directories —
a read-only `lower_dir` (base image) and a read-write `upper_dir` (container
layer) — into a single unified view at `mount_dir`. It is implemented in Go
on top of the FUSE kernel protocol via `github.com/hanwen/go-fuse/v2`.

The design mirrors the semantics of Docker's OverlayFS: reads fall through
from upper to lower, writes are captured in upper via Copy-on-Write, and
deletions are recorded as zero-byte *whiteout* markers (`.wh.<name>`) that
mask the underlying lower entry without mutating it.

```
user process
     │   read()/write()/unlink()/…
     ▼
┌──────────────┐
│  FUSE kernel │
└──────┬───────┘
       │ /dev/fuse
       ▼
┌──────────────────────────────┐
│   mini_unionfs (userspace)   │
│   upper (RW)  +  lower (RO)  │
└──────────────────────────────┘
```

## 2. Data Structures

### 2.1 `unionFS`
Global, immutable after startup. Holds the absolute paths of the two
branches. Every `unionNode` carries a pointer to the single shared instance.

```go
type unionFS struct {
    lowerDir string
    upperDir string
}
```

### 2.2 `unionNode`
The `InodeEmbedder` behind every file and directory in the mounted
namespace. It stores only the *virtual path* (relative to the mount root),
not a branch — resolution happens on every operation so that CoW
transparently moves a node from lower to upper.

```go
type unionNode struct {
    fs.Inode
    ufs  *unionFS
    path string  // e.g. "a/b/file.txt"
}
```

Stable inode numbers are derived as `fnv64a(path)`. This keeps the
kernel's dentry cache coherent even after a file migrates from lower to
upper, because the `StableAttr.Ino` is a property of the *virtual* path,
not the physical one.

### 2.3 `unionFile`
The per-open file handle. Holds a single OS fd into the resolved physical
file. By the time `unionFile` is constructed, `Open`/`Create` has already
guaranteed that the fd points into `upper_dir` whenever the open mode is
writable.

## 3. Path Resolution Algorithm

`resolve(rel)` is the heart of the system. It implements the precedence
rules required by the spec:

```
resolve(rel):
    if exists(upper/.wh.<base(rel)>):     return ENOENT   # whiteouted
    if exists(upper/rel):                 return (upper/rel, inUpper=true)
    if exists(lower/rel):                 return (lower/rel, inUpper=false)
    return ENOENT
```

The whiteout check happens first because a lower-only file that has been
deleted must appear gone even if it is still physically present in lower.

`readdir` performs the directory-level equivalent: it walks the upper
directory first, collecting both real entries and `.wh.*` markers, then
walks the lower directory and skips any entry that was masked or already
contributed by upper. Whiteout markers themselves are never returned to the
user.

## 4. Copy-on-Write

CoW is triggered at `Open` time — not at `Write` time — because by the
point the kernel asks us to write, the userspace fd has already been
bound. The sequence is:

1. Resolve the virtual path.
2. Inspect the requested access mode (`O_WRONLY` or `O_RDWR` ⇒ write).
3. If writing and the resolved path is in lower, call `copyUp(rel)`:
   - `mkdir -p $(dirname upper/rel)`
   - Copy `lower/rel` bytes into `upper/rel`, preserving mode.
4. `syscall.Open` the resolved upper path and wrap it in a `unionFile`.

A file opened read-only stays in lower. A file that is *already* in upper
skips the copy. This matches the "Test 2" expectation that after a write
the file's new contents exist in both the mount view and `upper_dir` but
`lower_dir` is untouched.

## 5. Whiteout (Deletion) Mechanism

`Unlink` and `Rmdir` use the same two-step rule:

| State                          | Action                                                             |
|--------------------------------|--------------------------------------------------------------------|
| In upper only                  | Physical `os.Remove(upper/rel)`.                                    |
| In lower only                  | Create zero-byte `upper/dirname/.wh.basename`.                     |
| In both (i.e. already CoW'd)   | Remove `upper/rel` *and* create the whiteout, because lower is still there. |
| Already whiteouted             | Return `ENOENT`.                                                   |

For `Rmdir`, an additional merged-view emptiness check runs first
(`dirEmptyMerged`): a directory counts as non-empty if *any* visible child
remains after applying whiteouts, so that `rmdir` on a directory whose
children live in lower is correctly rejected with `ENOTEMPTY` unless every
child has itself been whiteouted.

Creating a file or directory (`Create`, `Mkdir`) with the same name as a
whiteouted entry first removes the whiteout. This preserves the
"resurrection" semantics users expect: `rm foo && touch foo` leaves `foo`
visible again, backed by the upper layer.

## 6. FUSE Operation Map

| POSIX op | Implemented by              | Notes                                      |
|----------|-----------------------------|--------------------------------------------|
| getattr  | `unionNode.Getattr`          | Uses `fstat` on a live fd when available.  |
| readdir  | `unionNode.Readdir`          | Merges upper + lower, applies whiteouts.   |
| lookup   | `unionNode.Lookup`           | Rejects names starting with `.wh.`.        |
| open     | `unionNode.Open`             | Triggers CoW for writable opens.           |
| create   | `unionNode.Create`           | Writes straight into upper; drops whiteout.|
| read     | `unionFile.Read`             | `fuse.ReadResultFd` — zero-copy.           |
| write    | `unionFile.Write`            | `pwrite` on the upper-layer fd.            |
| unlink   | `unionNode.Unlink`           | Whiteout rules above.                      |
| mkdir    | `unionNode.Mkdir`            | Creates in upper; drops whiteout.          |
| rmdir    | `unionNode.Rmdir`            | Merged emptiness check + whiteout.         |
| flush    | `unionFile.Flush`            | `dup`+`close` to push writes to kernel.    |
| fsync    | `unionFile.Fsync`            | `fsync` on the fd.                         |
| release  | `unionFile.Release`          | Closes the fd.                             |

## 7. Edge Cases & Decisions

- **Hidden whiteouts.** Names beginning with `.wh.` are never surfaced by
  `Lookup` or `Readdir`; callers that try to `Create` one get `EINVAL`.
  The filesystem reserves the namespace for itself.
- **Root must always exist.** `Getattr("")` falls back to stat-ing the
  upper directory itself if both branches fail to resolve, so the mount
  point never appears broken.
- **Cache coherence across CoW.** Using `fnv64a(path)` for `StableAttr.Ino`
  keeps the same logical identity across a lower→upper migration. Relying
  on the physical inode number would change after `copyUp`, invalidating
  entries the kernel still held open.
- **Non-empty directory deletion.** The merged-view emptiness check
  prevents `rmdir` from succeeding when upper is empty but lower still
  contributes children. Without this, a subsequent `readdir` would show
  "ghost" children of a directory the user believes they deleted.
- **Concurrent writers to one open fd.** Each `unionFile` serialises its
  `Read`/`Write`/`Release`/`Flush` through a `sync.Mutex`. FUSE may
  dispatch these from multiple kernel threads.
- **Flush semantics.** `Flush` uses `dup`+`close` instead of `fsync`
  because POSIX `close(2)` is what the application expects to push
  pending writes; `fsync` is reserved for explicit `fsync(2)` calls.
- **XAttrs / symlinks / rename.** Explicitly out of scope. XAttrs are
  disabled via `DisableXAttrs`. The spec's Basic POSIX Operations list
  does not include them.

## 8. Build, Run, Test

```
make             # builds ./mini_unionfs
make test        # runs the test suite from Appendix B (extended)

# Manual:
./mini_unionfs  ./lower  ./upper  ./mnt     # foreground
fusermount -u ./mnt                         # unmount
```

The test suite covers the three required scenarios (visibility, CoW,
whiteout) plus three extensions: merged `readdir` with whiteouts applied,
round-trip `create → write → read` into upper, and `mkdir`/`rmdir` within
upper.

## 9. Limitations

Deliberate simplifications to keep the project focused:

- Single lower layer (not an N-deep stack).
- No opaque-directory markers: whiting out a lower-only directory that
  contains entries works via the merged emptiness check, but the spec's
  OverlayFS-style "opaque directory" for "rm -rf then mkdir" reuse of a
  name isn't implemented. It falls out naturally as long as the new
  directory is created in upper after the whiteout is dropped.
- No `rename`, `symlink`, `link`, `chmod`, `chown`, `truncate`, or xattr
  operations — outside the required POSIX subset.
