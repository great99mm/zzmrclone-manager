//go:build !linux

package proactive

import "os/exec"

func setProbePdeathsig(*exec.Cmd) {}
