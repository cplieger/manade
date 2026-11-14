package platform

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Driver is a Docker graph driver.
type Driver string

// The drivers a pick answers. The zfs driver is never one: it would have
// the host administer the machine's pool.
const (
	Overlay2 Driver = "overlay2"
	Btrfs    Driver = "btrfs"
	VFS      Driver = "vfs"
)

// For is who a storage pick is for.
type For int

const (
	// ForHost refuses a data root that holds neither overlay2 nor btrfs.
	ForHost For = iota + 1
	// ForJob is the docker image's own daemon, which falls back to vfs.
	ForJob
)

// errNoStorageDriver is a host data root that can hold neither overlay2
// nor btrfs.
var errNoStorageDriver = errors.New("platform: no storage driver fits the data root")

// StoragePick is a pick and why it was made.
type StoragePick struct {
	Driver Driver
	Why    string
}

// The statfs(2) magic numbers the pick names (linux/magic.h).
const (
	magicOverlay = 0x794c7630
	magicBtrfs   = 0x9123683e
)

var fsNames = map[int64]string{
	magicOverlay: "overlayfs", magicBtrfs: "btrfs", 0x2fc12fc1: "zfs", 0xef53: "ext4",
	0x58465342: "xfs", 0x01021994: "tmpfs", 0x6969: "nfs", 0xff534d42: "cifs",
}

func fsName(magic int64) string {
	if n, ok := fsNames[magic]; ok {
		return n
	}
	return fmt.Sprintf("filesystem 0x%x", magic)
}

// storageSeams are the kernel calls a pick makes.
type storageSeams struct {
	filesystems func() (string, error)
	fsType      func(dir string) (int64, error)
	overlay     func(dir string) error
}

// PickStorage picks the graph driver for dataRoot: overlay2 only when the
// kernel lists overlay, dataRoot is not overlayfs and a test overlay in it
// takes both a delete's and a rename's whiteout, since a kernel lists
// overlay whatever the volume holds and OpenZFS before 2.2 takes only the
// first; else btrfs on btrfs; else vfs for a job and errNoStorageDriver
// for a host. It mounts the test overlay, so it needs CAP_SYS_ADMIN.
func PickStorage(r Root, dataRoot string, who For) (StoragePick, error) {
	return pickStorage(&storageSeams{
		filesystems: func() (string, error) { return r.read("/proc/filesystems") },
		fsType:      statfsType,
		overlay:     testOverlay,
	}, dataRoot, who)
}

func pickStorage(s *storageSeams, dataRoot string, who For) (StoragePick, error) {
	fss, err := s.filesystems()
	if err != nil {
		return StoragePick{}, err
	}
	magic, err := s.fsType(dataRoot)
	if err != nil {
		return StoragePick{}, fmt.Errorf("the data root %s: %w", dataRoot, err)
	}
	name := fsName(magic)
	why := "the data root is " + name
	listed := listsFilesystem(fss, "overlay")
	switch {
	case magic == magicOverlay:
		if who == ForHost {
			return StoragePick{}, fmt.Errorf("%w: %s is on overlayfs, the container's own layer, which cannot hold the daemon's overlays; mount a volume there", errNoStorageDriver, dataRoot)
		}
		why += ", which cannot hold an overlay"
	case !listed:
		why += "; the kernel lists no overlay filesystem"
	default:
		if err := s.overlay(dataRoot); err != nil {
			why += "; a test overlay there failed: " + err.Error()
		} else {
			return StoragePick{Driver: Overlay2, Why: why + "; a test overlay there took both whiteouts"}, nil
		}
	}
	if magic == magicBtrfs {
		return StoragePick{Driver: Btrfs, Why: why}, nil
	}
	if who == ForJob {
		return StoragePick{Driver: VFS, Why: why}, nil
	}
	return StoragePick{}, fmt.Errorf("%w: %s; give %s a volume on OpenZFS 2.2 or later, ext4, XFS with ftype=1 or btrfs", errNoStorageDriver, why, dataRoot)
}

// listsFilesystem reports whether /proc/filesystems lists name.
func listsFilesystem(fss, name string) bool {
	for line := range strings.SplitSeq(fss, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[len(f)-1] == name {
			return true
		}
	}
	return false
}

func statfsType(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Type), nil //nolint:unconvert // Type's width differs by architecture.
}

// The test overlay's two lower files: one deleted, one renamed.
const (
	whiteoutDelete = "delete"
	whiteoutRename = "rename"
)

// testOverlay mounts an overlay in a fresh directory under dir, deletes one
// lower file and renames another through it, and requires each to leave a
// whiteout, a character device, in the upper directory.
func testOverlay(dir string) (err error) {
	t, err := os.MkdirTemp(dir, ".manade-overlay-test.")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(t)) }()
	l, u, w, m := filepath.Join(t, "l"), filepath.Join(t, "u"), filepath.Join(t, "w"), filepath.Join(t, "m")
	err = layOverlay(l, u, w, m)
	if err != nil {
		return err
	}
	if err = syscall.Mount("overlay", m, "overlay", 0, "lowerdir="+l+",upperdir="+u+",workdir="+w); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	defer func() { err = errors.Join(err, syscall.Unmount(m, 0)) }()
	return whiteouts(m, u)
}

// layOverlay makes the overlay's directories and its lower files.
func layOverlay(l string, dirs ...string) error {
	for _, d := range append([]string{l}, dirs...) {
		if err := os.Mkdir(d, 0o700); err != nil {
			return err
		}
	}
	for _, f := range []string{whiteoutDelete, whiteoutRename} {
		if err := os.WriteFile(filepath.Join(l, f), []byte(f), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// whiteouts deletes and renames through the overlay at m and checks the
// upper directory u for both whiteouts.
func whiteouts(m, u string) error {
	if err := os.Remove(filepath.Join(m, whiteoutDelete)); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if err := os.Rename(filepath.Join(m, whiteoutRename), filepath.Join(m, "renamed")); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	for _, f := range []string{whiteoutDelete, whiteoutRename} {
		fi, err := os.Lstat(filepath.Join(u, f))
		if err != nil || fi.Mode()&fs.ModeCharDevice == 0 {
			return fmt.Errorf("no whiteout for the %s", f)
		}
	}
	return nil
}

// Usage is a filesystem's size and the bytes free to an unprivileged
// writer; statfs shows a ZFS dataset's quota as its size.
type Usage struct {
	Size uint64
	Free uint64
}

// DataUsage answers the usage of the filesystem holding dir.
func DataUsage(dir string) (Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return Usage{}, err
	}
	bs := uint64(st.Bsize) //nolint:gosec // a block size is positive.
	return Usage{Size: st.Blocks * bs, Free: st.Bavail * bs}, nil
}

// ProjectQuota reports whether dir lies on XFS mounted with project quota,
// the one filesystem overlay2 enforces a writable-layer size on.
func ProjectQuota(ms []Mount, dir string) bool {
	var best *Mount
	for i := range ms {
		p := ms[i].Point
		if (dir == p || p == "/" || strings.HasPrefix(dir, p+"/")) && (best == nil || len(p) > len(best.Point)) {
			best = &ms[i]
		}
	}
	return best != nil && best.FSType == "xfs" && (best.hasSuper("prjquota") || best.hasSuper("pquota"))
}
