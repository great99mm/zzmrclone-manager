//go:build !linux

package proactive

import "errors"

type LinuxProcessInspector struct{}

func (LinuxProcessInspector) Inspect(int, string) (ProcessStatus, error) {
	return ProcessStatus{}, errors.New("proactive process inspection is unsupported on this platform")
}
func (LinuxProcessInspector) StopVerified(int, string) error {
	return errors.New("process control unsupported on this platform")
}
