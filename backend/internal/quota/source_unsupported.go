//go:build !linux

package quota

import (
	"errors"
	"os"
)

type SourceRootHandle struct{}
type StageHandle struct{}
type MoveQuarantine struct{}

func OpenSourceRoot(string) (*SourceRootHandle, error) {
	return nil, errors.New("descriptor-relative source validation is unsupported on this platform")
}
func (h *SourceRootHandle) File() *os.File { return nil }
func (h *SourceRootHandle) Close() error   { return nil }
func (h *SourceRootHandle) Validate(LocalSnapshot) (bool, error) {
	return false, errors.New("descriptor-relative source validation is unsupported on this platform")
}
func (h *SourceRootHandle) OpenValidated(LocalSnapshot) (*os.File, error) {
	return nil, errors.New("descriptor-relative source validation is unsupported on this platform")
}
func PrepareStage(string, uint, string, string) (*StageHandle, error) {
	return nil, errors.New("descriptor-relative staging is unsupported on this platform")
}
func (s *StageHandle) File() *os.File        { return nil }
func (s *StageHandle) SetBeforeClone(func()) {}
func (s *StageHandle) Snapshot(LocalSnapshot, *os.File) error {
	return errors.New("descriptor-relative staging is unsupported on this platform")
}
func (s *StageHandle) Validate(LocalSnapshot) error {
	return errors.New("descriptor-relative staging is unsupported on this platform")
}
func (s *StageHandle) Cleanup() error        { return nil }
func (s *StageHandle) Close() error          { return nil }
func SourceRootProcessToken(*os.File) string { return "" }
func PrepareMoveQuarantine(*SourceRootHandle, uint, string) (*MoveQuarantine, error) {
	return nil, errors.New("descriptor-relative move quarantine is unsupported on this platform")
}
func OpenMoveQuarantine(*SourceRootHandle, uint, string) (*MoveQuarantine, error) {
	return nil, errors.New("descriptor-relative move quarantine is unsupported on this platform")
}
func (q *MoveQuarantine) Path() string   { return "" }
func (q *MoveQuarantine) File() *os.File { return nil }
func (q *MoveQuarantine) Identity() (int64, int64, error) {
	return 0, 0, errors.New("descriptor-relative move quarantine is unsupported on this platform")
}
func (q *MoveQuarantine) Move(string, LocalSnapshot) (int64, int64, error) {
	return 0, 0, errors.New("descriptor-relative move quarantine is unsupported on this platform")
}
func (q *MoveQuarantine) Restore(string, LocalSnapshot) error {
	return errors.New("descriptor-relative move quarantine is unsupported on this platform")
}
func (q *MoveQuarantine) Present(string, LocalSnapshot) (bool, int64, int64, error) {
	return false, 0, 0, errors.New("descriptor-relative move quarantine is unsupported on this platform")
}
func (q *MoveQuarantine) Close() error { return nil }
