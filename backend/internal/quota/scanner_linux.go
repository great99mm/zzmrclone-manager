//go:build linux

package quota

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const directoryBatchSize = 128

type rootIdentity struct {
	device int64
	inode  int64
}

func (s Scanner) Scan(root string, minAge time.Duration) ([]LocalSnapshot, error) {
	outcome, err := s.ScanWithOutcome(root, minAge)
	return outcome.Snapshots, err
}

func (s Scanner) ScanWithOutcome(root string, minAge time.Duration) (ScanOutcome, error) {
	if !filepath.IsAbs(root) {
		return ScanOutcome{}, fmt.Errorf("source root must be absolute: %q", root)
	}
	rootFile, identity, err := openRoot(root)
	if err != nil {
		return ScanOutcome{}, err
	}
	defer rootFile.Close()

	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	sleep := time.Sleep
	if s.Sleep != nil {
		sleep = s.Sleep
	}
	settle := s.SettleInterval
	if settle < 0 {
		settle = 0
	}

	snapshots := make([]LocalSnapshot, 0)
	var nextEligibleAt *time.Time
	if err := s.scanDirectory(rootFile, "", minAge, now, sleep, settle, identity, &snapshots, &nextEligibleAt); err != nil {
		return ScanOutcome{}, err
	}
	if s.BeforeFinalValidation != nil {
		s.BeforeFinalValidation()
	}

	// The traversal FDs may now refer to an unlinked old tree. Rebind the
	// pathname and validate every returned name before exposing it.
	boundRoot, boundIdentity, err := openRoot(root)
	if err != nil {
		return ScanOutcome{}, fmt.Errorf("source root identity drift during scan: %w", err)
	}
	defer boundRoot.Close()
	if boundIdentity != identity {
		return ScanOutcome{}, fmt.Errorf("source root identity drift during scan")
	}
	validated := snapshots[:0]
	for _, snapshot := range snapshots {
		if s.BeforeSnapshotValidation != nil {
			s.BeforeSnapshotValidation(snapshot.RelativePath)
		}
		ok, err := validateSnapshot(boundRoot, snapshot)
		if err != nil {
			return ScanOutcome{}, err
		}
		if ok {
			validated = append(validated, snapshot)
		}
	}
	// A namespace change during the validation loop can leave boundRoot
	// pointing at the old tree. Reopen the pathname once more before return.
	finalRoot, finalIdentity, err := openRoot(root)
	if err != nil {
		return ScanOutcome{}, fmt.Errorf("source root identity drift during scan: %w", err)
	}
	_ = finalRoot.Close()
	if finalIdentity != identity {
		return ScanOutcome{}, fmt.Errorf("source root identity drift during scan")
	}
	sort.Slice(validated, func(i, j int) bool { return validated[i].RelativePath < validated[j].RelativePath })
	return ScanOutcome{Snapshots: validated, NextEligibleAt: nextEligibleAt}, nil
}

func openRoot(root string) (*os.File, rootIdentity, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, rootIdentity{}, fmt.Errorf("open filesystem root: %w", err)
	}
	current := os.NewFile(uintptr(fd), "scanner-root")
	if current == nil {
		_ = unix.Close(fd)
		return nil, rootIdentity{}, fmt.Errorf("create filesystem root file")
	}
	for _, component := range rootComponents(root) {
		nextFD, openErr := unix.Openat(int(current.Fd()), component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			_ = current.Close()
			return nil, rootIdentity{}, fmt.Errorf("open source root %q: %w", root, openErr)
		}
		next := os.NewFile(uintptr(nextFD), "scanner-root-component")
		if next == nil {
			_ = unix.Close(nextFD)
			_ = current.Close()
			return nil, rootIdentity{}, fmt.Errorf("create source root component")
		}
		_ = current.Close()
		current = next
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(current.Fd()), &stat); err != nil {
		_ = current.Close()
		return nil, rootIdentity{}, fmt.Errorf("stat source root: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = current.Close()
		return nil, rootIdentity{}, fmt.Errorf("source root is not a directory: %q", root)
	}
	return current, rootIdentity{device: int64(stat.Dev), inode: int64(stat.Ino)}, nil
}

func rootComponents(root string) []string {
	clean := filepath.Clean(root)
	if clean == string(filepath.Separator) {
		return nil
	}
	return strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator))
}

func (s Scanner) scanDirectory(dir *os.File, prefix string, minAge time.Duration, now func() time.Time, sleep func(time.Duration), settle time.Duration, identity rootIdentity, out *[]LocalSnapshot, nextEligibleAt **time.Time) error {
	for {
		entries, readErr := dir.ReadDir(directoryBatchSize)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("read directory %q: %w", prefix, readErr)
		}
		for _, entry := range entries {
			if prefix == "" && (entry.Name() == ".rclone-manager-stage" || entry.Name() == ".rclone-manager-move") {
				continue
			}
			relative := entry.Name()
			if prefix != "" {
				relative = filepath.Join(prefix, entry.Name())
			}
			if strings.ContainsAny(relative, "\r\n") {
				return fmt.Errorf("relative path contains newline: %q", relative)
			}
			childDir, kind, err := openDirectory(int(dir.Fd()), entry.Name())
			if err != nil {
				return err
			}
			if kind == directoryObject {
				err = s.scanDirectory(childDir, relative, minAge, now, sleep, settle, identity, out, nextEligibleAt)
				_ = childDir.Close()
				if err != nil {
					return err
				}
				continue
			}
			if kind != regularCandidate {
				continue
			}
			first, ok, err := s.observe(int(dir.Fd()), entry.Name(), relative, 1)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if settle > 0 {
				sleep(settle)
			}
			second, ok, err := s.observe(int(dir.Fd()), entry.Name(), relative, 2)
			if err != nil {
				return err
			}
			if !ok || first != second {
				continue
			}
			if now().Sub(time.Unix(0, second.mtimeNS)) < minAge {
				candidate := time.Unix(0, second.mtimeNS).Add(minAge)
				if *nextEligibleAt == nil || candidate.Before(**nextEligibleAt) {
					*nextEligibleAt = &candidate
				}
				continue
			}
			*out = append(*out, LocalSnapshot{RelativePath: relative, SizeBytes: second.size, MtimeNS: second.mtimeNS, Device: second.device, Inode: second.inode, RootDevice: identity.device, RootInode: identity.inode, SnapshotKey: makeSnapshotKey(relative, second)})
		}
		if len(entries) == 0 || errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

const (
	invalidObject = iota
	directoryObject
	regularCandidate
)

func openDirectory(parentFD int, name string) (*os.File, int, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == nil {
		var stat unix.Stat_t
		if statErr := unix.Fstat(fd, &stat); statErr != nil {
			_ = unix.Close(fd)
			return nil, invalidObject, fmt.Errorf("stat directory %q: %w", name, statErr)
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
			directory := os.NewFile(uintptr(fd), "scanner-directory")
			if directory == nil {
				_ = unix.Close(fd)
				return nil, invalidObject, fmt.Errorf("create directory %q", name)
			}
			return directory, directoryObject, nil
		}
		_ = unix.Close(fd)
		return nil, invalidObject, nil
	}
	if !skippableLookupError(err) {
		return nil, invalidObject, fmt.Errorf("open child %q: %w", name, err)
	}

	fd, err = unix.Openat(parentFD, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if skippableLookupError(err) {
			return nil, invalidObject, nil
		}
		return nil, invalidObject, fmt.Errorf("open child %q: %w", name, err)
	}
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, invalidObject, fmt.Errorf("stat child %q: %w", name, err)
	}
	_ = unix.Close(fd)
	if stat.Mode&unix.S_IFMT == unix.S_IFREG {
		return nil, regularCandidate, nil
	}
	return nil, invalidObject, nil
}

func skippableLookupError(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP)
}

func (s Scanner) observe(parentFD int, name, relative string, observation int) (fileMetadata, bool, error) {
	if s.LookupHook != nil {
		s.LookupHook(relative, observation)
	}
	fd, err := unix.Openat(parentFD, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if skippableLookupError(err) {
			return fileMetadata{}, false, nil
		}
		return fileMetadata{}, false, fmt.Errorf("observe file %q: %w", relative, err)
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fileMetadata{}, false, fmt.Errorf("stat file %q: %w", relative, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fileMetadata{}, false, nil
	}
	return fileMetadata{size: stat.Size, mtimeNS: stat.Mtim.Sec*1e9 + stat.Mtim.Nsec, device: int64(stat.Dev), inode: int64(stat.Ino)}, true, nil
}

func validateSnapshot(root *os.File, snapshot LocalSnapshot) (bool, error) {
	if err := ValidateRelativePath(snapshot.RelativePath); err != nil {
		return false, err
	}
	current := root
	defer func() {
		if current != root {
			_ = current.Close()
		}
	}()
	components := strings.Split(snapshot.RelativePath, string(filepath.Separator))
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			return false, nil
		}
		if index == len(components)-1 {
			fd, err := unix.Openat(int(current.Fd()), component, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				if skippableLookupError(err) {
					return false, nil
				}
				return false, fmt.Errorf("validate file %q: %w", snapshot.RelativePath, err)
			}
			var stat unix.Stat_t
			statErr := unix.Fstat(fd, &stat)
			_ = unix.Close(fd)
			if statErr != nil {
				return false, fmt.Errorf("validate file %q: %w", snapshot.RelativePath, statErr)
			}
			if stat.Mode&unix.S_IFMT != unix.S_IFREG {
				return false, nil
			}
			meta := fileMetadata{size: stat.Size, mtimeNS: stat.Mtim.Sec*1e9 + stat.Mtim.Nsec, device: int64(stat.Dev), inode: int64(stat.Ino)}
			return meta == (fileMetadata{size: snapshot.SizeBytes, mtimeNS: snapshot.MtimeNS, device: snapshot.Device, inode: snapshot.Inode}), nil
		}
		fd, err := unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if skippableLookupError(err) {
				return false, nil
			}
			return false, fmt.Errorf("validate ancestor %q: %w", snapshot.RelativePath, err)
		}
		next := os.NewFile(uintptr(fd), "scanner-validation-directory")
		if next == nil {
			_ = unix.Close(fd)
			return false, fmt.Errorf("create validation directory")
		}
		if current != root {
			_ = current.Close()
		}
		current = next
	}
	return false, nil
}
