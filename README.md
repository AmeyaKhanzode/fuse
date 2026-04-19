# Mini-UnionFS

A simplified userspace Union File System in Go, using FUSE. Stacks a
read-only `lower_dir` (base image) under a read-write `upper_dir`
(container layer) with Copy-on-Write and `.wh.` whiteout semantics.

## Requirements

- Linux (Ubuntu 22.04+ recommended) with FUSE 3
- Go 1.21+
- `sudo apt install fuse3 libfuse3-dev` (for `fusermount3`)

## Build

```
make
```

## Run

```
./mini_unionfs <lower_dir> <upper_dir> <mount_dir>
```

The process stays in the foreground; unmount with `fusermount -u <mount_dir>`
or `Ctrl-C`.

Add `-debug` to see the FUSE kernel protocol traffic.

## Test

```
make test
```

Runs the scripted suite (visibility, CoW, whiteout, merged readdir,
create/write/read, mkdir/rmdir).

## Layout

- `main.go` — FUSE implementation (nodes, file handles, CoW, whiteouts)
- `test_unionfs.sh` — automated test suite
- `Makefile` — build/test/clean
- `DESIGN.md` — 2-3 page design document
