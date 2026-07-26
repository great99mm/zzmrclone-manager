package proactive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/quota"
)

type fakeProcess struct {
	result  ProcessResult
	stopped bool
	stopErr error
	waitErr error
}

func (p *fakeProcess) Wait() (ProcessResult, error) { return p.result, p.waitErr }
func (p *fakeProcess) Stop() error                  { p.stopped = true; return p.stopErr }
func (p *fakeProcess) PID() int                     { return p.result.PID }
func (p *fakeProcess) StartToken() string           { return p.result.ProcessStartToken }

type fakeRunner struct {
	spec       CopySpec
	sourceLink string
	process    *fakeProcess
	object     RemoteObject
	statErr    error
	startErr   error
}

type fakeDedupeRunner struct{ process *fakeProcess }

func (r *fakeDedupeRunner) StartCopy(context.Context, CopySpec) (ProcessHandle, error) {
	return nil, errors.New("copy runner unused")
}

func (r *fakeDedupeRunner) StatRemote(context.Context, string, string, string, string) (RemoteObject, error) {
	return RemoteObject{}, errors.New("remote stat unused")
}

func (r *fakeDedupeRunner) StartDedupe(context.Context, DedupeSpec) (ProcessHandle, error) {
	return r.process, nil
}

const testOwner = "0123456789abcdef0123456789abcdef0123456789abcdef"

func (r *fakeRunner) StartCopy(_ context.Context, spec CopySpec) (ProcessHandle, error) {
	r.spec = spec
	if spec.SourceRoot != nil {
		r.sourceLink, _ = os.Readlink(fmt.Sprintf("/proc/self/fd/%d", spec.SourceRoot.Fd()))
	}
	if r.startErr != nil {
		return nil, r.startErr
	}
	return r.process, nil
}
func (r *fakeRunner) StatRemote(context.Context, string, string, string, string) (RemoteObject, error) {
	return r.object, r.statErr
}

func executionDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "execution.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.QuotaAccount{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &models.QuotaReservation{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func executionFixture(t *testing.T, db *gorm.DB, size int64) (models.RotationQuotaBatch, models.RotationQuotaBatchFile, string) {
	t.Helper()
	root := t.TempDir()
	filePath := filepath.Join(root, "file.txt")
	if err := os.WriteFile(filePath, []byte("execution"), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(config, []byte("[test]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshots, err := (quota.Scanner{}).Scan(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := snapshots[0]
	account := models.QuotaAccount{QuotaKey: "key", BudgetBytes: 100, Enabled: true, WindowSeconds: 3600}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	batch := models.RotationQuotaBatch{TaskID: 1, QuotaAccountID: account.ID, DestinationScope: models.DestinationScope(config, "/dest"), SourceRoot: root, SourceRootDevice: snapshot.RootDevice, SourceRootInode: snapshot.RootInode, DestinationRemote: "remote", TransferMode: models.TransferModeCopy, RcloneTransfers: 16, DestinationScopeVersion: 1, RcloneConfigPath: config, RequestKey: "request", RequestFingerprint: "fingerprint", DestinationPath: "/dest", State: models.BatchStateReserved, OwnerToken: testOwner, LeaseToken: ""}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	file := models.RotationQuotaBatchFile{BatchID: batch.ID, RelativePath: snapshot.RelativePath, SnapshotKey: snapshot.SnapshotKey, SizeBytes: snapshot.SizeBytes, MtimeNS: snapshot.MtimeNS, Device: snapshot.Device, Inode: snapshot.Inode, State: models.BatchFileStateHeld}
	if err := db.Create(&file).Error; err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	if err := db.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchID: batch.ID, BatchFileID: file.ID, Bytes: snapshot.SizeBytes, State: models.ReservationStateHeld, IdempotencyKey: "reservation", ExpiresAt: &expires}).Error; err != nil {
		t.Fatal(err)
	}
	return batch, file, config
}

func TestManifestExactBytesHashReuseAndConflict(t *testing.T) {
	dir := t.TempDir()
	batch := models.RotationQuotaBatch{ID: 7, OwnerToken: testOwner}
	files := []models.RotationQuotaBatchFile{{RelativePath: "z", SnapshotKey: "z"}, {RelativePath: "a", SnapshotKey: "a"}}
	writer := ManifestWriter{}
	path, hash, data, err := writer.Write(dir, batch, files)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a\nz\n" {
		t.Fatalf("manifest bytes=%q", data)
	}
	sum := sha256.Sum256(data)
	if hash != hex.EncodeToString(sum[:]) {
		t.Fatal("manifest hash mismatch")
	}
	reused, reusedHash, reusedData, err := writer.Write(dir, models.RotationQuotaBatch{ID: 7, OwnerToken: testOwner, ManifestPath: path}, files)
	if err != nil || reused != path || reusedHash != hash || string(reusedData) != string(data) {
		t.Fatalf("manifest reuse path=%q hash=%q err=%v", reused, reusedHash, err)
	}
	if _, _, _, err := writer.Write(dir, models.RotationQuotaBatch{ID: 7, OwnerToken: testOwner, ManifestPath: path}, []models.RotationQuotaBatchFile{{RelativePath: "other"}}); err == nil {
		t.Fatal("manifest conflict was accepted")
	}
	failing := ManifestWriter{SyncDir: func(string) error { return fmt.Errorf("directory fsync failed") }}
	if _, _, _, err := failing.Write(dir, models.RotationQuotaBatch{ID: 8, OwnerToken: testOwner}, files); err == nil {
		t.Fatal("directory fsync failure was ignored")
	}
}

func TestManifestRejectsOwnerTokenPathEscape(t *testing.T) {
	_, _, _, err := (ManifestWriter{}).Write(t.TempDir(), models.RotationQuotaBatch{ID: 1, OwnerToken: "../escape"}, nil)
	if err == nil {
		t.Fatal("manifest accepted unsafe owner token")
	}
}

func TestExecutorCopySuccessAndSpec(t *testing.T) {
	db := executionDB(t)
	batch, file, config := executionFixture(t, db, 8)
	runner := &fakeRunner{process: &fakeProcess{result: ProcessResult{ExitCode: 0, PID: 42, ProcessStartToken: "42:1"}}, object: RemoteObject{Path: file.RelativePath, Size: file.SizeBytes}}
	executor := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: runner, Now: func() time.Time { return time.Unix(100, 0) }}
	if err := executor.RunBatch(context.Background(), batch.ID); err != nil {
		t.Fatal(err)
	}
	if runner.spec.ConfigPath != config || runner.spec.ManifestPath == "" || runner.spec.DestinationRemote != "remote" || runner.spec.SourceRoot == nil || runner.spec.Transfers != 16 {
		var failed models.RotationQuotaBatch
		_ = db.First(&failed, batch.ID)
		t.Fatalf("copy spec=%#v state=%s error=%s", runner.spec, failed.State, failed.LastError)
	}
	if !strings.Contains(runner.sourceLink, ".rclone-manager-stage") {
		t.Fatalf("runner did not receive manager stage: %q", runner.sourceLink)
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateSucceeded || stored.TransferMode != models.TransferModeCopy {
		t.Fatalf("batch=%#v", stored)
	}
	var storedFile models.RotationQuotaBatchFile
	if err := db.First(&storedFile, file.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedFile.State != models.BatchFileStateCommitted {
		t.Fatalf("file=%#v", storedFile)
	}
	var reservation models.QuotaReservation
	if err := db.First(&reservation, "batch_file_id = ?", file.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reservation.State != models.ReservationStateCommitted {
		t.Fatalf("reservation=%#v", reservation)
	}
}

func TestExecutorRefusesStartedReservedBatch(t *testing.T) {
	db := executionDB(t)
	batch, _, _ := executionFixture(t, db, 8)
	started := time.Unix(100, 0)
	if err := db.Model(&models.RotationQuotaBatch{}).Where("id = ?", batch.ID).Updates(map[string]interface{}{"started_at": started, "state": models.BatchStateReserved}).Error; err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{process: &fakeProcess{result: ProcessResult{PID: 101, ProcessStartToken: "101:1"}}}
	e := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: runner}
	if err := e.RunBatch(context.Background(), batch.ID); err == nil {
		t.Fatal("started batch was copied")
	}
	if runner.spec.SourceRoot != nil {
		t.Fatal("runner received started batch")
	}
}

func TestExecRunnerTokenFailureUsesWaitAndReconcilesMarker(t *testing.T) {
	db := executionDB(t)
	batch, file, config := executionFixture(t, db, 8)
	script := filepath.Join(t.TempDir(), "runner")
	body := "#!/bin/sh\nfor arg in \"$@\"; do if [ \"$arg\" = lsjson ]; then printf '%s' '{\"Path\":\"file.txt\",\"Size\":9,\"IsDir\":false}'; exit 0; fi; done\nprintf '%s' 'drive upload limit exceeded'\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	runner := ExecRunner{Binary: script, ProcessStartToken: func(int) string { time.Sleep(30 * time.Millisecond); return "" }}
	e := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: runner, Now: func() time.Time { return time.Unix(100, 0) }}
	if err := e.RunBatch(context.Background(), batch.ID); err != nil {
		t.Fatal(err)
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateSucceeded || stored.LimitMarker == "" || stored.MarkerDetectedAt == nil {
		t.Fatalf("batch=%#v", stored)
	}
	var account models.QuotaAccount
	if err := db.First(&account, batch.QuotaAccountID).Error; err != nil {
		t.Fatal(err)
	}
	if account.ProviderBlockedUntil == nil {
		t.Fatal("account was not frozen")
	}
	var committed models.RotationQuotaBatchFile
	if err := db.First(&committed, file.ID).Error; err != nil {
		t.Fatal(err)
	}
	if committed.State != models.BatchFileStateCommitted {
		t.Fatalf("file=%s", committed.State)
	}
	assertNoStageLeak(t, e.ManifestDir)
	_ = config
}

func assertNoStageLeak(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, ".rclone-manager-stage"))
	if err == nil && len(entries) != 0 {
		t.Fatalf("stage leak in %s: %v", dir, entries)
	}
}

func TestLeaseStagesAreOwnedAndCleanupCannotCrossDelete(t *testing.T) {
	base := t.TempDir()
	oldLease := testOwner
	newLease := "0123456789abcdef0123456789abcdef0123456789abcdef"[:47] + "e"
	oldStage, err := quota.PrepareStage(base, 7, testOwner, oldLease)
	if err != nil {
		t.Fatal(err)
	}
	newStage, err := quota.PrepareStage(base, 7, testOwner, newLease)
	if err != nil {
		t.Fatal(err)
	}
	oldPath, _ := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", oldStage.File().Fd()))
	newPath, _ := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", newStage.File().Fd()))
	if oldPath == newPath || !strings.Contains(oldPath, oldLease) || !strings.Contains(newPath, newLease) {
		t.Fatalf("stage paths old=%q new=%q", oldPath, newPath)
	}
	if err := oldStage.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("old cleanup removed successor: %v", err)
	}
	if err := newStage.Cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestSourceBookendRejectsMutationBeforeRunner(t *testing.T) {
	db := executionDB(t)
	batch, file, _ := executionFixture(t, db, 8)
	runner := &fakeRunner{process: &fakeProcess{result: ProcessResult{PID: 81, ProcessStartToken: "81:1"}}}
	executor := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: runner, BeforeStageClone: func() {
		time.Sleep(2 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(batch.SourceRoot, file.RelativePath), []byte("mutation!"), 0600)
	}, Now: func() time.Time { return time.Unix(100, 0) }}
	_ = executor.RunBatch(context.Background(), batch.ID)
	if runner.spec.SourceRoot != nil {
		t.Fatal("runner started after source bookend mismatch")
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateUnknown {
		t.Fatalf("state=%s", stored.State)
	}
	assertNoStageLeak(t, executor.ManifestDir)
}

func TestStageSnapshotSurvivesNestedAncestorSubstitutionAndRewrite(t *testing.T) {
	rootPath := t.TempDir()
	ancestor := filepath.Join(rootPath, "nested")
	if err := os.Mkdir(ancestor, 0700); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(ancestor, "file")
	if err := os.WriteFile(original, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshots, err := (quota.Scanner{}).Scan(rootPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	root, err := quota.OpenSourceRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	source, err := root.OpenValidated(quota.LocalSnapshot{RelativePath: snapshots[0].RelativePath, SizeBytes: snapshots[0].SizeBytes, MtimeNS: snapshots[0].MtimeNS, Device: snapshots[0].Device, Inode: snapshots[0].Inode, RootDevice: snapshots[0].RootDevice, RootInode: snapshots[0].RootInode})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := os.Rename(ancestor, ancestor+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ancestor, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ancestor, "file"), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	stage, err := quota.PrepareStage(t.TempDir(), 99, testOwner, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Cleanup()
	if err := stage.Snapshot(quota.LocalSnapshot{RelativePath: snapshots[0].RelativePath, SizeBytes: snapshots[0].SizeBytes, MtimeNS: snapshots[0].MtimeNS, Device: snapshots[0].Device, Inode: snapshots[0].Inode}, source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ancestor+"-old", "file"), []byte("rewriten"), 0600); err != nil {
		t.Fatal(err)
	}
	staged, err := os.Open(filepath.Join("/proc/self/fd", fmt.Sprint(stage.File().Fd()), snapshots[0].RelativePath))
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	var sourceStat, stagedStat syscall.Stat_t
	if err := syscall.Fstat(int(source.Fd()), &sourceStat); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Fstat(int(staged.Fd()), &stagedStat); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join("/proc/self/fd", fmt.Sprint(stage.File().Fd()), snapshots[0].RelativePath))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "original" {
		t.Fatalf("stage content changed: %q", content)
	}
	if sourceStat.Ino == stagedStat.Ino && sourceStat.Dev == stagedStat.Dev {
		t.Fatalf("stage still shares source inode: %d/%d", stagedStat.Dev, stagedStat.Ino)
	}
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if err := stage.Validate(quota.LocalSnapshot{RelativePath: snapshots[0].RelativePath, SizeBytes: snapshots[0].SizeBytes, Device: int64(stagedStat.Dev), Inode: int64(stagedStat.Ino)}); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) > len(before)+1 {
		t.Fatalf("stage validation leaked descriptors: before=%d after=%d", len(before), len(after))
	}
}

func TestPersistedBatchFilePathIsRejected(t *testing.T) {
	db := executionDB(t)
	batch, file, _ := executionFixture(t, db, 8)
	if err := db.Model(&models.RotationQuotaBatchFile{}).Where("id = ?", file.ID).Update("relative_path", "../escape").Error; err != nil {
		t.Fatal(err)
	}
	e := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: &fakeRunner{}}
	_ = e.RunBatch(context.Background(), batch.ID)
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateUnknown {
		t.Fatalf("unsafe persisted path state=%s", stored.State)
	}
}

func TestPlannedBatchWithHeldLeaseAndManifestRecovers(t *testing.T) {
	db := executionDB(t)
	batch, file, config := executionFixture(t, db, 8)
	manifest, hash, _, err := (ManifestWriter{}).Write(t.TempDir(), batch, []models.RotationQuotaBatchFile{file})
	if err != nil {
		t.Fatal(err)
	}
	oldLease := "restart-lease"
	past := time.Unix(1, 0)
	if err := db.Model(&models.RotationQuotaBatch{}).Where("id = ?", batch.ID).Updates(map[string]interface{}{"state": models.BatchStatePlanned, "lease_token": oldLease, "lease_until": past, "manifest_path": manifest, "manifest_hash": hash}).Error; err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{process: &fakeProcess{result: ProcessResult{PID: 44, ProcessStartToken: "44:1"}}, object: RemoteObject{Path: file.RelativePath, Size: file.SizeBytes}}
	if err := (&Executor{DB: db, ManifestDir: filepath.Dir(manifest), Runner: runner, Now: func() time.Time { return time.Unix(100, 0) }}).RunBatch(context.Background(), batch.ID); err != nil {
		t.Fatal(err)
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateSucceeded {
		t.Fatalf("state=%s", stored.State)
	}
	_ = config
}

func TestBlockedAccountRetainsPlannedBatch(t *testing.T) {
	db := executionDB(t)
	batch, _, _ := executionFixture(t, db, 8)
	blocked := time.Unix(1000, 0)
	if err := db.Model(&models.QuotaAccount{}).Where("id = ?", batch.QuotaAccountID).Update("provider_blocked_until", blocked).Error; err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{process: &fakeProcess{result: ProcessResult{PID: 45, ProcessStartToken: "45:1"}}}
	err := (&Executor{DB: db, ManifestDir: t.TempDir(), Runner: runner, Now: func() time.Time { return time.Unix(100, 0) }}).RunBatch(context.Background(), batch.ID)
	if err == nil {
		t.Fatal("blocked account started")
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStatePlanned || runner.spec.SourceRoot != nil {
		t.Fatalf("batch=%s runner=%#v", stored.State, runner.spec)
	}
}

func TestExhaustedRecoveryRetainsPlannedBatchAfterLegacyBlockExpires(t *testing.T) {
	db := executionDB(t)
	batch, _, _ := executionFixture(t, db, 8)
	past := time.Unix(90, 0)
	if err := db.Model(&models.QuotaAccount{}).Where("id = ?", batch.QuotaAccountID).Updates(map[string]interface{}{
		"provider_blocked_until": past,
		"recovery_state":         models.QuotaRecoveryStateExhausted,
		"recovery_generation":    1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{process: &fakeProcess{result: ProcessResult{PID: 46, ProcessStartToken: "46:1"}}}
	err := (&Executor{DB: db, ManifestDir: t.TempDir(), Runner: runner, Now: func() time.Time { return time.Unix(100, 0) }}).RunBatch(context.Background(), batch.ID)
	if !errors.Is(err, ErrAccountBlocked) {
		t.Fatalf("exhausted account start error = %v", err)
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStatePlanned || runner.spec.SourceRoot != nil {
		t.Fatalf("batch=%s runner=%#v", stored.State, runner.spec)
	}
}

func TestToReconcilingPersistsMarkerAndExhaustionTogether(t *testing.T) {
	db := executionDB(t)
	batch, _, _ := executionFixture(t, db, 8)
	lease := "atomic-marker-lease"
	if err := db.Model(&models.RotationQuotaBatch{}).Where("id = ?", batch.ID).Updates(map[string]interface{}{"state": models.BatchStateRunning, "lease_token": lease}).Error; err != nil {
		t.Fatal(err)
	}
	e := &Executor{DB: db, Now: func() time.Time { return time.Unix(100, 0) }}
	if err := e.toReconciling(batch.ID, lease, ProcessResult{ExitCode: 1, Stderr: "drive upload limit exceeded"}, nil); err != nil {
		t.Fatal(err)
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateReconciling || stored.LimitMarker == "" || stored.MarkerDetectedAt == nil {
		t.Fatalf("reconciling marker outcome = %#v", stored)
	}
	var account models.QuotaAccount
	if err := db.First(&account, batch.QuotaAccountID).Error; err != nil {
		t.Fatal(err)
	}
	if account.RecoveryState != models.QuotaRecoveryStateExhausted || account.RecoveryGeneration != 1 || account.NextProbeAt == nil || !account.NextProbeAt.Equal(time.Unix(100, 0).Add(models.DefaultQuotaRecoveryProbeDelay)) {
		t.Fatalf("atomic marker recovery outcome = %#v", account)
	}
}

func TestUnknownSiblingBlocksSameDestinationScope(t *testing.T) {
	db := executionDB(t)
	batch, _, config := executionFixture(t, db, 8)
	sibling := models.RotationQuotaBatch{TaskID: 2, QuotaAccountID: batch.QuotaAccountID, DestinationScope: batch.DestinationScope, SourceRoot: batch.SourceRoot, DestinationRemote: "other", TransferMode: models.TransferModeCopy, DestinationScopeVersion: 1, RcloneConfigPath: config, RequestKey: "sibling", RequestFingerprint: "sibling", DestinationPath: batch.DestinationPath, State: models.BatchStateUnknown, OwnerToken: "sibling-owner"}
	if err := db.Create(&sibling).Error; err != nil {
		t.Fatal(err)
	}
	e := &Executor{DB: db, Runner: &fakeRunner{}}
	if _, _, _, err := e.claim(batch.ID); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("unknown sibling did not block: %v", err)
	}
}

func TestExecutorVerifiedMarkerBatchBecomesTerminalAndFreezesAccount(t *testing.T) {
	db := executionDB(t)
	batch, file, _ := executionFixture(t, db, 8)
	runner := &fakeRunner{process: &fakeProcess{result: ProcessResult{ExitCode: 1, PID: 43, ProcessStartToken: "43:1", Stderr: "drive upload limit exceeded"}}, object: RemoteObject{Path: file.RelativePath, Size: file.SizeBytes}}
	executor := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: runner, Now: func() time.Time { return time.Unix(100, 0) }}
	if err := executor.RunBatch(context.Background(), batch.ID); err != nil {
		t.Fatal(err)
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateSucceeded {
		t.Fatalf("batch state=%q", stored.State)
	}
	var storedFile models.RotationQuotaBatchFile
	if err := db.First(&storedFile, file.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedFile.State != models.BatchFileStateCommitted {
		t.Fatalf("file state=%s", storedFile.State)
	}
	var account models.QuotaAccount
	if err := db.First(&account).Error; err != nil {
		t.Fatal(err)
	}
	if account.ProviderBlockedUntil == nil {
		t.Fatal("upload marker did not freeze account")
	}
	if stored.LimitMarker == "" || stored.MarkerDetectedAt == nil {
		t.Fatalf("marker metadata missing: %#v", stored)
	}
	assertNoStageLeak(t, executor.ManifestDir)
}

func TestDetectUploadLimit(t *testing.T) {
	if !DetectUploadLimit("fatal: drive upload limit exceeded").Detected {
		t.Fatal("marker not detected")
	}
	if DetectUploadLimit("ordinary copy completed").Detected {
		t.Fatal("false marker")
	}
}

func TestExecRunnerStatCanonicalizesNestedBasename(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rclone-stat")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s' '{\"Path\":\"file\",\"Size\":8,\"IsDir\":false}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runner := ExecRunner{Binary: binary}
	object, err := runner.StatRemote(context.Background(), "/tmp/config", "remote", "/dest", "dir/file")
	if err != nil {
		t.Fatal(err)
	}
	if object.Path != "dir/file" || object.IsDir || object.Size != 8 {
		t.Fatalf("object=%#v", object)
	}
}

func TestMissingProcessIdentityBecomesUnknown(t *testing.T) {
	db := executionDB(t)
	batch, _, _ := executionFixture(t, db, 8)
	runner := &fakeRunner{process: &fakeProcess{result: ProcessResult{PID: 0, ProcessStartToken: ""}}}
	executor := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: runner, Now: func() time.Time { return time.Unix(100, 0) }}
	_ = executor.RunBatch(context.Background(), batch.ID)
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateUnknown {
		t.Fatalf("state=%s", stored.State)
	}
}

func TestMarkerDoesNotShortenLongerAccountBlock(t *testing.T) {
	db := executionDB(t)
	batch, _, _ := executionFixture(t, db, 8)
	lease := "marker-lease"
	until := time.Unix(100+48*60*60, 0)
	if err := db.Model(&models.RotationQuotaBatch{}).Where("id = ?", batch.ID).Updates(map[string]interface{}{"state": models.BatchStateReconciling, "lease_token": lease}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.QuotaAccount{}).Where("id = ?", batch.QuotaAccountID).Update("provider_blocked_until", until).Error; err != nil {
		t.Fatal(err)
	}
	e := &Executor{DB: db, Now: func() time.Time { return time.Unix(100, 0) }}
	if err := e.freezeOnMarker(batch.ID, lease, ProcessResult{Stderr: "upload limit exceeded"}); err != nil {
		t.Fatal(err)
	}
	var account models.QuotaAccount
	if err := db.First(&account, batch.QuotaAccountID).Error; err != nil {
		t.Fatal(err)
	}
	if !account.ProviderBlockedUntil.Equal(until) {
		t.Fatalf("block shortened to %v", account.ProviderBlockedUntil)
	}
	if account.RecoveryState != models.QuotaRecoveryStateExhausted || account.RecoveryGeneration != 1 || account.FirstExhaustedAt == nil || !account.FirstExhaustedAt.Equal(time.Unix(100, 0)) {
		t.Fatalf("recovery transition = state %q generation %d first %v", account.RecoveryState, account.RecoveryGeneration, account.FirstExhaustedAt)
	}
	if account.NextProbeAt == nil {
		t.Fatal("first probe was not scheduled")
	}
	firstProbe := *account.NextProbeAt
	if !firstProbe.Equal(time.Unix(100, 0).Add(models.DefaultQuotaRecoveryProbeDelay)) {
		t.Fatalf("first probe = %v", account.NextProbeAt)
	}
	if err := e.freezeOnMarker(batch.ID, lease, ProcessResult{Stderr: "upload limit exceeded again"}); err != nil {
		t.Fatal(err)
	}
	var repeated models.QuotaAccount
	if err := db.First(&repeated, batch.QuotaAccountID).Error; err != nil {
		t.Fatal(err)
	}
	if repeated.RecoveryGeneration != 1 || repeated.FirstExhaustedAt == nil || !repeated.FirstExhaustedAt.Equal(*account.FirstExhaustedAt) || repeated.NextProbeAt == nil || !repeated.NextProbeAt.Equal(firstProbe) {
		t.Fatalf("repeated marker changed recovery timing = %#v", repeated)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- e.freezeOnMarker(batch.ID, lease, ProcessResult{Stderr: "upload limit exceeded concurrently"})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var concurrent models.QuotaAccount
	if err := db.First(&concurrent, batch.QuotaAccountID).Error; err != nil {
		t.Fatal(err)
	}
	if concurrent.RecoveryGeneration != 1 || concurrent.NextProbeAt == nil || !concurrent.NextProbeAt.Equal(firstProbe) {
		t.Fatalf("concurrent markers changed recovery timing = %#v", concurrent)
	}
}

func TestExecutorStartFailureReleasesOnlyThisBatch(t *testing.T) {
	db := executionDB(t)
	batch, file, _ := executionFixture(t, db, 8)
	runner := &fakeRunner{startErr: fmt.Errorf("start failed")}
	executor := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: runner, Now: func() time.Time { return time.Unix(100, 0) }}
	if err := executor.RunBatch(context.Background(), batch.ID); err == nil {
		t.Fatal("start failure was swallowed")
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateFailed {
		t.Fatalf("batch state=%q", stored.State)
	}
	var reservation models.QuotaReservation
	if err := db.First(&reservation, "batch_file_id = ?", file.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reservation.State != models.ReservationStateReleased {
		t.Fatalf("reservation state=%q", reservation.State)
	}
}

func TestStartedIdentityErrorKeepsQuotaUnknownForReconcile(t *testing.T) {
	db := executionDB(t)
	batch, file, _ := executionFixture(t, db, 8)
	runner := &fakeRunner{startErr: &StartedProcessIdentityError{PID: 77, Cause: errors.New("token unavailable"), Result: ProcessResult{PID: 77, ExitCode: 1, Stderr: "drive upload limit exceeded"}}, object: RemoteObject{Path: "wrong", Size: file.SizeBytes}}
	e := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: runner, Now: func() time.Time { return time.Unix(100, 0) }}
	if err := e.RunBatch(context.Background(), batch.ID); err != nil {
		t.Fatal(err)
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateUnknown {
		t.Fatalf("state=%s", stored.State)
	}
	var reservation models.QuotaReservation
	if err := db.First(&reservation, "batch_file_id = ?", file.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reservation.State != models.ReservationStateUnknown {
		t.Fatalf("reservation released after started error: %s", reservation.State)
	}
	if stored.LimitMarker == "" {
		t.Fatal("started identity marker was not processed")
	}
	assertNoStageLeak(t, e.ManifestDir)
}

func TestPersistProcessFailureStillRunsMarkerReconcile(t *testing.T) {
	db := executionDB(t)
	batch, file, _ := executionFixture(t, db, 8)
	lease := "persist-failure-lease"
	if err := db.Model(&models.RotationQuotaBatch{}).Where("id = ?", batch.ID).Update("state", models.BatchStateRunning).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.RotationQuotaBatch{}).Where("id = ?", batch.ID).Update("lease_token", lease).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.RotationQuotaBatchFile{}).Where("id = ?", file.ID).Update("state", models.BatchFileStateActive).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.QuotaReservation{}).Where("batch_file_id = ?", file.ID).Update("state", models.ReservationStateActive).Error; err != nil {
		t.Fatal(err)
	}
	stage, err := quota.PrepareStage(t.TempDir(), batch.ID, testOwner, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{DB: db, Runner: &fakeRunner{object: RemoteObject{Path: file.RelativePath, Size: file.SizeBytes}}, Now: func() time.Time { return time.Unix(100, 0) }}
	batch.LeaseToken = lease
	processErr := errors.New("persist process row failed")
	if err := e.finishProcess(context.Background(), batch, []models.RotationQuotaBatchFile{file}, lease, stage, ProcessResult{PID: 90, Stderr: "drive upload limit exceeded"}, nil, processErr); !errors.Is(err, processErr) {
		t.Fatalf("finish error=%v", err)
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateSucceeded || stored.LimitMarker == "" {
		t.Fatalf("marker result=%#v", stored)
	}
	var committed models.RotationQuotaBatchFile
	if err := db.First(&committed, file.ID).Error; err != nil {
		t.Fatal(err)
	}
	if committed.State != models.BatchFileStateCommitted {
		t.Fatalf("file=%s", committed.State)
	}
	assertNoStageLeak(t, e.ManifestDir)
}

func TestPersistFailureProcessDoneStillFreezesMarker(t *testing.T) {
	db := executionDB(t)
	batch, file, _ := executionFixture(t, db, 8)
	runner := &fakeRunner{process: &fakeProcess{result: ProcessResult{PID: 92, Stderr: "drive upload limit exceeded"}, stopErr: os.ErrProcessDone}, object: RemoteObject{Path: file.RelativePath, Size: file.SizeBytes}}
	e := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: runner, PersistProcessFunc: func(uint, string, ProcessHandle) error { return errors.New("persist failed") }, Now: func() time.Time { return time.Unix(100, 0) }}
	if err := e.RunBatch(context.Background(), batch.ID); err == nil {
		t.Fatal("persist failure was swallowed")
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateSucceeded || stored.LimitMarker == "" {
		t.Fatalf("batch=%#v", stored)
	}
	var account models.QuotaAccount
	if err := db.First(&account, batch.QuotaAccountID).Error; err != nil {
		t.Fatal(err)
	}
	if account.ProviderBlockedUntil == nil {
		t.Fatal("account was not frozen")
	}
	assertNoStageLeak(t, e.ManifestDir)
}

func TestLeaseHeartbeatRenewsDuringStagePreparation(t *testing.T) {
	db := executionDB(t)
	batch, file, _ := executionFixture(t, db, 8)
	clock := time.Unix(100, 0)
	e := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: &fakeRunner{process: &fakeProcess{result: ProcessResult{PID: 91, ProcessStartToken: "91:1"}}, object: RemoteObject{Path: file.RelativePath, Size: file.SizeBytes}}, LeaseDuration: 10 * time.Minute, LeaseRenewInterval: time.Millisecond, BeforeStageClone: func() { clock = clock.Add(time.Hour); time.Sleep(20 * time.Millisecond) }, Now: func() time.Time { return clock }}
	if err := e.RunBatch(context.Background(), batch.ID); err != nil {
		t.Fatal(err)
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateSucceeded {
		t.Fatalf("state=%s", stored.State)
	}
	if stored.LeaseUntil == nil || !stored.LeaseUntil.After(time.Unix(100+3600, 0)) {
		t.Fatalf("lease was not renewed: %v", stored.LeaseUntil)
	}
}

func TestManualIdentityPersistenceFailureAfterWaitErrorIsKnownFailure(t *testing.T) {
	db := executionDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}, &models.Task{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.Task{ID: 1, Enabled: true, TaskType: "rotation", RotationStrategy: "proactive_quota", RcloneConfig: "/config", RemoteDir: "/dest"}).Error; err != nil {
		t.Fatal(err)
	}
	leaseUntil := time.Unix(200, 0)
	epoch := models.DestinationScopeMaintenance{DestinationScope: models.DestinationScope("/config", "/dest"), Epoch: 1, OwnerTaskID: 1, FirstRemote: "remote", RemoteDir: "/dest", ResolvedConfigPath: "/config", ResolvedConfigIdentity: "/config", State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateClaimed, Reason: models.MaintenanceReasonManualMerge, LeaseToken: "manual-owner", LeaseUntil: &leaseUntil, Revision: 1}
	if err := db.Create(&epoch).Error; err != nil {
		t.Fatal(err)
	}
	process := &fakeProcess{result: ProcessResult{PID: 77, ProcessStartToken: "77:1"}, waitErr: errors.New("wait reported reaped process")}
	e := &Executor{DB: db, Runner: &fakeDedupeRunner{process: process}, Now: func() time.Time { return time.Unix(100, 0) }, PersistDedupeIdentityFunc: func(models.DestinationScopeMaintenance, ProcessHandle) error {
		return errors.New("identity write failed")
	}}
	if err := e.RunDedupe(context.Background(), epoch); err == nil {
		t.Fatal("identity failure was swallowed")
	}
	if !process.stopped {
		t.Fatal("process was not stopped")
	}
	var stored models.DestinationScopeMaintenance
	if err := db.First(&stored, epoch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.MaintenanceStateClosed || stored.DedupeState != models.DedupeStateFailed {
		t.Fatalf("reaped process entered unknown/fenced state: %#v", stored)
	}
}

func TestManualDedupeSuccessDoesNotPersistRcloneOutputAsError(t *testing.T) {
	db := executionDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}, &models.Task{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.Task{ID: 1, Enabled: true, TaskType: "rotation", RotationStrategy: "proactive_quota", RcloneConfig: "/config", RemoteDir: "/dest"}).Error; err != nil {
		t.Fatal(err)
	}
	leaseUntil := time.Unix(200, 0)
	epoch := models.DestinationScopeMaintenance{DestinationScope: models.DestinationScope("/config", "/dest"), Epoch: 1, OwnerTaskID: 1, FirstRemote: "remote", RemoteDir: "/dest", ResolvedConfigPath: "/config", ResolvedConfigIdentity: "/config", State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateClaimed, Reason: models.MaintenanceReasonManualMerge, LeaseToken: "manual-owner", LeaseUntil: &leaseUntil, Revision: 1}
	if err := db.Create(&epoch).Error; err != nil {
		t.Fatal(err)
	}
	process := &fakeProcess{result: ProcessResult{ExitCode: 0, Stderr: "INFO : merged duplicate directory", PID: 78, ProcessStartToken: "78:1"}}
	e := &Executor{DB: db, Runner: &fakeDedupeRunner{process: process}, Now: func() time.Time { return time.Unix(100, 0) }}
	if err := e.RunDedupe(context.Background(), epoch); err != nil {
		t.Fatal(err)
	}
	var storedEpoch models.DestinationScopeMaintenance
	if err := db.First(&storedEpoch, epoch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedEpoch.State != models.MaintenanceStateClosed || storedEpoch.DedupeState != models.DedupeStateSucceeded || storedEpoch.LastError != "" {
		t.Fatalf("successful manual dedupe retained output as error: %#v", storedEpoch)
	}
	var storedTask models.Task
	if err := db.First(&storedTask, 1).Error; err != nil {
		t.Fatal(err)
	}
	if storedTask.LastError != "" {
		t.Fatalf("successful manual dedupe retained task error: %#v", storedTask)
	}
}

func TestExecutorRejectsLegacyQuotaDedupeExecution(t *testing.T) {
	db := executionDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}); err != nil {
		t.Fatal(err)
	}
	leaseUntil := time.Unix(200, 0)
	epoch := models.DestinationScopeMaintenance{DestinationScope: models.DestinationScope("/config", "/dest"), Epoch: 1, State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateClaimed, Reason: models.MaintenanceReasonQuotaExhaustion, LeaseToken: "legacy", LeaseUntil: &leaseUntil}
	if err := db.Create(&epoch).Error; err != nil {
		t.Fatal(err)
	}
	e := &Executor{DB: db, Runner: &fakeDedupeRunner{process: &fakeProcess{}}, Now: func() time.Time { return time.Unix(100, 0) }}
	if err := e.RunDedupe(context.Background(), epoch); !errors.Is(err, ErrManualMergeConflict) {
		t.Fatalf("legacy quota dedupe error=%v", err)
	}
	var stored models.DestinationScopeMaintenance
	if err := db.First(&stored, epoch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.DedupeState != models.DedupeStateClaimed {
		t.Fatalf("legacy quota epoch was mutated: %#v", stored)
	}
}

func TestLeaseHeartbeatFailureNeverStartsRunner(t *testing.T) {
	db := executionDB(t)
	batch, _, _ := executionFixture(t, db, 8)
	runner := &fakeRunner{process: &fakeProcess{result: ProcessResult{PID: 93, ProcessStartToken: "93:1"}}}
	e := &Executor{DB: db, ManifestDir: t.TempDir(), Runner: runner, LeaseDuration: time.Minute, LeaseRenewInterval: time.Millisecond, BeforeStageClone: func() {
		_ = db.Model(&models.RotationQuotaBatch{}).Where("id = ?", batch.ID).Update("lease_token", "different-owner").Error
		time.Sleep(20 * time.Millisecond)
	}, Now: func() time.Time { return time.Unix(100, 0) }}
	_ = e.RunBatch(context.Background(), batch.ID)
	if runner.spec.SourceRoot != nil {
		t.Fatal("runner started after lease renewal failure")
	}
}
