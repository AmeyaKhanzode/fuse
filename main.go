// Mini-UnionFS: a simple userspace union filesystem using FUSE.
//
// Usage: mini_unionfs <lower_dir> <upper_dir> <mount_dir>
//
//   lower_dir : read-only base layer
//   upper_dir : read-write top layer (receives CoW copies and whiteouts)
//   mount_dir : where the merged view is mounted
package main

import (
	"context"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// whPrefix marks deletions of lower-layer entries. Example: deleting
// "config.txt" creates ".wh.config.txt" in the upper layer.
const whPrefix = ".wh."

// unionFS owns the underlying branches. It is shared by every node.
//
// lowerDirs is ordered by decreasing priority: lowerDirs[0] is the topmost
// lower layer (checked first after upper), lowerDirs[len-1] is the deepest
// base. upperDir is always the sole read-write layer and wins over every
// lower.
type unionFS struct {
	lowerDirs []string
	upperDir  string
}

func (u *unionFS) upperPath(rel string) string { return filepath.Join(u.upperDir, rel) }

// findInLowers walks the lower stack top-to-bottom and returns the first
// layer path that contains rel. (path, true) on hit; ("", false) otherwise.
func (u *unionFS) findInLowers(rel string) (string, bool) {
	for _, d := range u.lowerDirs {
		p := filepath.Join(d, rel)
		if _, err := os.Lstat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// whPathFor returns the whiteout path that would shadow rel in the upper dir.
func (u *unionFS) whPathFor(rel string) string {
	dir, base := filepath.Split(rel)
	return filepath.Join(u.upperDir, dir, whPrefix+base)
}

// isWhiteouted reports whether rel has been marked deleted in upper.
func (u *unionFS) isWhiteouted(rel string) bool {
	if rel == "" || rel == "/" {
		return false
	}
	_, err := os.Lstat(u.whPathFor(rel))
	return err == nil
}

// resolve picks the effective physical path for rel: upper wins unless
// whiteouted; otherwise fall back to the first lower that has it.
// Returns (path, inUpper, err).
func (u *unionFS) resolve(rel string) (string, bool, error) {
	if u.isWhiteouted(rel) {
		return "", false, syscall.ENOENT
	}
	up := u.upperPath(rel)
	if _, err := os.Lstat(up); err == nil {
		return up, true, nil
	}
	if lp, ok := u.findInLowers(rel); ok {
		return lp, false, nil
	}
	return "", false, syscall.ENOENT
}

// copyUp performs Copy-on-Write: finds rel in the first lower that has it
// and copies it into the upper branch, creating parent directories as
// needed. No-op if already present in upper.
func (u *unionFS) copyUp(rel string) error {
	up := u.upperPath(rel)
	if _, err := os.Lstat(up); err == nil {
		return nil
	}
	lp, ok := u.findInLowers(rel)
	if !ok {
		return syscall.ENOENT
	}
	srcInfo, err := os.Stat(lp)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(up), 0o755); err != nil {
		return err
	}
	src, err := os.Open(lp)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(up, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, srcInfo.Mode().Perm())
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return nil
}

// stableIno derives a deterministic inode number from a virtual path so that
// repeated Lookups return the same StableAttr even when the file migrates
// between lower and upper (e.g. after CoW).
func stableIno(path string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(path))
	n := h.Sum64()
	if n == 0 {
		return 1
	}
	return n
}

// unionNode is the InodeEmbedder for every file and directory in the union.
// It stores the virtual path (relative to the mount root) so each op can
// re-resolve against lower/upper without walking the inode tree.
type unionNode struct {
	fs.Inode
	ufs  *unionFS
	path string
}

func (n *unionNode) child(name string) string {
	if n.path == "" {
		return name
	}
	return filepath.Join(n.path, name)
}

// Compile-time interface assertions — fail fast if the FUSE surface we
// think we implement drifts.
var (
	_ fs.NodeGetattrer = (*unionNode)(nil)
	_ fs.NodeLookuper  = (*unionNode)(nil)
	_ fs.NodeReaddirer = (*unionNode)(nil)
	_ fs.NodeOpener    = (*unionNode)(nil)
	_ fs.NodeCreater   = (*unionNode)(nil)
	_ fs.NodeUnlinker  = (*unionNode)(nil)
	_ fs.NodeMkdirer   = (*unionNode)(nil)
	_ fs.NodeRmdirer   = (*unionNode)(nil)
)

func (n *unionNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	// If a live fd is attached, fstat it — cheaper and avoids TOCTOU.
	if f, ok := fh.(*unionFile); ok && f != nil {
		var st syscall.Stat_t
		if err := syscall.Fstat(f.fd, &st); err != nil {
			return fs.ToErrno(err)
		}
		out.FromStat(&st)
		return 0
	}
	path, _, err := n.ufs.resolve(n.path)
	if err != nil {
		// The mount root must always have attrs; fall back to upper itself.
		if n.path == "" {
			path = n.ufs.upperDir
		} else {
			return fs.ToErrno(err)
		}
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return fs.ToErrno(err)
	}
	out.FromStat(&st)
	return 0
}

func (n *unionNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	// Whiteouts are internal bookkeeping — never expose them.
	if strings.HasPrefix(name, whPrefix) {
		return nil, syscall.ENOENT
	}
	childPath := n.child(name)
	resolved, _, err := n.ufs.resolve(childPath)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(resolved, &st); err != nil {
		return nil, fs.ToErrno(err)
	}
	out.Attr.FromStat(&st)
	child := &unionNode{ufs: n.ufs, path: childPath}
	stable := fs.StableAttr{
		Mode: uint32(st.Mode) & syscall.S_IFMT,
		Ino:  stableIno(childPath),
	}
	return n.NewInode(ctx, child, stable), 0
}

func (n *unionNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	seen := map[string]bool{}
	whited := map[string]bool{}
	var entries []fuse.DirEntry

	// Upper first so it wins on name collisions; also collects whiteouts.
	if up, err := os.ReadDir(n.ufs.upperPath(n.path)); err == nil {
		for _, e := range up {
			name := e.Name()
			if strings.HasPrefix(name, whPrefix) {
				whited[strings.TrimPrefix(name, whPrefix)] = true
				continue
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			info, err := e.Info()
			if err != nil {
				continue
			}
			entries = append(entries, fuse.DirEntry{
				Name: name,
				Mode: uint32(info.Mode()),
				Ino:  stableIno(n.child(name)),
			})
		}
	}
	// Each lower, top-down, fills in anything not already contributed or
	// masked. Higher-priority lowers win over deeper ones on name collisions.
	for _, ld := range n.ufs.lowerDirs {
		lp, err := os.ReadDir(filepath.Join(ld, n.path))
		if err != nil {
			continue
		}
		for _, e := range lp {
			name := e.Name()
			if whited[name] || seen[name] {
				continue
			}
			if strings.HasPrefix(name, whPrefix) {
				continue
			}
			seen[name] = true
			info, err := e.Info()
			if err != nil {
				continue
			}
			entries = append(entries, fuse.DirEntry{
				Name: name,
				Mode: uint32(info.Mode()),
				Ino:  stableIno(n.child(name)),
			})
		}
	}
	return fs.NewListDirStream(entries), 0
}

func (n *unionNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	resolved, inUpper, err := n.ufs.resolve(n.path)
	if err != nil {
		return nil, 0, fs.ToErrno(err)
	}
	accMode := flags & uint32(syscall.O_ACCMODE)
	wantWrite := accMode == syscall.O_WRONLY || accMode == syscall.O_RDWR
	// CoW: first write against a lower-only file materialises it in upper.
	if wantWrite && !inUpper {
		if err := n.ufs.copyUp(n.path); err != nil {
			return nil, 0, fs.ToErrno(err)
		}
		resolved = n.ufs.upperPath(n.path)
	}
	fd, err := syscall.Open(resolved, int(flags), 0)
	if err != nil {
		return nil, 0, fs.ToErrno(err)
	}
	return &unionFile{fd: fd}, 0, 0
}

func (n *unionNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	if strings.HasPrefix(name, whPrefix) {
		return nil, nil, 0, syscall.EINVAL
	}
	childPath := n.child(name)
	upperPath := n.ufs.upperPath(childPath)
	// Resurrecting a previously-deleted lower entry: drop the whiteout.
	_ = os.Remove(n.ufs.whPathFor(childPath))
	if err := os.MkdirAll(filepath.Dir(upperPath), 0o755); err != nil {
		return nil, nil, 0, fs.ToErrno(err)
	}
	fd, err := syscall.Open(upperPath, int(flags)|syscall.O_CREAT, mode)
	if err != nil {
		return nil, nil, 0, fs.ToErrno(err)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return nil, nil, 0, fs.ToErrno(err)
	}
	out.Attr.FromStat(&st)
	child := &unionNode{ufs: n.ufs, path: childPath}
	stable := fs.StableAttr{
		Mode: uint32(st.Mode) & syscall.S_IFMT,
		Ino:  stableIno(childPath),
	}
	inode := n.NewInode(ctx, child, stable)
	return inode, &unionFile{fd: fd}, 0, 0
}

func (n *unionNode) Unlink(ctx context.Context, name string) syscall.Errno {
	if strings.HasPrefix(name, whPrefix) {
		return syscall.ENOENT
	}
	childPath := n.child(name)
	if n.ufs.isWhiteouted(childPath) {
		return syscall.ENOENT
	}
	upperP := n.ufs.upperPath(childPath)
	_, upErr := os.Lstat(upperP)
	_, inLower := n.ufs.findInLowers(childPath)
	if upErr != nil && !inLower {
		return syscall.ENOENT
	}
	if upErr == nil {
		if err := os.Remove(upperP); err != nil {
			return fs.ToErrno(err)
		}
	}
	// Lowers are immutable, so we shadow them with a whiteout marker instead.
	if inLower {
		if err := writeWhiteout(n.ufs.whPathFor(childPath)); err != nil {
			return fs.ToErrno(err)
		}
	}
	return 0
}

func (n *unionNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if strings.HasPrefix(name, whPrefix) {
		return nil, syscall.EINVAL
	}
	childPath := n.child(name)
	upperPath := n.ufs.upperPath(childPath)
	_ = os.Remove(n.ufs.whPathFor(childPath))
	if err := os.MkdirAll(filepath.Dir(upperPath), 0o755); err != nil {
		return nil, fs.ToErrno(err)
	}
	if err := os.Mkdir(upperPath, os.FileMode(mode)); err != nil {
		if !os.IsExist(err) {
			return nil, fs.ToErrno(err)
		}
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(upperPath, &st); err != nil {
		return nil, fs.ToErrno(err)
	}
	out.Attr.FromStat(&st)
	child := &unionNode{ufs: n.ufs, path: childPath}
	stable := fs.StableAttr{Mode: syscall.S_IFDIR, Ino: stableIno(childPath)}
	return n.NewInode(ctx, child, stable), 0
}

func (n *unionNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	if strings.HasPrefix(name, whPrefix) {
		return syscall.ENOENT
	}
	childPath := n.child(name)
	if n.ufs.isWhiteouted(childPath) {
		return syscall.ENOENT
	}
	upperP := n.ufs.upperPath(childPath)
	_, upErr := os.Lstat(upperP)
	_, inLower := n.ufs.findInLowers(childPath)
	if upErr != nil && !inLower {
		return syscall.ENOENT
	}
	// Merged-view emptiness check: the directory may inherit entries from
	// any lower layer that aren't physically in upper.
	if !n.dirEmptyMerged(childPath) {
		return syscall.ENOTEMPTY
	}
	if upErr == nil {
		if err := os.Remove(upperP); err != nil {
			return fs.ToErrno(err)
		}
	}
	if inLower {
		if err := writeWhiteout(n.ufs.whPathFor(childPath)); err != nil {
			return fs.ToErrno(err)
		}
	}
	return 0
}

// dirEmptyMerged returns true if the merged view of rel has no visible
// children (lower entries are hidden by whiteouts in upper).
func (n *unionNode) dirEmptyMerged(rel string) bool {
	whited := map[string]bool{}
	seen := map[string]bool{}
	if up, err := os.ReadDir(n.ufs.upperPath(rel)); err == nil {
		for _, e := range up {
			name := e.Name()
			if strings.HasPrefix(name, whPrefix) {
				whited[strings.TrimPrefix(name, whPrefix)] = true
				continue
			}
			seen[name] = true
		}
	}
	if len(seen) > 0 {
		return false
	}
	for _, ld := range n.ufs.lowerDirs {
		lp, err := os.ReadDir(filepath.Join(ld, rel))
		if err != nil {
			continue
		}
		for _, e := range lp {
			name := e.Name()
			if whited[name] || strings.HasPrefix(name, whPrefix) {
				continue
			}
			return false
		}
	}
	return true
}

// writeWhiteout creates the zero-byte marker file used to mask a lower entry.
func writeWhiteout(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// unionFile is the per-open file handle. It owns an fd into the *resolved*
// file (always in upper for write paths thanks to Open's CoW logic).
type unionFile struct {
	mu sync.Mutex
	fd int
}

var (
	_ fs.FileReader   = (*unionFile)(nil)
	_ fs.FileWriter   = (*unionFile)(nil)
	_ fs.FileReleaser = (*unionFile)(nil)
	_ fs.FileFsyncer  = (*unionFile)(nil)
	_ fs.FileFlusher  = (*unionFile)(nil)
	_ fs.FileGetattrer = (*unionFile)(nil)
)

func (f *unionFile) Read(ctx context.Context, buf []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fuse.ReadResultFd(uintptr(f.fd), off, len(buf)), 0
}

func (f *unionFile) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := syscall.Pwrite(f.fd, data, off)
	if err != nil {
		return 0, fs.ToErrno(err)
	}
	return uint32(n), 0
}

func (f *unionFile) Release(ctx context.Context) syscall.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fd >= 0 {
		err := syscall.Close(f.fd)
		f.fd = -1
		if err != nil {
			return fs.ToErrno(err)
		}
	}
	return 0
}

func (f *unionFile) Flush(ctx context.Context) syscall.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	// dup+close so buffered data reaches the kernel without closing our fd.
	newFd, err := syscall.Dup(f.fd)
	if err != nil {
		return fs.ToErrno(err)
	}
	if err := syscall.Close(newFd); err != nil {
		return fs.ToErrno(err)
	}
	return 0
}

func (f *unionFile) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := syscall.Fsync(f.fd); err != nil {
		return fs.ToErrno(err)
	}
	return 0
}

func (f *unionFile) Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	var st syscall.Stat_t
	if err := syscall.Fstat(f.fd, &st); err != nil {
		return fs.ToErrno(err)
	}
	out.FromStat(&st)
	return 0
}

func main() {
	debug := flag.Bool("debug", false, "enable FUSE debug logging")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <lower_dirs> <upper_dir> <mount_dir>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  <lower_dirs>  one or more read-only layers, colon-separated.\n")
		fmt.Fprintf(os.Stderr, "                Leftmost = highest priority, rightmost = deepest base.\n")
		fmt.Fprintf(os.Stderr, "                Example: base:midlayer:topmost\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 3 {
		flag.Usage()
		os.Exit(2)
	}

	rawLowers := strings.Split(flag.Arg(0), ":")
	lowerDirs := make([]string, 0, len(rawLowers))
	for _, d := range rawLowers {
		if d == "" {
			log.Fatalf("empty entry in <lower_dirs>")
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			log.Fatalf("lower_dir %q: %v", d, err)
		}
		lowerDirs = append(lowerDirs, abs)
	}
	if len(lowerDirs) == 0 {
		log.Fatal("at least one lower_dir required")
	}

	upperDir, err := filepath.Abs(flag.Arg(1))
	if err != nil {
		log.Fatalf("upper_dir: %v", err)
	}
	mountDir, err := filepath.Abs(flag.Arg(2))
	if err != nil {
		log.Fatalf("mount_dir: %v", err)
	}
	for _, d := range append(append([]string{}, lowerDirs...), upperDir, mountDir) {
		st, err := os.Stat(d)
		if err != nil {
			log.Fatalf("cannot stat %s: %v", d, err)
		}
		if !st.IsDir() {
			log.Fatalf("not a directory: %s", d)
		}
	}

	ufs := &unionFS{lowerDirs: lowerDirs, upperDir: upperDir}
	root := &unionNode{ufs: ufs, path: ""}

	opts := &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:        "mini_unionfs",
			Name:          "mini_unionfs",
			Debug:         *debug,
			AllowOther:    false,
			DisableXAttrs: true,
		},
	}

	server, err := fs.Mount(mountDir, root, opts)
	if err != nil {
		log.Fatalf("mount failed: %v", err)
	}
	log.Printf("mini_unionfs mounted: lowers=%v upper=%s mnt=%s", lowerDirs, upperDir, mountDir)

	// Clean unmount on SIGINT/SIGTERM so the test harness doesn't leave a
	// stale mount behind.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("unmounting...")
		_ = server.Unmount()
	}()

	server.Wait()
}
