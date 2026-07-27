package proactive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"rclone-manager/internal/quota"
)

func TestExecRunnerCopySinkIsLosslessUnderBackpressure(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "rclone.conf")
	if err := os.WriteFile(config, []byte("[remote]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "manifest")
	if err := os.WriteFile(manifest, []byte("file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "runner")
	body := "#!/bin/sh\nfor i in $(seq 1 1000); do printf 'chunk-%s\\n' \"$i\"; done\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	source, err := quota.OpenSourceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	var mu sync.Mutex
	var output strings.Builder
	process, err := (ExecRunner{Binary: script}).StartCopy(context.Background(), CopySpec{ConfigPath: config, ManifestPath: manifest, SourceRoot: source.File(), DestinationRemote: "remote", DestinationPath: "/dest", OutputSink: func(chunk ProcessOutputChunk) {
		mu.Lock()
		defer mu.Unlock()
		output.WriteString(chunk.Data)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	data := output.String()
	mu.Unlock()
	if !strings.Contains(data, "chunk-1\n") || !strings.Contains(data, "chunk-1000\n") {
		t.Fatalf("stream sink lost output: first/last missing, bytes=%d", len(data))
	}
	if count := strings.Count(data, "chunk-"); count != 1000 {
		t.Fatalf("stream sink delivered %d chunks, want 1000", count)
	}
}
