package proactive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rclone-manager/internal/models"
)

func TestExecRunnerProbeUploadUsesExactSizeAndTarget(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "args.log")
	script := filepath.Join(dir, "rclone-probe")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PROBE_LOG\"\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	old := os.Getenv("PROBE_LOG")
	if err := os.Setenv("PROBE_LOG", logPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("PROBE_LOG", old) })

	runner := ExecRunner{Binary: script, ProcessStartToken: func(pid int) string { return fmt.Sprintf("%d:test", pid) }}
	process, err := runner.StartProbeUpload(context.Background(), ProbeUploadSpec{ConfigPath: "/tmp/rclone.conf", Remote: "remote", ObjectPath: ".probe-object", ExpectedBytes: models.ProbeExpectedBytes})
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
	command := string(data)
	for _, expected := range []string{"rcat", "--size=1073741824", "remote:.probe-object"} {
		if !strings.Contains(command, expected) {
			t.Fatalf("probe command %q lacks %q", command, expected)
		}
	}
	if execProcess, ok := process.(*execProcess); !ok || execProcess.cmd.SysProcAttr == nil {
		t.Fatal("probe process did not receive process attributes")
	}
}

func TestProbeZeroReaderProducesZeros(t *testing.T) {
	buffer := make([]byte, 4096)
	reader := zeroReader{}
	if n, err := reader.Read(buffer); err != nil || n != len(buffer) {
		t.Fatalf("zero reader read n=%d err=%v", n, err)
	}
	for i, value := range buffer {
		if value != 0 {
			t.Fatalf("zero reader byte %d = %d", i, value)
		}
	}
}

func TestExecRunnerProbeVerifyRequiresOneExactObject(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "rclone-probe-stat")
	body := "#!/bin/sh\nfor arg in \"$@\"; do if [ \"$arg\" = lsjson ]; then printf '%s' '{\"Path\":\".probe-object\",\"Size\":1073741824,\"IsDir\":false}'; exit 0; fi; done\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	runner := ExecRunner{Binary: script}
	result, err := runner.VerifyProbeObject(context.Background(), "/tmp/rclone.conf", "remote", ".probe-object", models.ProbeExpectedBytes)
	if err != nil || !result.Exact || result.ObjectCount != 1 {
		t.Fatalf("probe verification = %#v err=%v", result, err)
	}
}

func TestExecRunnerProbeVerifyClassifiesRcloneMissingObject(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "rclone-probe-missing")
	body := "#!/bin/sh\nprintf '%s' 'Failed to lsjson: directory not found' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	runner := ExecRunner{Binary: script}
	result, err := runner.VerifyProbeObject(context.Background(), "/tmp/rclone.conf", "remote", ".probe-object", models.ProbeExpectedBytes)
	if err != nil {
		t.Fatalf("missing object returned error: %v", err)
	}
	if result.Exists || result.Exact || result.ObjectCount != 0 || !strings.Contains(result.Evidence, "directory not found") {
		t.Fatalf("missing object result = %#v", result)
	}
}

func TestExecRunnerProbeVerifyDoesNotClassifyGenericErrorAsAbsence(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "rclone-probe-network-error")
	body := "#!/bin/sh\nprintf '%s' 'Failed to lsjson: temporary network error' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	runner := ExecRunner{Binary: script}
	result, err := runner.VerifyProbeObject(context.Background(), "/tmp/rclone.conf", "remote", ".probe-object", models.ProbeExpectedBytes)
	if err == nil {
		t.Fatal("generic rclone error was classified as absence")
	}
	if result.Exists || result.Exact {
		t.Fatalf("generic error result = %#v", result)
	}
}

func TestExecRunnerProbeDeleteConfirmsAbsenceAfterSuccessfulDelete(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "rclone-probe-delete")
	body := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = deletefile ]; then exit 0; fi\ndone\nprintf '%s' 'Failed to lsjson: directory not found' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	runner := ExecRunner{Binary: script}
	result, err := runner.DeleteProbeObject(context.Background(), "/tmp/rclone.conf", "remote", ".probe-object")
	if err != nil || !result.Absent {
		t.Fatalf("delete result = %#v err=%v", result, err)
	}
}
