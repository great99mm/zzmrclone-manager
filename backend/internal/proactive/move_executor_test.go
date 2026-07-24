package proactive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/quota"
)

type moveTestRunner struct {
	startErr  error
	remove    func(string, *os.File) error
	statCalls int
	moveSpec  MoveSpec
}

func (r *moveTestRunner) StartCopy(context.Context, CopySpec) (ProcessHandle, error) {
	return nil, errors.New("copy runner should not be used for move")
}

func (r *moveTestRunner) StatRemote(context.Context, string, string, string, string) (RemoteObject, error) {
	r.statCalls++
	return RemoteObject{}, errors.New("remote stat must not be called for move")
}

func (r *moveTestRunner) StartMove(_ context.Context, spec MoveSpec) (ProcessHandle, error) {
	r.moveSpec = spec
	if r.startErr != nil {
		return nil, r.startErr
	}
	if r.remove != nil {
		if err := r.remove(spec.ManifestPath, spec.SourceRoot); err != nil {
			return nil, err
		}
	}
	return &fakeProcess{result: ProcessResult{PID: 77, ProcessStartToken: "77:1"}}, nil
}

type moveFixture struct {
	batch models.RotationQuotaBatch
	files []models.RotationQuotaBatchFile
	root  string
}

func makeMoveFixture(t *testing.T, db *gorm.DB, count int) moveFixture {
	t.Helper()
	root := t.TempDir()
	config := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(config, []byte("[test]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	account := models.QuotaAccount{QuotaKey: "move-key", BudgetBytes: 1000, Enabled: true, WindowSeconds: 3600}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	var snapshots []quota.LocalSnapshot
	for i := 0; i < count; i++ {
		relative := filepath.ToSlash(filepath.Join("nested", fmt.Sprintf("selected-%d.bin", i)))
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Repeat("x", i+3)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "unselected.bin"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshots, err := (quota.Scanner{}).Scan(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	batch := models.RotationQuotaBatch{TaskID: 1, QuotaAccountID: account.ID, DestinationScope: models.DestinationScope(config, "/dest"), SourceRoot: root, SourceRootDevice: snapshots[0].RootDevice, SourceRootInode: snapshots[0].RootInode, DestinationRemote: "remote", TransferMode: models.TransferModeMove, RcloneTransfers: 16, DestinationScopeVersion: 1, RcloneConfigPath: config, RequestKey: "move-request", RequestFingerprint: "move-fingerprint", DestinationPath: "/dest", State: models.BatchStateReserved, OwnerToken: testOwner}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	files := make([]models.RotationQuotaBatchFile, 0, count)
	for i, snapshot := range snapshots {
		if snapshot.RelativePath == "unselected.bin" {
			continue
		}
		file := models.RotationQuotaBatchFile{BatchID: batch.ID, RelativePath: snapshot.RelativePath, SnapshotKey: fmt.Sprintf("move-snapshot-%d", i), SizeBytes: snapshot.SizeBytes, MtimeNS: snapshot.MtimeNS, Device: snapshot.Device, Inode: snapshot.Inode, State: models.BatchFileStateHeld}
		if err := db.Create(&file).Error; err != nil {
			t.Fatal(err)
		}
		expires := time.Now().Add(time.Hour)
		if err := db.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchID: batch.ID, BatchFileID: file.ID, Bytes: file.SizeBytes, State: models.ReservationStateHeld, IdempotencyKey: fmt.Sprintf("move-reservation-%d", i), ExpiresAt: &expires}).Error; err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	return moveFixture{batch: batch, files: files, root: root}
}

func removeManifestFiles(manifestPath string, sourceRoot *os.File, removeFirstOnly bool) error {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	paths := strings.Split(strings.TrimSpace(string(data)), "\n")
	if removeFirstOnly && len(paths) > 1 {
		paths = paths[:1]
	}
	for _, relative := range paths {
		if relative == "" {
			continue
		}
		if err := os.Remove(filepath.Join(fmt.Sprintf("/proc/self/fd/%d", sourceRoot.Fd()), filepath.FromSlash(relative))); err != nil {
			return err
		}
	}
	return nil
}

func TestMoveExecutorMovesOnlyManifestFilesAndUsesNoRemoteStat(t *testing.T) {
	db := executionDB(t)
	fixture := makeMoveFixture(t, db, 2)
	runner := &moveTestRunner{remove: func(manifest string, root *os.File) error { return removeManifestFiles(manifest, root, false) }}
	executor := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: runner, Manifest: ManifestWriter{}, MoveEnabled: func() bool { return true }}
	if err := executor.RunBatch(context.Background(), fixture.batch.ID); err != nil {
		t.Fatal(err)
	}
	for _, file := range fixture.files {
		if _, err := os.Stat(filepath.Join(fixture.root, filepath.FromSlash(file.RelativePath))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("selected source remains: %s err=%v", file.RelativePath, err)
		}
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "unselected.bin")); err != nil {
		t.Fatalf("unselected source was changed: %v", err)
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, fixture.batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateSucceeded || stored.CompletionEvidence != models.CompletionEvidenceLocal || runner.statCalls != 0 {
		t.Fatalf("move result=%#v statCalls=%d", stored, runner.statCalls)
	}
	if !strings.Contains(runner.moveSpec.DestinationRemote+":"+runner.moveSpec.DestinationPath, "remote:/dest") {
		t.Fatalf("unexpected move destination: %#v", runner.moveSpec)
	}
	if runner.moveSpec.Transfers != 16 {
		t.Fatalf("move transfers=%d, want 16", runner.moveSpec.Transfers)
	}
}

func TestMoveExecutorStartFailureRestoresAndReleases(t *testing.T) {
	db := executionDB(t)
	fixture := makeMoveFixture(t, db, 1)
	runner := &moveTestRunner{startErr: errors.New("rclone start failed")}
	executor := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: runner, Manifest: ManifestWriter{}, MoveEnabled: func() bool { return true }}
	if err := executor.RunBatch(context.Background(), fixture.batch.ID); err == nil {
		var stored models.RotationQuotaBatch
		_ = db.First(&stored, fixture.batch.ID).Error
		t.Fatalf("move start failure unexpectedly succeeded: state=%s runner=%#v", stored.State, runner)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, filepath.FromSlash(fixture.files[0].RelativePath))); err != nil {
		t.Fatalf("source was not restored: %v", err)
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, fixture.batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateFailed {
		t.Fatalf("batch state = %s, want failed", stored.State)
	}
	var reservation models.QuotaReservation
	if err := db.Where("batch_id = ?", fixture.batch.ID).First(&reservation).Error; err != nil {
		t.Fatal(err)
	}
	if reservation.State != models.ReservationStateReleased {
		t.Fatalf("reservation state = %s, want released", reservation.State)
	}
}

func TestMoveRecoveryRestoresRenameWhenHandoffStateUpdateWasLost(t *testing.T) {
	db := executionDB(t)
	fixture := makeMoveFixture(t, db, 1)
	root, err := quota.OpenSourceRoot(fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	quarantine, err := quota.PrepareMoveQuarantine(root, fixture.batch.ID, fixture.batch.OwnerToken)
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	device, inode, err := quarantine.Identity()
	if err != nil {
		t.Fatal(err)
	}
	file := fixture.files[0]
	if _, _, err := quarantine.Move(file.RelativePath, quota.LocalSnapshot{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode}); err != nil {
		t.Fatal(err)
	}
	_ = quarantine.Close()
	_ = root.Close()
	if err := db.Model(&models.RotationQuotaBatch{}).Where("id = ?", fixture.batch.ID).Updates(map[string]interface{}{"lease_token": "recovery-lease", "state": models.BatchStatePlanned, "move_handoff_contract_version": models.MoveHandoffVersion, "move_quarantine_path": filepath.Join(fixture.root, ".rclone-manager-move", fmt.Sprintf("%d-%s", fixture.batch.ID, fixture.batch.OwnerToken)), "move_quarantine_device": device, "move_quarantine_inode": inode}).Error; err != nil {
		t.Fatal(err)
	}
	executor := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: &moveTestRunner{}, Manifest: ManifestWriter{}, MoveEnabled: func() bool { return true }}
	if err := executor.recoverMoveBatch(context.Background(), fixture.batch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, filepath.FromSlash(file.RelativePath))); err != nil {
		t.Fatalf("lost handoff file was not restored: %v", err)
	}
	var reservation models.QuotaReservation
	if err := db.Where("batch_id = ?", fixture.batch.ID).First(&reservation).Error; err != nil {
		t.Fatal(err)
	}
	if reservation.State != models.ReservationStateReleased {
		t.Fatalf("reservation state = %s, want released", reservation.State)
	}
}

func persistLostMoveHandoff(t *testing.T, db *gorm.DB, fixture moveFixture) (int64, int64) {
	t.Helper()
	root, err := quota.OpenSourceRoot(fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	quarantine, err := quota.PrepareMoveQuarantine(root, fixture.batch.ID, fixture.batch.OwnerToken)
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	device, inode, err := quarantine.Identity()
	if err != nil {
		t.Fatal(err)
	}
	file := fixture.files[0]
	if _, _, err := quarantine.Move(file.RelativePath, quota.LocalSnapshot{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode}); err != nil {
		t.Fatal(err)
	}
	_ = quarantine.Close()
	_ = root.Close()
	path := filepath.Join(fixture.root, ".rclone-manager-move", fmt.Sprintf("%d-%s", fixture.batch.ID, fixture.batch.OwnerToken))
	if err := db.Model(&models.RotationQuotaBatch{}).Where("id = ?", fixture.batch.ID).Updates(map[string]interface{}{"lease_token": "recovery-lease", "state": models.BatchStatePlanned, "move_handoff_contract_version": models.MoveHandoffVersion, "move_quarantine_path": path, "move_quarantine_device": device, "move_quarantine_inode": inode}).Error; err != nil {
		t.Fatal(err)
	}
	return device, inode
}

func TestMoveRecoveryFreezesOnSourceRootReplacement(t *testing.T) {
	db := executionDB(t)
	fixture := makeMoveFixture(t, db, 1)
	persistLostMoveHandoff(t, db, fixture)
	if err := db.Model(&models.RotationQuotaBatch{}).Where("id = ?", fixture.batch.ID).Update("source_root_device", fixture.batch.SourceRootDevice+1).Error; err != nil {
		t.Fatal(err)
	}
	executor := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: &moveTestRunner{}, Manifest: ManifestWriter{}, MoveEnabled: func() bool { return true }}
	if err := executor.recoverMoveBatch(context.Background(), fixture.batch.ID); err == nil {
		t.Fatal("source root replacement was not frozen")
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, fixture.batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateUnknown {
		t.Fatalf("state = %s, want unknown", stored.State)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, filepath.FromSlash(fixture.files[0].RelativePath))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source replacement path was mutated: %v", err)
	}
}

func TestMoveRecoveryFreezesOnQuarantineReplacement(t *testing.T) {
	db := executionDB(t)
	fixture := makeMoveFixture(t, db, 1)
	device, _ := persistLostMoveHandoff(t, db, fixture)
	if err := db.Model(&models.RotationQuotaBatch{}).Where("id = ?", fixture.batch.ID).Update("move_quarantine_device", device+1).Error; err != nil {
		t.Fatal(err)
	}
	executor := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: &moveTestRunner{}, Manifest: ManifestWriter{}, MoveEnabled: func() bool { return true }}
	if err := executor.recoverMoveBatch(context.Background(), fixture.batch.ID); err == nil {
		t.Fatal("quarantine replacement was not frozen")
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, fixture.batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateUnknown {
		t.Fatalf("state = %s, want unknown", stored.State)
	}
}

func TestMoveRecoveryReleasesUnstartedBatchWithoutHandoff(t *testing.T) {
	db := executionDB(t)
	fixture := makeMoveFixture(t, db, 1)
	executor := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: &moveTestRunner{}, Manifest: ManifestWriter{}, MoveEnabled: func() bool { return false }}
	err := executor.recoverMoveBatch(context.Background(), fixture.batch.ID)
	if !errors.Is(err, ErrRetryableExecutor) {
		t.Fatalf("recovery error = %v, want retryable", err)
	}
	var batch models.RotationQuotaBatch
	if err := db.First(&batch, fixture.batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if batch.State != models.BatchStateFailed {
		t.Fatalf("batch state = %s, want failed", batch.State)
	}
	var reservation models.QuotaReservation
	if err := db.Where("batch_id = ?", fixture.batch.ID).First(&reservation).Error; err != nil {
		t.Fatal(err)
	}
	if reservation.State != models.ReservationStateReleased {
		t.Fatalf("reservation state = %s, want released", reservation.State)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, filepath.FromSlash(fixture.files[0].RelativePath))); err != nil {
		t.Fatalf("source was mutated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, ".rclone-manager-move")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantine was mutated: %v", err)
	}
}

func TestDispatcherRecoverRestoresPreStartMoveBeforeGroupExecution(t *testing.T) {
	db := dispatcherDB(t)
	fixture := makeMoveFixture(t, db, 1)
	persistLostMoveHandoff(t, db, fixture)
	task := models.Task{ID: fixture.batch.TaskID, Name: "move-recovery", SourceType: "local", SourceDir: fixture.root, DestType: "remote", RemoteName: "remote", RemoteDir: "/dest", TransferMode: string(models.TransferModeMove), RcloneConfig: fixture.batch.RcloneConfigPath, TaskType: "rotation", RotationStrategy: "proactive_quota", RotationRemotes: `["remote"]`, MinAge: "0s", Enabled: true, RotationQuotaLimitBytes: 1000}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	executor := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: &moveTestRunner{}, Manifest: ManifestWriter{}, MoveEnabled: func() bool { return false }}
	dispatcher := &Dispatcher{DB: db, Quota: &quota.Service{DB: db, ConfigResolver: func(raw string) (string, error) { return raw, nil }, Now: func() time.Time { return time.Unix(100, 0) }}, Executor: executor, Now: func() time.Time { return time.Unix(100, 0) }, ManagerDataDir: t.TempDir()}
	if err := dispatcher.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, filepath.FromSlash(fixture.files[0].RelativePath))); err != nil {
		t.Fatalf("source was not restored: %v", err)
	}
	var reservation models.QuotaReservation
	if err := db.Where("batch_id = ?", fixture.batch.ID).First(&reservation).Error; err != nil {
		t.Fatal(err)
	}
	if reservation.State != models.ReservationStateReleased {
		t.Fatalf("reservation state = %s, want released", reservation.State)
	}
}

func TestDispatcherRecoverRetriesUnstartedMoveWithoutUnknownQuotaBlock(t *testing.T) {
	db := dispatcherDB(t)
	fixture := makeMoveFixture(t, db, 1)
	task := models.Task{ID: fixture.batch.TaskID, Name: "move-retry", SourceType: "local", SourceDir: fixture.root, DestType: "remote", RemoteName: "remote", RemoteDir: "/dest", TransferMode: string(models.TransferModeMove), RcloneConfig: fixture.batch.RcloneConfigPath, TaskType: "rotation", RotationStrategy: "proactive_quota", RotationRemotes: `["remote"]`, MinAge: "0s", Enabled: true, RotationQuotaLimitBytes: 1000}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	executor := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: &moveTestRunner{}, Manifest: ManifestWriter{}, MoveEnabled: func() bool { return false }}
	dispatcher := &Dispatcher{DB: db, Quota: &quota.Service{DB: db, ConfigResolver: func(raw string) (string, error) { return raw, nil }, Now: func() time.Time { return time.Unix(100, 0) }}, Executor: executor, Now: func() time.Time { return time.Unix(100, 0) }, ManagerDataDir: t.TempDir()}
	if err := dispatcher.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	var batch models.RotationQuotaBatch
	if err := db.First(&batch, fixture.batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if batch.State == models.BatchStateUnknown {
		t.Fatal("pre-handoff batch was marked unknown")
	}
	var reservation models.QuotaReservation
	if err := db.Where("batch_id = ?", fixture.batch.ID).First(&reservation).Error; err != nil {
		t.Fatal(err)
	}
	if reservation.State != models.ReservationStateReleased {
		t.Fatalf("reservation state = %s, want released", reservation.State)
	}
	var stored models.Task
	if err := db.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RotationQuotaWakeAt == nil || stored.LastError == "" {
		t.Fatalf("retry evidence missing: wake=%v error=%q", stored.RotationQuotaWakeAt, stored.LastError)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, filepath.FromSlash(fixture.files[0].RelativePath))); err != nil {
		t.Fatalf("source was mutated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, ".rclone-manager-move")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantine was mutated: %v", err)
	}
}

func TestMoveExecutorMixedLocalPresenceFreezesRetainedFile(t *testing.T) {
	db := executionDB(t)
	fixture := makeMoveFixture(t, db, 2)
	runner := &moveTestRunner{remove: func(manifest string, root *os.File) error { return removeManifestFiles(manifest, root, true) }}
	executor := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: runner, Manifest: ManifestWriter{}, MoveEnabled: func() bool { return true }}
	if err := executor.RunBatch(context.Background(), fixture.batch.ID); err != nil {
		t.Fatal(err)
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, fixture.batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateUnknown {
		t.Fatalf("mixed move state = %s, want unknown", stored.State)
	}
	var unknown int64
	if err := db.Model(&models.RotationQuotaBatchFile{}).Where("batch_id = ? AND state = ?", fixture.batch.ID, models.BatchFileStateUnknown).Count(&unknown).Error; err != nil {
		t.Fatal(err)
	}
	if unknown != 1 {
		t.Fatalf("unknown file count = %d, want 1", unknown)
	}
}
