//go:build linux

package proactive

import (
	"os/exec"
	"syscall"
)

func setProbePdeathsig(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
