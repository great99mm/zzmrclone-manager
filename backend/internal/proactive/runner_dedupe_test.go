package proactive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecRunnerDedupeUsesRootRemoteAndDirectArgv(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "args.log")
	config := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(config, []byte("config"), 0600); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "rclone")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" > \""+logPath+"\"\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	process, err := (ExecRunner{Binary: wrapper}).StartDedupe(context.Background(), DedupeSpec{ConfigPath: config, Remote: "first", DestinationPath: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	args := string(data)
	for _, expected := range []string{"--config", config, "dedupe", "first:", "--dedupe-mode newest", "-v"} {
		if !strings.Contains(args, expected) {
			t.Fatalf("dedupe argv missing %q: %s", expected, args)
		}
	}
}
