package proactive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"rclone-manager/internal/quota"
)

func TestExecRunnerClassifiesOnlyRcloneNotFoundExitCodes(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "rclone.conf")
	if err := os.WriteFile(config, []byte("[remote]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		exitCode string
		notFound bool
	}{{name: "file not found", exitCode: "4", notFound: true}, {name: "temporary failure", exitCode: "5", notFound: false}} {
		t.Run(test.name, func(t *testing.T) {
			script := filepath.Join(dir, "rclone-"+test.exitCode)
			if err := os.WriteFile(script, []byte("#!/bin/sh\nexit "+test.exitCode+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			_, err := (ExecRunner{Binary: script}).StatRemote(context.Background(), config, "remote", "/dest", "file.txt")
			if errors.Is(err, ErrRemoteObjectNotFound) != test.notFound {
				t.Fatalf("error = %v, notFound=%v", err, errors.Is(err, ErrRemoteObjectNotFound))
			}
		})
	}
}

func TestExecRunnerEnablesAccountRcloneLogs(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "rclone.conf")
	manifest := filepath.Join(root, "manifest")
	arguments := filepath.Join(root, "arguments")
	if err := os.WriteFile(config, []byte("[remote]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "runner")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + arguments + "\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	source, err := quota.OpenSourceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	runner := ExecRunner{Binary: script, ProcessStartToken: func(int) string { return "test-token" }}
	copyProcess, err := runner.StartCopy(context.Background(), CopySpec{ConfigPath: config, ManifestPath: manifest, SourceRoot: source.File(), DestinationRemote: "remote", DestinationPath: "/dest"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := copyProcess.Wait(); err != nil {
		t.Fatal(err)
	}
	moveProcess, err := runner.StartMove(context.Background(), MoveSpec{ConfigPath: config, ManifestPath: manifest, SourceRoot: source.File(), DestinationRemote: "remote", DestinationPath: "/dest"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := moveProcess.Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("runner invocations = %#v", lines)
	}
	for _, line := range lines {
		if !strings.Contains(line, "--log-level INFO") || !strings.Contains(line, "--stats-log-level INFO") || !strings.Contains(line, "--stats 1s") {
			t.Fatalf("rclone logging flags missing: %s", line)
		}
	}
}

func TestExecRunnerCopySinkIsLosslessUnderBackpressure(t *testing.T) {
	testExecRunnerSinkIsLosslessUnderBackpressure(t, func(ctx context.Context, runner ExecRunner, config, manifest string, source *quota.SourceRootHandle, sink func(ProcessOutputChunk)) (ProcessHandle, error) {
		return runner.StartCopy(ctx, CopySpec{ConfigPath: config, ManifestPath: manifest, SourceRoot: source.File(), DestinationRemote: "remote", DestinationPath: "/dest", OutputSink: sink})
	})
}

func TestExecRunnerMoveSinkIsLosslessUnderBackpressure(t *testing.T) {
	testExecRunnerSinkIsLosslessUnderBackpressure(t, func(ctx context.Context, runner ExecRunner, config, manifest string, source *quota.SourceRootHandle, sink func(ProcessOutputChunk)) (ProcessHandle, error) {
		return runner.StartMove(ctx, MoveSpec{ConfigPath: config, ManifestPath: manifest, SourceRoot: source.File(), DestinationRemote: "remote", DestinationPath: "/dest", OutputSink: sink})
	})
}

func testExecRunnerSinkIsLosslessUnderBackpressure(t *testing.T, start func(context.Context, ExecRunner, string, string, *quota.SourceRootHandle, func(ProcessOutputChunk)) (ProcessHandle, error)) {
	t.Helper()
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
	process, err := start(context.Background(), ExecRunner{Binary: script}, config, manifest, source, func(chunk ProcessOutputChunk) {
		mu.Lock()
		defer mu.Unlock()
		output.WriteString(chunk.Data)
	})
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
