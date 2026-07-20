package proactive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/quota"
)

func prepareUnknownMoveResolution(t *testing.T, db *gorm.DB, fixture moveFixture) (models.RotationQuotaBatchFile, string) {
	t.Helper()
	persistLostMoveHandoff(t, db, fixture)
	root, err := quota.OpenSourceRoot(fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	quarantine, err := quota.OpenMoveQuarantine(root, fixture.batch.ID, fixture.batch.OwnerToken)
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	snapshot := quota.LocalSnapshot{RelativePath: fixture.files[0].RelativePath, SizeBytes: fixture.files[0].SizeBytes, MtimeNS: fixture.files[0].MtimeNS, Device: fixture.files[0].Device, Inode: fixture.files[0].Inode}
	_, qDevice, qInode, err := quarantine.Present(snapshot.RelativePath, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(quarantine.Path(), filepath.FromSlash(snapshot.RelativePath))
	_ = quarantine.Close()
	_ = root.Close()
	if err := db.Model(&models.RotationQuotaBatch{}).Where("id = ?", fixture.batch.ID).Updates(map[string]interface{}{"state": models.BatchStateUnknown}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.RotationQuotaBatchFile{}).Where("id = ?", fixture.files[0].ID).Updates(map[string]interface{}{"state": models.BatchFileStateUnknown, "move_handoff_state": models.MoveHandoffUnknown, "move_handoff_device": qDevice, "move_handoff_inode": qInode}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.QuotaReservation{}).Where("batch_file_id = ?", fixture.files[0].ID).Update("state", models.ReservationStateUnknown).Error; err != nil {
		t.Fatal(err)
	}
	var file models.RotationQuotaBatchFile
	if err := db.First(&file, fixture.files[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	return file, path
}

func TestResolveUnknownMoveAcceptMovedUsesCASAndLocalEvidence(t *testing.T) {
	db := executionDB(t)
	fixture := makeMoveFixture(t, db, 1)
	file, quarantinePath := prepareUnknownMoveResolution(t, db, fixture)
	if err := os.Remove(quarantinePath); err != nil {
		t.Fatal(err)
	}
	result, err := ResolveUnknownMoveFile(db, MoveResolutionRequest{BatchID: fixture.batch.ID, FileID: file.ID, Action: "accept_moved", ExpectedState: models.BatchFileStateUnknown, ExpectedUpdatedAt: file.UpdatedAt})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != models.BatchStateSucceeded {
		t.Fatalf("batch state = %s, want succeeded", result.State)
	}
	var reservation models.QuotaReservation
	if err := db.Where("batch_file_id = ?", file.ID).First(&reservation).Error; err != nil {
		t.Fatal(err)
	}
	if reservation.State != models.ReservationStateCommitted {
		t.Fatalf("reservation state = %s, want committed", reservation.State)
	}
	if _, err := ResolveUnknownMoveFile(db, MoveResolutionRequest{BatchID: fixture.batch.ID, FileID: file.ID, Action: "accept_moved", ExpectedState: models.BatchFileStateUnknown, ExpectedUpdatedAt: file.UpdatedAt}); !errors.Is(err, ErrMoveResolutionConflict) {
		t.Fatalf("stale resolution error = %v, want conflict", err)
	}
}

func TestResolveUnknownMoveRestoreRejectsSourceCollision(t *testing.T) {
	db := executionDB(t)
	fixture := makeMoveFixture(t, db, 1)
	file, _ := prepareUnknownMoveResolution(t, db, fixture)
	source := filepath.Join(fixture.root, filepath.FromSlash(file.RelativePath))
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("collision"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveUnknownMoveFile(db, MoveResolutionRequest{BatchID: fixture.batch.ID, FileID: file.ID, Action: "restore_and_release", ExpectedState: models.BatchFileStateUnknown, ExpectedUpdatedAt: file.UpdatedAt}); !errors.Is(err, ErrMoveResolutionEvidence) {
		t.Fatalf("collision error = %v, want evidence error", err)
	}
	var reservation models.QuotaReservation
	if err := db.Where("batch_file_id = ?", file.ID).First(&reservation).Error; err != nil {
		t.Fatal(err)
	}
	if reservation.State != models.ReservationStateUnknown {
		t.Fatalf("reservation state = %s, want unknown", reservation.State)
	}
	var stored models.RotationQuotaBatchFile
	if err := db.First(&stored, file.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.LastError == "" {
		t.Fatal("collision evidence was not persisted")
	}
}

func TestResolveUnknownMoveRestoresExactQuarantineAndReleasesReservation(t *testing.T) {
	db := executionDB(t)
	fixture := makeMoveFixture(t, db, 1)
	file, _ := prepareUnknownMoveResolution(t, db, fixture)
	result, err := ResolveUnknownMoveFile(db, MoveResolutionRequest{BatchID: fixture.batch.ID, FileID: file.ID, Action: "restore_and_release", ExpectedState: models.BatchFileStateUnknown, ExpectedUpdatedAt: file.UpdatedAt})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != models.BatchStateFailed {
		t.Fatalf("batch state = %s, want failed", result.State)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, filepath.FromSlash(file.RelativePath))); err != nil {
		t.Fatalf("restored source missing: %v", err)
	}
	var reservation models.QuotaReservation
	if err := db.Where("batch_file_id = ?", file.ID).First(&reservation).Error; err != nil {
		t.Fatal(err)
	}
	if reservation.State != models.ReservationStateReleased || reservation.ReleasedAt == nil {
		t.Fatalf("reservation was not released durably: %#v", reservation)
	}
}

func TestMoveResolutionRestartFinalizesResolvingRestoreClaim(t *testing.T) {
	db := executionDB(t)
	fixture := makeMoveFixture(t, db, 1)
	file, _ := prepareUnknownMoveResolution(t, db, fixture)
	root, err := quota.OpenSourceRoot(fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	quarantine, err := quota.OpenMoveQuarantine(root, fixture.batch.ID, fixture.batch.OwnerToken)
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	if err := quarantine.Restore(file.RelativePath, quota.LocalSnapshot{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode}); err != nil {
		t.Fatal(err)
	}
	_ = quarantine.Close()
	_ = root.Close()
	if err := db.Model(&models.RotationQuotaBatchFile{}).Where("id = ?", file.ID).Updates(map[string]interface{}{"move_resolution_state": models.MoveResolutionResolving, "move_resolution_token": "restart-token", "move_resolution_started_at": time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	executor := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: &moveTestRunner{}, MoveEnabled: func() bool { return true }}
	if err := executor.recoverMoveBatch(context.Background(), fixture.batch.ID); err != nil {
		t.Fatal(err)
	}
	var stored models.RotationQuotaBatchFile
	if err := db.First(&stored, file.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchFileStateFailed || stored.MoveResolutionState != "" {
		t.Fatalf("resolving claim was not finalized: %#v", stored)
	}
	var reservation models.QuotaReservation
	if err := db.Where("batch_file_id = ?", file.ID).First(&reservation).Error; err != nil {
		t.Fatal(err)
	}
	if reservation.State != models.ReservationStateReleased {
		t.Fatalf("restart finalization reservation = %s", reservation.State)
	}
}
