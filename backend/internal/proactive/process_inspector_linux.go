//go:build linux

package proactive

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// LinuxProcessInspector verifies both PID existence and the persisted kernel
// start-time token, preventing PID reuse from being treated as our process.
type LinuxProcessInspector struct{}

func (LinuxProcessInspector) Inspect(pid int, token string) (ProcessStatus, error) {
	if pid <= 0 || strings.TrimSpace(token) == "" {
		return ProcessStatus{}, fmt.Errorf("invalid process identity")
	}
	actual, live, exists, err := linuxProcessIdentity(pid)
	if err != nil {
		return ProcessStatus{}, err
	}
	if !exists {
		return ProcessStatus{Confirmed: true, Alive: false}, nil
	}
	matched := actual == token
	return ProcessStatus{Confirmed: matched, Alive: matched && live}, nil
}

func (LinuxProcessInspector) StopVerified(pid int, token string) error {
	actual, live, exists, err := linuxProcessIdentity(pid)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if actual != token {
		return fmt.Errorf("process %d start token mismatch", pid)
	}
	if !live {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		actual, live, exists, err = linuxProcessIdentity(pid)
		if err != nil {
			return err
		}
		if !exists || !live {
			return nil
		}
		if actual != token {
			return fmt.Errorf("process %d start token changed", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	actual, live, exists, err = linuxProcessIdentity(pid)
	if err != nil {
		return err
	}
	if !exists || !live {
		return nil
	}
	if actual != token {
		return fmt.Errorf("process %d start token changed", pid)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		return err
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		actual, live, exists, err = linuxProcessIdentity(pid)
		if err != nil {
			return err
		}
		if !exists || !live {
			return nil
		}
		if actual != token {
			return fmt.Errorf("process %d start token changed", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("process %d remained alive after identity-safe stop", pid)
}

func linuxProcessIdentity(pid int) (token string, live, exists bool, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, false, nil
		}
		return "", false, false, err
	}
	text := string(data)
	end := strings.LastIndex(text, ") ")
	if end < 0 {
		return "", false, true, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(text[end+2:])
	if len(fields) <= 19 {
		return "", false, true, fmt.Errorf("invalid process stat fields")
	}
	return strconv.Itoa(pid) + ":" + fields[19], fields[0] != "Z", true, nil
}
