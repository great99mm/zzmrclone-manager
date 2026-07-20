//go:build linux

package proactive

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestLinuxProcessInspectorStopsOnlyMatchingProcessIdentity(t *testing.T) {
	command := exec.Command("sleep", "10")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Wait()
	token := processStartToken(command.Process.Pid)
	if token == "" {
		t.Fatal("process start token unavailable")
	}
	inspector := LinuxProcessInspector{}
	if err := inspector.StopVerified(command.Process.Pid, token); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ProcessState == nil || !syscall.WaitStatus(exitErr.ProcessState.Sys().(syscall.WaitStatus)).Signaled() {
			t.Fatalf("matching process was not reaped: %v", err)
		}
	}
	if status, err := inspector.Inspect(command.Process.Pid, token); err != nil || status.Alive {
		t.Fatalf("process still executable: status=%#v err=%v", status, err)
	}

	command = exec.Command("sleep", "10")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	wrong := token + "-replacement"
	if status, err := inspector.Inspect(command.Process.Pid, wrong); err != nil || status.Confirmed {
		t.Fatalf("mismatched process identity was confirmed: status=%#v err=%v", status, err)
	}
	if err := inspector.StopVerified(command.Process.Pid, wrong); err == nil {
		t.Fatal("mismatched process identity was stopped")
	}
	actual := processStartToken(command.Process.Pid)
	if status, err := inspector.Inspect(command.Process.Pid, actual); err != nil || !status.Alive {
		t.Fatalf("mismatched process was not left running: status=%#v err=%v", status, err)
	}
	_ = command.Process.Kill()
	_ = command.Wait()
}
