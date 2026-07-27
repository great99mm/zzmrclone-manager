package manualtransfer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/quota"
)

func manualTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "manual.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.AutoMigrate(&models.Task{}, &models.QuotaAccount{}, &models.QuotaReservation{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &ManualTaskAccount{}, &ManualTransferRun{}, &ManualRunAccount{}, &ManualRunFile{}, &ManualRunAllocation{}, &ManualRunEvent{}); err != nil {
		t.Fatal(err)
	}
	return database
}

func manualTask(t *testing.T, database *gorm.DB, source string) models.Task {
	t.Helper()
	task := models.Task{Name: "manual", SourceType: "local", SourceDir: source, DestType: "remote", RemoteName: "remote", RemoteDir: "/dest", TransferMode: models.TransferModeCopy, TaskType: models.TaskTypeManual, ManualStrategy: models.ManualStrategyAllocation, WatchEnabled: false, ScheduleEnabled: false, QBEnabled: false, Enabled: true}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	for id, suffix := range map[uint]string{1: "a", 2: "b"} {
		if err := database.Create(&models.QuotaAccount{ID: id, QuotaKey: "account-" + suffix, RemoteName: "remote-" + suffix, ConfigIdentity: filepath.Join(source, "config-"+suffix), Enabled: true}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return task
}

func manualRequest(taskID uint, key string) AnalyzeRequest {
	return AnalyzeRequest{TaskID: taskID, SourcePath: "", DestinationPath: "/dest", TransferMode: models.TransferModeCopy, ConfigIdentity: "config-identity", IdempotencyKey: key, Accounts: []AccountInput{{AccountID: 1, AccountIdentity: "account-a"}, {AccountID: 2, AccountIdentity: "account-b"}}}
}

func waitManualRun(t *testing.T, service *Service, runID uint, want string) ManualTransferRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := service.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State == want {
			return run
		}
		time.Sleep(5 * time.Millisecond)
	}
	run, _ := service.GetRun(runID)
	t.Fatalf("run did not reach %q: %#v", want, run)
	return run
}

func TestAnalyzeDeterministicDigestAndPagination(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	for name, value := range map[string]string{"z.txt": "z", "a.txt": "a", "nested/m.txt": "m"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	task := manualTask(t, database, root)
	service := NewService(database)
	service.Start()
	defer service.Stop()
	request := manualRequest(task.ID, "digest-1")
	request.SourcePath = root
	first, err := service.CreateAnalyze(request)
	if err != nil {
		t.Fatal(err)
	}
	run := waitManualRun(t, service, first.Run.ID, ManualRunStateAnalyzed)
	if run.SnapshotCount != 3 || run.SnapshotBytes != 3 || run.SnapshotDigest == "" {
		t.Fatalf("snapshot aggregate = count %d bytes %d digest %q", run.SnapshotCount, run.SnapshotBytes, run.SnapshotDigest)
	}

	var paths []string
	cursor := ""
	for {
		page, err := service.ListFiles(run.ID, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range page.Files {
			paths = append(paths, file.RelativePath)
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	want := []string{"a.txt", "nested/m.txt", "z.txt"}
	if len(paths) != len(want) {
		t.Fatalf("paginated paths = %#v, want %#v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paginated paths = %#v, want %#v", paths, want)
		}
	}

	secondRequest := request
	secondRequest.IdempotencyKey = "digest-2"
	secondRequest.ExpectedRunID = &run.ID
	secondRequest.ExpectedRevision = &run.Revision
	second, err := service.CreateAnalyze(secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	secondRun := waitManualRun(t, service, second.Run.ID, ManualRunStateAnalyzed)
	if secondRun.SnapshotDigest != run.SnapshotDigest {
		t.Fatalf("digest changed between identical snapshots: %q != %q", secondRun.SnapshotDigest, run.SnapshotDigest)
	}
}

func TestAnalyzeNoUploadOrReservationSideEffects(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	task := manualTask(t, database, root)
	service := NewService(database)
	service.Start()
	defer service.Stop()
	request := manualRequest(task.ID, "no-upload")
	request.SourcePath = root
	result, err := service.CreateAnalyze(request)
	if err != nil {
		t.Fatal(err)
	}
	waitManualRun(t, service, result.Run.ID, ManualRunStateAnalyzed)
	var reservations int64
	if err := database.Model(&models.QuotaReservation{}).Count(&reservations).Error; err != nil {
		t.Fatal(err)
	}
	var batches int64
	if err := database.Model(&models.RotationQuotaBatch{}).Count(&batches).Error; err != nil {
		t.Fatal(err)
	}
	if reservations != 0 || batches != 0 {
		t.Fatalf("analyze created transfer side effects: reservations=%d batches=%d", reservations, batches)
	}
}

func TestAnalyzeRejectsInvalidSourceAndRootSwap(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	task := manualTask(t, database, root)
	service := NewService(database)
	bad := manualRequest(task.ID, "invalid")
	bad.SourcePath = filepath.Join(root, "file")
	if _, err := service.CreateAnalyze(bad); err == nil {
		t.Fatal("file source path was accepted")
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside"), []byte("outside"), 0644); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	service.Scanner.BeforeFinalValidation = func() {
		once.Do(func() {
			moved := filepath.Join(filepath.Dir(root), "moved-root")
			if err := os.Rename(root, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, root); err != nil {
				t.Fatal(err)
			}
		})
	}
	service.Start()
	defer service.Stop()
	request := manualRequest(task.ID, "root-swap")
	request.SourcePath = root
	result, err := service.CreateAnalyze(request)
	if err != nil {
		t.Fatal(err)
	}
	waitManualRun(t, service, result.Run.ID, ManualRunStateAnalysisFailed)
}

func TestAnalyzeIdempotencyAndRevisionConflict(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	task := manualTask(t, database, root)
	service := NewService(database)
	service.Start()
	defer service.Stop()
	request := manualRequest(task.ID, "same-key")
	request.SourcePath = root
	first, err := service.CreateAnalyze(request)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := service.CreateAnalyze(request)
	if err != nil || !duplicate.Existing || duplicate.Run.ID != first.Run.ID {
		t.Fatalf("duplicate = %#v, err=%v", duplicate, err)
	}
	changed := request
	changed.DestinationPath = "/different"
	if _, err := service.CreateAnalyze(changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed idempotency request error = %v", err)
	}
	stale := request
	stale.IdempotencyKey = "new-key"
	expected := int64(99)
	stale.ExpectedRevision = &expected
	if _, err := service.CreateAnalyze(stale); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
}

func TestAnalyzeLargeCollectionUsesBatchedRowsAndRestartTerminalizes(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	for i := 0; i < 700; i++ {
		name := filepath.Join(root, "files", filepath.Base(filepath.Join("x", formatTestNumber(i))))
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	task := manualTask(t, database, root)
	service := NewService(database)
	service.Start()
	request := manualRequest(task.ID, "large")
	request.SourcePath = root
	result, err := service.CreateAnalyze(request)
	if err != nil {
		t.Fatal(err)
	}
	run := waitManualRun(t, service, result.Run.ID, ManualRunStateAnalyzed)
	if run.SnapshotCount != 700 {
		t.Fatalf("large collection count = %d", run.SnapshotCount)
	}
	service.Stop()

	restartService := NewService(database)
	rootHandle, err := quota.OpenSourceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	pendingRun := ManualTransferRun{TaskID: task.ID, State: ManualRunStateAnalyzing, Revision: 1, ActorIdentity: "operator", ActorType: "admin_session", SourcePath: root, DestinationPath: "/dest", TransferMode: models.TransferModeCopy, ConfigIdentity: "config", FrozenInput: "{}", IdempotencyKey: "restart", RequestFingerprint: "restart", SourceRootDevice: rootHandle.Device, SourceRootInode: rootHandle.Inode}
	_ = rootHandle.Close()
	if err := database.Create(&pendingRun).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&ManualRunEvent{RunID: pendingRun.ID, EventType: ManualRunEventRequested, ToState: ManualRunStateAnalyzing, ActorIdentity: "operator", ActorType: "admin_session"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := restartService.RecoverAnalyzing(); err != nil {
		t.Fatal(err)
	}
	failed := waitManualRun(t, restartService, pendingRun.ID, ManualRunStateAnalysisFailed)
	if !strings.Contains(failed.LastError, "restart") {
		t.Fatalf("restart failure message = %q", failed.LastError)
	}
	var restartEvent ManualRunEvent
	if err := database.Where("run_id = ? AND event_type = ?", pendingRun.ID, ManualRunEventStartupTerminalized).First(&restartEvent).Error; err != nil {
		t.Fatal(err)
	}
	if restartEvent.ActorIdentity != "system" || restartEvent.ActorType != "system" {
		t.Fatalf("restart event actor = %q/%q", restartEvent.ActorIdentity, restartEvent.ActorType)
	}
}

func formatTestNumber(value int) string {
	return filepath.Base(filepath.Join("000", strconvItoa(value))) + ".bin"
}

func strconvItoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}
