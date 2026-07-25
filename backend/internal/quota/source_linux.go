//go:build linux

package quota

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"rclone-manager/internal/models"
)

// SourceRootHandle is a no-follow descriptor for a validated local source root.
type SourceRootHandle struct {
	file   *os.File
	Device int64
	Inode  int64
}

func OpenSourceRoot(root string) (*SourceRootHandle, error) {
	file, identity, err := openRoot(root)
	if err != nil {
		return nil, err
	}
	return &SourceRootHandle{file: file, Device: identity.device, Inode: identity.inode}, nil
}

func (h *SourceRootHandle) File() *os.File { return h.file }
func (h *SourceRootHandle) Close() error   { return h.file.Close() }

func (h *SourceRootHandle) Validate(snapshot LocalSnapshot) (bool, error) {
	if snapshot.RootDevice != h.Device || snapshot.RootInode != h.Inode {
		return false, fmt.Errorf("source root identity mismatch")
	}
	return validateSnapshot(h.file, snapshot)
}

// OpenValidated opens the exact snapshotted inode through the root descriptor.
// Every ancestor is opened with O_NOFOLLOW, so a pathname replacement cannot
// redirect the returned descriptor.
func (h *SourceRootHandle) OpenValidated(snapshot LocalSnapshot) (*os.File, error) {
	if err := ValidateRelativePath(snapshot.RelativePath); err != nil {
		return nil, err
	}
	if snapshot.RootDevice != h.Device || snapshot.RootInode != h.Inode {
		return nil, fmt.Errorf("source root identity mismatch")
	}
	components := strings.Split(snapshot.RelativePath, "/")
	parent, err := unix.Dup(int(h.file.Fd()))
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(parent), "validated-source-parent")
	if current == nil {
		_ = unix.Close(parent)
		return nil, fmt.Errorf("open source parent")
	}
	for _, component := range components[:len(components)-1] {
		fd, openErr := unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = current.Close()
		if openErr != nil {
			return nil, fmt.Errorf("open source ancestor %q: %w", component, openErr)
		}
		current = os.NewFile(uintptr(fd), "validated-source-parent")
	}
	fd, err := unix.Openat(int(current.Fd()), components[len(components)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	_ = current.Close()
	if err != nil {
		return nil, fmt.Errorf("open source file: %w", err)
	}
	file := os.NewFile(uintptr(fd), "validated-source-file")
	var stat unix.Stat_t
	if file == nil || unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || int64(stat.Dev) != snapshot.Device || int64(stat.Ino) != snapshot.Inode || int64(stat.Size) != snapshot.SizeBytes || stat.Mtim.Sec*1e9+stat.Mtim.Nsec != snapshot.MtimeNS {
		if file != nil {
			_ = file.Close()
		} else {
			_ = unix.Close(fd)
		}
		return nil, fmt.Errorf("source file changed")
	}
	return file, nil
}

// RemoveEmptyParents removes only empty ancestors of the supplied source files.
// Traversal remains descriptor-relative and rejects symlinked ancestors.
func (h *SourceRootHandle) RemoveEmptyParents(relativePaths []string) error {
	if h == nil || h.file == nil {
		return errors.New("source root is unavailable")
	}
	directories := make(map[string]struct{})
	for _, relative := range relativePaths {
		if err := ValidateRelativePath(relative); err != nil {
			return err
		}
		for directory := path.Dir(relative); directory != "." && directory != "/"; directory = path.Dir(directory) {
			directories[directory] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(directories))
	for directory := range directories {
		ordered = append(ordered, directory)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i]) == len(ordered[j]) {
			return ordered[i] < ordered[j]
		}
		return len(ordered[i]) > len(ordered[j])
	})
	for _, directory := range ordered {
		if err := h.removeEmptyDirectory(directory); err != nil {
			return err
		}
	}
	return nil
}

func (h *SourceRootHandle) removeEmptyDirectory(relative string) error {
	if err := ValidateRelativePath(relative); err != nil {
		return err
	}
	parts := strings.Split(relative, "/")
	fd, err := unix.Dup(int(h.file.Fd()))
	if err != nil {
		return err
	}
	parent := os.NewFile(uintptr(fd), "source-empty-parent")
	if parent == nil {
		_ = unix.Close(fd)
		return errors.New("open source empty parent")
	}
	defer parent.Close()
	for _, part := range parts[:len(parts)-1] {
		nextFD, openErr := unix.Openat(int(parent.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			if errors.Is(openErr, syscall.ENOENT) || errors.Is(openErr, syscall.ENOTDIR) {
				return nil
			}
			return openErr
		}
		next := os.NewFile(uintptr(nextFD), "source-empty-parent")
		if next == nil {
			_ = unix.Close(nextFD)
			return errors.New("open source empty parent")
		}
		if err := parent.Close(); err != nil {
			_ = next.Close()
			return err
		}
		parent = next
	}
	err = unix.Unlinkat(int(parent.Fd()), parts[len(parts)-1], unix.AT_REMOVEDIR)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			return nil
		}
		return err
	}
	return unix.Fsync(int(parent.Fd()))
}

type StageHandle struct {
	file        *os.File
	manager     *os.File
	name        string
	expected    map[string]fileMetadata
	BeforeClone func()
	device      int64
	inode       int64
}

func PrepareStage(base string, batchID uint, owner, lease string) (*StageHandle, error) {
	if !models.IsValidOwnerToken(owner) || !models.IsValidLeaseToken(lease) || !filepath.IsAbs(base) || filepath.Clean(base) != base {
		return nil, fmt.Errorf("invalid stage base or owner")
	}
	info, err := os.Lstat(base)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("stage base must be a trusted directory")
	}
	baseFD, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	managerFD, err := openOrCreateDir(baseFD, ".rclone-manager-stage")
	if err != nil {
		_ = unix.Close(baseFD)
		return nil, err
	}
	_ = unix.Close(baseFD)
	name := fmt.Sprintf("%d-%s-%s", batchID, owner, lease)
	if err = unix.Mkdirat(managerFD, name, 0700); err != nil {
		if !errors.Is(err, syscall.EEXIST) {
			_ = unix.Close(managerFD)
			return nil, err
		}
	}
	stageFD, err := unix.Openat(managerFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(managerFD)
		return nil, err
	}
	var stat unix.Stat_t
	if err = unix.Fstat(managerFD, &stat); err != nil || stat.Mode&0777 != 0700 {
		_ = unix.Close(stageFD)
		_ = unix.Close(managerFD)
		return nil, fmt.Errorf("manager stage directory must be mode 0700")
	}
	if err = unix.Fstat(stageFD, &stat); err != nil || stat.Mode&0777 != 0700 {
		_ = unix.Close(stageFD)
		_ = unix.Close(managerFD)
		return nil, fmt.Errorf("batch stage directory must be mode 0700")
	}
	return &StageHandle{file: os.NewFile(uintptr(stageFD), "rclone-stage"), manager: os.NewFile(uintptr(managerFD), "rclone-stage-manager"), name: name, expected: make(map[string]fileMetadata), device: int64(stat.Dev), inode: int64(stat.Ino)}, nil
}

func openOrCreateDir(parent int, name string) (int, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if !errors.Is(err, syscall.ENOENT) {
			return -1, err
		}
		if err = unix.Mkdirat(parent, name, 0700); err != nil && !errors.Is(err, syscall.EEXIST) {
			return -1, err
		}
		fd, err = unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if err == nil {
		var stat unix.Stat_t
		if unix.Fstat(fd, &stat) != nil || stat.Mode&0777 != 0700 {
			_ = unix.Close(fd)
			return -1, fmt.Errorf("stage manager must be mode 0700")
		}
	}
	return fd, err
}

func (s *StageHandle) File() *os.File             { return s.file }
func (s *StageHandle) SetBeforeClone(hook func()) { s.BeforeClone = hook }
func (s *StageHandle) Snapshot(snapshot LocalSnapshot, source *os.File) error {
	if err := ValidateRelativePath(snapshot.RelativePath); err != nil {
		return err
	}
	parts := strings.Split(snapshot.RelativePath, "/")
	if source == nil {
		return fmt.Errorf("source descriptor is required")
	}
	var before unix.Stat_t
	if err := unix.Fstat(int(source.Fd()), &before); err != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || int64(before.Size) != snapshot.SizeBytes || int64(before.Dev) != snapshot.Device || int64(before.Ino) != snapshot.Inode || before.Mtim.Sec*1e9+before.Mtim.Nsec != snapshot.MtimeNS {
		return fmt.Errorf("source changed before immutable staging")
	}
	parent := s.file
	opened := make([]*os.File, 0, len(parts))
	defer func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		if err := unix.Mkdirat(int(parent.Fd()), part, 0700); err != nil && !errors.Is(err, syscall.EEXIST) {
			return err
		}
		fd, err := unix.Openat(int(parent.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		next := os.NewFile(uintptr(fd), "rclone-stage-parent")
		if next == nil {
			_ = unix.Close(fd)
			return fmt.Errorf("stage parent")
		}
		opened = append(opened, next)
		parent = next
	}
	fd, err := unix.Openat(int(parent.Fd()), parts[len(parts)-1], unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		if errors.Is(err, syscall.EEXIST) {
			if unlinkErr := unix.Unlinkat(int(parent.Fd()), parts[len(parts)-1], 0); unlinkErr != nil {
				return unlinkErr
			}
			fd, err = unix.Openat(int(parent.Fd()), parts[len(parts)-1], unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		}
	}
	if err != nil {
		return err
	}
	dst := os.NewFile(uintptr(fd), "rclone-stage-file")
	if dst == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("stage file")
	}
	defer dst.Close()
	if s.BeforeClone != nil {
		s.BeforeClone()
	}
	const ficlone = 0x40049409
	cloneErr := error(nil)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(dst.Fd()), uintptr(ficlone), uintptr(source.Fd())); errno != 0 {
		cloneErr = errno
	}
	if cloneErr != nil {
		if err := ensureFreeCapacity(parent, snapshot.SizeBytes); err != nil {
			_ = os.Remove(fmt.Sprintf("/proc/self/fd/%d/%s", parent.Fd(), parts[len(parts)-1]))
			return err
		}
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(dst, source, snapshot.SizeBytes); err != nil {
			return err
		}
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return err
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(source.Fd()), &after); err != nil || after.Mode&unix.S_IFMT != unix.S_IFREG || int64(after.Size) != snapshot.SizeBytes || int64(after.Dev) != snapshot.Device || int64(after.Ino) != snapshot.Inode || after.Mtim.Sec*1e9+after.Mtim.Nsec != snapshot.MtimeNS {
		_ = os.Remove(fmt.Sprintf("/proc/self/fd/%d/%s", parent.Fd(), parts[len(parts)-1]))
		return fmt.Errorf("source changed during immutable staging")
	}
	if int64(st.Size) != snapshot.SizeBytes || (int64(st.Dev) == snapshot.Device && int64(st.Ino) == snapshot.Inode) {
		return fmt.Errorf("immutable stage verification failed")
	}
	s.expected[snapshot.RelativePath] = fileMetadata{size: st.Size, device: int64(st.Dev), inode: int64(st.Ino)}
	return nil
}

func (s *StageHandle) Validate(snapshot LocalSnapshot) error {
	if err := ValidateRelativePath(snapshot.RelativePath); err != nil {
		return err
	}
	parts := strings.Split(snapshot.RelativePath, "/")
	parentFD, err := unix.Dup(int(s.file.Fd()))
	if err != nil {
		return err
	}
	parent := os.NewFile(uintptr(parentFD), "rclone-stage-parent")
	if parent == nil {
		_ = unix.Close(parentFD)
		return fmt.Errorf("stage parent")
	}
	defer func() {
		if parent != nil {
			_ = parent.Close()
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		fd, openErr := unix.Openat(int(parent.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			return openErr
		}
		if err := parent.Close(); err != nil {
			return err
		}
		parent = os.NewFile(uintptr(fd), "rclone-stage-parent")
		if parent == nil {
			_ = unix.Close(fd)
			return fmt.Errorf("stage parent")
		}
	}
	fd, err := unix.Openat(int(parent.Fd()), parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		return err
	}
	expected := fileMetadata{size: snapshot.SizeBytes, device: snapshot.Device, inode: snapshot.Inode}
	if value, ok := s.expected[snapshot.RelativePath]; ok {
		expected = value
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || int64(stat.Dev) != expected.device || int64(stat.Ino) != expected.inode || int64(stat.Size) != expected.size {
		return fmt.Errorf("stage link changed")
	}
	return nil
}

func ensureFreeCapacity(parent *os.File, size int64) error {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(parent.Fd()), &stat); err != nil {
		return err
	}
	if size < 0 || uint64(size) > stat.Bavail*uint64(stat.Bsize) {
		return fmt.Errorf("insufficient stage capacity")
	}
	return nil
}

func (s *StageHandle) Close() error {
	if s.file != nil {
		_ = s.file.Close()
	}
	if s.manager != nil {
		return s.manager.Close()
	}
	return nil
}
func (s *StageHandle) Cleanup() error {
	if s == nil || s.manager == nil {
		return nil
	}
	var owned unix.Stat_t
	if s.file == nil || unix.Fstat(int(s.file.Fd()), &owned) != nil || int64(owned.Dev) != s.device || int64(owned.Ino) != s.inode {
		_ = s.Close()
		return fmt.Errorf("stage identity changed; refusing cleanup")
	}
	fd, err := unix.Openat(int(s.manager.Fd()), s.name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = s.Close()
		return fmt.Errorf("stage identity unavailable; refusing cleanup: %w", err)
	}
	var reopened unix.Stat_t
	if unix.Fstat(fd, &reopened) != nil || int64(reopened.Dev) != s.device || int64(reopened.Ino) != s.inode {
		_ = unix.Close(fd)
		_ = s.Close()
		return fmt.Errorf("stage replacement detected; refusing cleanup")
	}
	_ = unix.Close(fd)
	err = os.RemoveAll(fmt.Sprintf("/proc/self/fd/%d/%s", s.manager.Fd(), s.name))
	_ = s.Close()
	return err
}

func SourceRootProcessToken(file *os.File) string {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
}
