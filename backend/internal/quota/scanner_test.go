//go:build linux

package quota

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScannerLexicalAgeAndSymlinkHandling(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for name, contents := range map[string]string{"z.txt": "12345", "a.txt": "12", "dir/nested": "nested"} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
		old := now.Add(-time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "z.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}

	snapshots, err := (Scanner{Now: func() time.Time { return now }}).Scan(root, time.Minute)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	paths := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		paths = append(paths, snapshot.RelativePath)
	}
	want := []string{"a.txt", filepath.Join("dir", "nested"), "z.txt"}
	if len(paths) != len(want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths = %#v, want %#v", paths, want)
		}
	}
	if old, err := (Scanner{Now: func() time.Time { return now }}).Scan(root, 2*time.Hour); err != nil || len(old) != 0 {
		t.Fatalf("age scan = %#v, %v; want no snapshots", old, err)
	}
}

func TestScannerRejectsNewline(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "bad\nname"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := (Scanner{}).Scan(root, 0); err == nil {
		t.Fatal("newline path was accepted")
	}
}

func TestScannerExcludesMoveQuarantine(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".rclone-manager-move"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".rclone-manager-move", "hidden.bin"), []byte("hidden"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "visible.bin"), []byte("visible"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshots, err := (Scanner{}).Scan(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].RelativePath != "visible.bin" {
		t.Fatalf("move quarantine was scanned: %#v", snapshots)
	}
}

func TestScannerRejectsRootAndNestedSymlinks(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "outside"), []byte("outside"), 0644); err != nil {
		t.Fatal(err)
	}
	rootLink := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(root, rootLink); err != nil {
		t.Fatal(err)
	}
	if _, err := (Scanner{}).Scan(rootLink, 0); err == nil {
		t.Fatal("symlink root was accepted")
	}
	if err := os.Symlink(target, filepath.Join(root, "nested")); err != nil {
		t.Fatal(err)
	}
	snapshots, err := (Scanner{}).Scan(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("nested symlink snapshots = %#v", snapshots)
	}
}

func TestScannerRejectsSymlinkRootAncestorComponent(t *testing.T) {
	parent := t.TempDir()
	target := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, "a"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(parent, "a", "b"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(parent, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := (Scanner{}).Scan(filepath.Join(parent, "link", "b"), 0); err == nil {
		t.Fatal("symlink ancestor component was accepted")
	}
}

func TestScannerSkipsLeafReplacementBetweenLookups(t *testing.T) {
	root := t.TempDir()
	leaf := filepath.Join(root, "leaf")
	target := filepath.Join(root, "outside")
	if err := os.WriteFile(leaf, []byte("inside"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("outside"), 0644); err != nil {
		t.Fatal(err)
	}
	replaced := false
	scanner := Scanner{LookupHook: func(path string, observation int) {
		if path == "leaf" && observation == 2 && !replaced {
			replaced = true
			if err := os.Remove(leaf); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, leaf); err != nil {
				t.Fatal(err)
			}
		}
	}}
	snapshots, err := scanner.Scan(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range snapshots {
		if snapshot.RelativePath == "leaf" {
			t.Fatal("replaced leaf was included")
		}
	}
	if !replaced {
		t.Fatal("leaf replacement hook was not triggered")
	}
}

func TestScannerSkipsMetadataChangeBetweenLookups(t *testing.T) {
	root := t.TempDir()
	leaf := filepath.Join(root, "changing")
	if err := os.WriteFile(leaf, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	changed := false
	scanner := Scanner{LookupHook: func(path string, observation int) {
		if path != "changing" || observation != 2 || changed {
			return
		}
		changed = true
		if err := os.Remove(leaf); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(leaf, []byte("new contents"), 0644); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-time.Hour)
		if err := os.Chtimes(leaf, when, when); err != nil {
			t.Fatal(err)
		}
	}}
	snapshots, err := scanner.Scan(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("changed metadata was included: %#v", snapshots)
	}
	if !changed {
		t.Fatal("metadata change hook was not triggered")
	}
}

func TestScannerAncestorReplacementCannotEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := t.TempDir()
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside"), []byte("inside"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "outside"), []byte("outside"), 0644); err != nil {
		t.Fatal(err)
	}
	called := false
	scanner := Scanner{LookupHook: func(path string, observation int) {
		if path == "inside" && observation == 1 && !called {
			called = true
			moved := filepath.Join(parent, "moved")
			if err := os.Rename(root, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, root); err != nil {
				t.Fatal(err)
			}
		}
	}}
	snapshots, err := scanner.Scan(root, 0)
	if err == nil {
		t.Fatalf("root replacement was not rejected, snapshots = %#v", snapshots)
	}
	if !called {
		t.Fatal("ancestor replacement hook was not triggered")
	}
}

func TestScannerFinalValidationSkipsReplacedSubdirectory(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	sub := filepath.Join(root, "sub")
	outside := t.TempDir()
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "inside"), []byte("inside"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "outside"), []byte("outside"), 0644); err != nil {
		t.Fatal(err)
	}
	replaced := false
	scanner := Scanner{LookupHook: func(path string, observation int) {
		if path != filepath.Join("sub", "inside") || observation != 1 || replaced {
			return
		}
		replaced = true
		moved := filepath.Join(parent, "moved-sub")
		if err := os.Rename(sub, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, sub); err != nil {
			t.Fatal(err)
		}
	}}
	snapshots, err := scanner.Scan(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !replaced {
		t.Fatal("subdirectory replacement hook was not triggered")
	}
	if len(snapshots) != 0 {
		t.Fatalf("stale subdirectory snapshot escaped final validation: %#v", snapshots)
	}
}

func TestScannerFinalRootIdentityDriftFailsClosed(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := t.TempDir()
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside"), []byte("inside"), 0644); err != nil {
		t.Fatal(err)
	}
	called := false
	scanner := Scanner{BeforeFinalValidation: func() {
		called = true
		moved := filepath.Join(parent, "moved-root")
		if err := os.Rename(root, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, root); err != nil {
			t.Fatal(err)
		}
	}}
	if snapshots, err := scanner.Scan(root, 0); err == nil {
		t.Fatalf("root identity drift was accepted: %#v", snapshots)
	}
	if !called {
		t.Fatal("final validation hook was not triggered")
	}
}

func TestScannerBookendDetectsRootDriftDuringValidation(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := t.TempDir()
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	validated := make([]string, 0, 2)
	scanner := Scanner{BeforeSnapshotValidation: func(path string) {
		validated = append(validated, path)
		if path != "a" {
			return
		}
		moved := filepath.Join(parent, "moved-root")
		if err := os.Rename(root, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, root); err != nil {
			t.Fatal(err)
		}
	}}
	if snapshots, err := scanner.Scan(root, 0); err == nil || !strings.Contains(err.Error(), "root identity drift") {
		t.Fatalf("bookend root drift error = %v, snapshots = %#v", err, snapshots)
	}
	if len(validated) < 2 {
		t.Fatalf("validation hook ran for %#v, want at least two snapshots", validated)
	}
}

func TestScannerRootOpenFailuresDoNotLeakFDs(t *testing.T) {
	fdCount := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := fdCount()
	base := filepath.Join(t.TempDir(), "missing")
	for i := 0; i < 200; i++ {
		if _, err := (Scanner{}).Scan(filepath.Join(base, "deep", "root"), 0); err == nil {
			t.Fatal("bad root unexpectedly opened")
		}
	}
	after := fdCount()
	if after > before+2 {
		t.Fatalf("file descriptors grew from %d to %d", before, after)
	}
}
