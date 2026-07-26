//go:build linux

package proactive

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"rclone-manager/internal/quota"
)

func TestExecRunnerMoveLocalAliasNestedManifest(t *testing.T) {
	realBinary, err := exec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone is not installed; run this test in the production Alpine image")
	}
	rootPath := t.TempDir()
	selected := filepath.Join(rootPath, "a", "a", "a", "a.mp4")
	unselected := filepath.Join(rootPath, "keep.txt")
	if err := os.MkdirAll(filepath.Dir(selected), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selected, []byte("selected"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unselected, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(t.TempDir(), "files-from-raw")
	if err := os.WriteFile(manifest, []byte("a/a/a/a.mp4\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(config, []byte("[local]\ntype = local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "rclone-command.log")
	wrapper := filepath.Join(t.TempDir(), "rclone-wrapper")
	contents := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$RCLONE_TEST_LOG\"\nexec \"$RCLONE_TEST_REAL\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(contents), 0700); err != nil {
		t.Fatal(err)
	}
	oldLog, oldReal := os.Getenv("RCLONE_TEST_LOG"), os.Getenv("RCLONE_TEST_REAL")
	if err := os.Setenv("RCLONE_TEST_LOG", logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("RCLONE_TEST_REAL", realBinary); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Setenv("RCLONE_TEST_LOG", oldLog)
		_ = os.Setenv("RCLONE_TEST_REAL", oldReal)
	})
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	destinationName := fmt.Sprintf("phase4-rclone-%d", os.Getpid())
	destinationRelative := filepath.Join("tmp", destinationName)
	destinationPath := filepath.Join(workingDir, destinationRelative)
	_ = os.RemoveAll(destinationPath)
	t.Cleanup(func() { _ = os.RemoveAll(destinationPath) })
	root, err := quota.OpenSourceRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	runner := ExecRunner{Binary: wrapper}
	process, err := runner.StartMove(context.Background(), MoveSpec{ConfigPath: config, ManifestPath: manifest, SourceRoot: root.File(), DestinationRemote: "local", DestinationPath: "/" + destinationRelative})
	if err != nil {
		t.Fatal(err)
	}
	result, waitErr := process.Wait()
	if waitErr != nil || result.ExitCode != 0 {
		t.Fatalf("local alias move failed: result=%#v err=%v stderr=%s", result, waitErr, result.Stderr)
	}
	if _, err := os.Stat(selected); !os.IsNotExist(err) {
		t.Fatalf("selected source remains: %v", err)
	}
	if data, err := os.ReadFile(unselected); err != nil || string(data) != "keep" {
		t.Fatalf("unselected source changed: data=%q err=%v", data, err)
	}
	destinationFile := filepath.Join(destinationPath, "a", "a", "a", "a.mp4")
	if data, err := os.ReadFile(destinationFile); err != nil || string(data) != "selected" {
		t.Fatalf("nested destination missing or changed: data=%q err=%v", data, err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	commandLog := strings.ToLower(string(log))
	if !strings.Contains(commandLog, "--drive-stop-on-upload-limit") {
		t.Fatalf("move did not include upload-limit stop flag: %s", commandLog)
	}
	for _, forbidden := range []string{"lsjson", "list", "lsf", "hash", "size"} {
		if strings.Contains(commandLog, forbidden) {
			t.Fatalf("move invoked forbidden remote observation %q: %s", forbidden, commandLog)
		}
	}
}
