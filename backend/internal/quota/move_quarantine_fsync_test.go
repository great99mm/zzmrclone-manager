//go:build linux

package quota

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMoveQuarantineFsyncsNestedParentBeforeRename(t *testing.T) {
	rootPath := t.TempDir()
	filePath := filepath.Join(rootPath, "nested", "deeper", "file.bin")
	if err := os.MkdirAll(filepath.Dir(filePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshots, err := (Scanner{}).Scan(rootPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot LocalSnapshot
	for _, candidate := range snapshots {
		if candidate.RelativePath == "nested/deeper/file.bin" {
			snapshot = candidate
		}
	}
	if snapshot.RelativePath == "" {
		t.Fatal("file snapshot not found")
	}
	root, err := OpenSourceRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	quarantine, err := PrepareMoveQuarantine(root, 91, "0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	defer quarantine.Close()
	originalFsync := moveQuarantineFsync
	t.Cleanup(func() { moveQuarantineFsync = originalFsync })
	calls := 0
	moveQuarantineFsync = func(fd int) error {
		calls++
		path, _ := os.Readlink(filepath.Join("/proc/self/fd", fmt.Sprintf("%d", fd)))
		if strings.Contains(path, "/91-0123456789abcdef0123456789abcdef0123456789abcdef/nested") {
			return errors.New("injected nested-parent fsync failure")
		}
		return unix.Fsync(fd)
	}
	if _, _, err := quarantine.Move(snapshot.RelativePath, snapshot); err == nil {
		t.Fatal("rename succeeded after nested parent fsync failure")
	}
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("source file was moved despite fsync failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(quarantine.Path(), filepath.FromSlash(snapshot.RelativePath))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantine entry exists after fsync failure: %v", err)
	}
	if calls < 1 {
		t.Fatalf("fsync calls = %d, want nested parent fsync", calls)
	}
}
