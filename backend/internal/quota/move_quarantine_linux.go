//go:build linux

package quota

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"rclone-manager/internal/models"
)

// MoveQuarantine is a descriptor-relative, same-filesystem handoff area.
// It never recursively removes files; callers may only operate on manifest
// entries and the directory remains available for recovery evidence.
type MoveQuarantine struct {
	root    *SourceRootHandle
	manager *os.File
	dir     *os.File
	path    string
}

var moveQuarantineFsync = unix.Fsync

func PrepareMoveQuarantine(root *SourceRootHandle, batchID uint, owner string) (*MoveQuarantine, error) {
	return openMoveQuarantine(root, batchID, owner, true)
}

func OpenMoveQuarantine(root *SourceRootHandle, batchID uint, owner string) (*MoveQuarantine, error) {
	return openMoveQuarantine(root, batchID, owner, false)
}

func openMoveQuarantine(root *SourceRootHandle, batchID uint, owner string, create bool) (*MoveQuarantine, error) {
	if root == nil || root.file == nil || !models.IsValidOwnerToken(owner) {
		return nil, errors.New("invalid move quarantine arguments")
	}
	var managerFD int
	var err error
	if create {
		managerFD, err = openOrCreateDir(int(root.file.Fd()), ".rclone-manager-move")
	} else {
		managerFD, err = unix.Openat(int(root.file.Fd()), ".rclone-manager-move", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%d-%s", batchID, owner)
	if create {
		created, mkdirErr := true, unix.Mkdirat(managerFD, name, 0700)
		if errors.Is(mkdirErr, syscall.EEXIST) {
			created = false
		}
		if mkdirErr != nil && !errors.Is(mkdirErr, syscall.EEXIST) {
			_ = unix.Close(managerFD)
			return nil, mkdirErr
		}
		if created {
			if err := moveQuarantineFsync(managerFD); err != nil {
				_ = unix.Close(managerFD)
				return nil, err
			}
		}
	}
	dirFD, err := unix.Openat(managerFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(managerFD)
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(dirFD, &stat); err != nil || stat.Mode&0777 != 0700 {
		_ = unix.Close(dirFD)
		_ = unix.Close(managerFD)
		return nil, errors.New("move quarantine must be a trusted 0700 directory")
	}
	if err := moveQuarantineFsync(int(root.file.Fd())); err != nil {
		_ = unix.Close(dirFD)
		_ = unix.Close(managerFD)
		return nil, err
	}
	if err := moveQuarantineFsync(managerFD); err != nil {
		_ = unix.Close(dirFD)
		_ = unix.Close(managerFD)
		return nil, err
	}
	return &MoveQuarantine{root: root, manager: os.NewFile(uintptr(managerFD), "move-quarantine-manager"), dir: os.NewFile(uintptr(dirFD), "move-quarantine"), path: filepath.Join(filepath.Clean(rootPath(root)), ".rclone-manager-move", name)}, nil
}

func rootPath(root *SourceRootHandle) string {
	path, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", root.file.Fd()))
	if err != nil {
		return ""
	}
	return path
}

func (q *MoveQuarantine) Path() string { return q.path }
func (q *MoveQuarantine) File() *os.File {
	if q == nil {
		return nil
	}
	return q.dir
}

func (q *MoveQuarantine) Identity() (int64, int64, error) {
	var stat unix.Stat_t
	if q == nil || q.dir == nil || unix.Fstat(int(q.dir.Fd()), &stat) != nil {
		return 0, 0, errors.New("move quarantine identity unavailable")
	}
	return int64(stat.Dev), int64(stat.Ino), nil
}

func (q *MoveQuarantine) Move(relative string, snapshot LocalSnapshot) (int64, int64, error) {
	return q.rename(relative, snapshot, false)
}

func (q *MoveQuarantine) Restore(relative string, snapshot LocalSnapshot) error {
	_, _, err := q.rename(relative, snapshot, true)
	return err
}

func (q *MoveQuarantine) Present(relative string, snapshot LocalSnapshot) (bool, int64, int64, error) {
	parent, leaf, opened, err := q.openParents(relative, false, true)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
			return false, 0, 0, nil
		}
		return false, 0, 0, err
	}
	defer closeFiles(opened)
	fd, err := unix.Openat(int(parent.Fd()), leaf, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return false, 0, 0, nil
		}
		return false, 0, 0, err
	}
	defer unix.Close(fd)
	stat, err := statFD(fd)
	if err != nil {
		return false, 0, 0, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return false, 0, 0, errors.New("move quarantine entry is not a regular file")
	}
	if int64(stat.Dev) != snapshot.Device || int64(stat.Ino) != snapshot.Inode || stat.Size != snapshot.SizeBytes || mtimeNS(stat) != snapshot.MtimeNS {
		return false, 0, 0, errors.New("move quarantine entry identity changed")
	}
	return true, int64(stat.Dev), int64(stat.Ino), nil
}

func (q *MoveQuarantine) rename(relative string, snapshot LocalSnapshot, restore bool) (int64, int64, error) {
	if err := ValidateRelativePath(relative); err != nil {
		return 0, 0, err
	}
	if q == nil || q.root == nil || q.dir == nil {
		return 0, 0, errors.New("move quarantine is closed")
	}
	srcParent, srcLeaf, srcOpened, err := q.openRootParents(relative, restore)
	if err != nil {
		return 0, 0, err
	}
	defer closeFiles(srcOpened)
	dstParent, dstLeaf, dstOpened, err := q.openParents(relative, !restore, false)
	if err != nil {
		return 0, 0, err
	}
	defer closeFiles(dstOpened)
	from, to := srcParent, dstParent
	fromLeaf, toLeaf := srcLeaf, dstLeaf
	if restore {
		from, to, fromLeaf, toLeaf = dstParent, srcParent, dstLeaf, srcLeaf
	}
	if err := unix.Renameat2(int(from.Fd()), fromLeaf, int(to.Fd()), toLeaf, unix.RENAME_NOREPLACE); err != nil {
		return 0, 0, err
	}
	if err := moveQuarantineFsync(int(srcParent.Fd())); err != nil {
		return 0, 0, err
	}
	if err := moveQuarantineFsync(int(dstParent.Fd())); err != nil {
		return 0, 0, err
	}
	stat, err := statAt(int(to.Fd()), toLeaf)
	if err != nil {
		return 0, 0, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || int64(stat.Dev) != snapshot.Device || int64(stat.Ino) != snapshot.Inode || stat.Size != snapshot.SizeBytes || mtimeNS(stat) != snapshot.MtimeNS {
		return 0, 0, errors.New("move handoff identity changed after rename")
	}
	return int64(stat.Dev), int64(stat.Ino), nil
}

func (q *MoveQuarantine) openRootParents(relative string, create bool) (*os.File, string, []*os.File, error) {
	parts := strings.Split(relative, "/")
	parent := q.root.file
	opened := make([]*os.File, 0, len(parts))
	for _, part := range parts[:len(parts)-1] {
		fd, err := unix.Openat(int(parent.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil && create && errors.Is(err, syscall.ENOENT) {
			created := true
			if mkdirErr := unix.Mkdirat(int(parent.Fd()), part, 0700); mkdirErr != nil {
				created = false
				if !errors.Is(mkdirErr, syscall.EEXIST) {
					return nil, "", opened, mkdirErr
				}
			}
			if created {
				if syncErr := moveQuarantineFsync(int(parent.Fd())); syncErr != nil {
					return nil, "", opened, syncErr
				}
			}
			fd, err = unix.Openat(int(parent.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			return nil, "", opened, err
		}
		next := os.NewFile(uintptr(fd), "move-source-parent")
		if next == nil {
			_ = unix.Close(fd)
			return nil, "", opened, errors.New("move source parent unavailable")
		}
		opened = append(opened, next)
		parent = next
	}
	return parent, parts[len(parts)-1], opened, nil
}

func (q *MoveQuarantine) openParents(relative string, create bool, quarantine bool) (*os.File, string, []*os.File, error) {
	parts := strings.Split(relative, "/")
	parent := q.dir
	opened := make([]*os.File, 0, len(parts))
	for _, part := range parts[:len(parts)-1] {
		fd, err := unix.Openat(int(parent.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil && create && errors.Is(err, syscall.ENOENT) {
			created := true
			if mkdirErr := unix.Mkdirat(int(parent.Fd()), part, 0700); mkdirErr != nil {
				created = false
				if !errors.Is(mkdirErr, syscall.EEXIST) {
					return nil, "", opened, mkdirErr
				}
			}
			if created {
				if syncErr := moveQuarantineFsync(int(parent.Fd())); syncErr != nil {
					return nil, "", opened, syncErr
				}
			}
			fd, err = unix.Openat(int(parent.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			return nil, "", opened, err
		}
		next := os.NewFile(uintptr(fd), "move-quarantine-parent")
		if next == nil {
			_ = unix.Close(fd)
			return nil, "", opened, errors.New("move quarantine parent unavailable")
		}
		opened = append(opened, next)
		parent = next
	}
	_ = quarantine
	return parent, parts[len(parts)-1], opened, nil
}

func closeFiles(files []*os.File) {
	for _, file := range files {
		_ = file.Close()
	}
}

func statAt(parent int, name string) (unix.Stat_t, error) {
	fd, err := unix.Openat(parent, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return unix.Stat_t{}, err
	}
	defer unix.Close(fd)
	return statFD(fd)
}

func statFD(fd int) (unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return unix.Stat_t{}, err
	}
	return stat, nil
}

func mtimeNS(stat unix.Stat_t) int64 { return stat.Mtim.Sec*1e9 + stat.Mtim.Nsec }

func (q *MoveQuarantine) Close() error {
	if q == nil {
		return nil
	}
	if q.dir != nil {
		_ = q.dir.Close()
	}
	if q.manager != nil {
		return q.manager.Close()
	}
	return nil
}
