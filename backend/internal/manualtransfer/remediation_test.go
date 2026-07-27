package manualtransfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/quota"
)

type recordingFence struct {
	Calls int
	Err   error
}

func (f *recordingFence) WithTaskExclusive(_ context.Context, _ uint, fn func(*models.Task) error) error {
	f.Calls++
	if f.Err != nil {
		return f.Err
	}
	return fn(nil)
}

func TestTaskScopedIdempotencyAndRunReferenceRevision(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	task := manualTask(t, database, root)
	secondTask := models.Task{Name: "second", SourceType: "local", SourceDir: root, DestType: "remote", RemoteName: "remote-2", RemoteDir: "/dest-2", TransferMode: models.TransferModeCopy, TaskType: models.TaskTypeManual, ManualStrategy: models.ManualStrategyAllocation, WatchEnabled: false, ScheduleEnabled: false, QBEnabled: false, Enabled: true}
	if err := database.Create(&secondTask).Error; err != nil {
		t.Fatal(err)
	}
	for id, suffix := range map[uint]string{3: "c", 4: "d"} {
		if err := database.Create(&models.QuotaAccount{ID: id, QuotaKey: "account-" + suffix, RemoteName: "remote-" + suffix, ConfigIdentity: filepath.Join(root, "config-"+suffix), Enabled: true}).Error; err != nil {
			t.Fatal(err)
		}
	}
	service := NewService(database)
	service.Start()
	defer service.Stop()
	firstRequest := manualRequest(task.ID, "same-key")
	firstRequest.SourcePath = root
	first, err := service.CreateAnalyze(firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	firstRun := waitManualRun(t, service, first.Run.ID, ManualRunStateAnalyzed)
	secondRequest := manualRequest(secondTask.ID, "same-key")
	secondRequest.SourcePath = root
	secondRequest.DestinationPath = secondTask.RemoteDir
	secondRequest.Accounts = []AccountInput{{AccountID: 3}, {AccountID: 4}}
	second, err := service.CreateAnalyze(secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	if second.Run.ID == first.Run.ID {
		t.Fatal("same idempotency key was global instead of task-scoped")
	}
	reanalysis := firstRequest
	reanalysis.IdempotencyKey = "explicit-reanalysis"
	reanalysis.ExpectedRunID = &firstRun.ID
	reanalysis.ExpectedRevision = &firstRun.Revision
	reanalyzed, err := service.CreateAnalyze(reanalysis)
	if err != nil {
		t.Fatalf("explicit reanalysis was rejected: %v", err)
	}
	reanalyzedRun := waitManualRun(t, service, reanalyzed.Run.ID, ManualRunStateAnalyzed)
	replay, err := service.CreateAnalyze(reanalysis)
	if err != nil || !replay.Existing || replay.Run.ID != reanalyzedRun.ID {
		t.Fatalf("replacement replay = %#v, err=%v", replay, err)
	}
	staleRevision := firstRun.Revision
	stale := reanalysis
	stale.IdempotencyKey = "stale-key"
	stale.ExpectedRunID = &firstRun.ID
	stale.ExpectedRevision = &staleRevision
	if _, err := service.CreateAnalyze(stale); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale predecessor accepted for a new key: %v", err)
	}
	wrongRun := first.Run.ID + 1000
	correctRevision := firstRun.Revision
	duplicate := reanalysis
	duplicate.IdempotencyKey = "aba-key"
	duplicate.ExpectedRunID = &wrongRun
	duplicate.ExpectedRevision = &correctRevision
	if _, err := service.CreateAnalyze(duplicate); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("ABA predecessor was accepted: %v", err)
	}
	changedPredecessor := firstRequest
	changedPredecessor.ExpectedRunID = &firstRun.ID
	changedPredecessor.ExpectedRevision = &correctRevision
	if _, err := service.CreateAnalyze(changedPredecessor); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("existing key accepted a changed predecessor: %v", err)
	}
}

func TestAnalyzingAndFailedSnapshotsAreInvisible(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	task := manualTask(t, database, root)
	analyzing := seedManualRun(t, database, task.ID, root, ManualRunStateAnalyzing)
	if err := database.Create(&ManualRunFile{RunID: analyzing.ID, Generation: 1, RelativePath: "hidden", SnapshotKey: "hidden", SizeBytes: 1, Device: 1, Inode: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(database).ListFiles(analyzing.ID, "", 10); !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("analyzing snapshot was exposed: %v", err)
	}
	failed := seedManualRun(t, database, task.ID, root, ManualRunStateAnalysisFailed)
	if _, err := NewService(database).ListFiles(failed.ID, "", 10); !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("failed snapshot was exposed: %v", err)
	}
	activatedAt := time.Now()
	analyzed := seedManualRun(t, database, task.ID, root, ManualRunStateAnalyzed)
	analyzed.SnapshotGeneration = 1
	analyzed.Revision = 2
	if err := database.Model(&analyzed).Updates(map[string]interface{}{"snapshot_generation": 1, "revision": 2, "analyzed_at": activatedAt}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&ManualRunFile{RunID: analyzed.ID, Generation: 1, RelativePath: "visible", SnapshotKey: "visible", SizeBytes: 1, Device: 1, Inode: 1, ActivatedAt: &activatedAt}).Error; err != nil {
		t.Fatal(err)
	}
	page, err := NewService(database).ListFiles(analyzed.ID, "", 10)
	if err != nil || len(page.Files) != 1 || page.Files[0].RelativePath != "visible" {
		t.Fatalf("activated snapshot listing = %#v, %v", page, err)
	}
}

func TestRunAccountResponsesAreBoundedAndPropagateDBErrors(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	task := manualTask(t, database, root)
	run := seedManualRun(t, database, task.ID, root, ManualRunStateAnalyzed)
	for position := 0; position < 101; position++ {
		if err := database.Create(&ManualRunAccount{RunID: run.ID, Position: position, AccountID: uint(position + 1), AccountIdentity: "account", RemoteName: "remote", ConfigIdentity: "config"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	accounts, truncated, err := NewService(database).GetRunAccountsBounded(run.ID, ManualAccountPageLimit)
	if err != nil || len(accounts) != ManualAccountPageLimit || !truncated {
		t.Fatalf("bounded accounts = %d truncated=%t err=%v", len(accounts), truncated, err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewService(database).GetRunAccountsBounded(run.ID, ManualAccountPageLimit); err == nil {
		t.Fatal("database read error was converted into a successful empty response")
	}
}

func TestArbitraryAccountCountUsesTechnicalBoundAndCursorPages(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	task := manualTask(t, database, root)
	accounts := make([]AccountInput, 0, 101)
	for id := uint(3); id <= 103; id++ {
		if err := database.Create(&models.QuotaAccount{ID: id, QuotaKey: "account-" + strconv.FormatUint(uint64(id), 10), RemoteName: "remote-" + strconv.FormatUint(uint64(id), 10), ConfigIdentity: filepath.Join(root, "config-"+strconv.FormatUint(uint64(id), 10)), Enabled: true}).Error; err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, AccountInput{AccountID: id})
	}
	service := NewService(database)
	service.Start()
	request := manualRequest(task.ID, "many-accounts")
	request.SourcePath = root
	request.Accounts = accounts
	result, err := service.CreateAnalyze(request)
	if err != nil {
		t.Fatal(err)
	}
	waitManualRun(t, service, result.Run.ID, ManualRunStateAnalyzed)
	all, err := service.GetRunAccounts(result.Run.ID)
	if err != nil || len(all) != 101 {
		t.Fatalf("internal account load = %d, err=%v", len(all), err)
	}
	seen := 0
	cursor := ""
	for {
		page, pageErr := service.ListRunAccounts(result.Run.ID, cursor, 37)
		if pageErr != nil {
			t.Fatal(pageErr)
		}
		seen += len(page.Accounts)
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if seen != 101 {
		t.Fatalf("cursor account pages returned %d accounts", seen)
	}
	tooMany := request
	tooMany.IdempotencyKey = "too-many"
	tooMany.Accounts = make([]AccountInput, ManualMaxAccountInputs+1)
	if _, err := service.CreateAnalyze(tooMany); err == nil {
		t.Fatal("technical account input bound was not enforced")
	}
	service.Stop()
}

func TestAnalysisFailsClosedForAddDeleteAndChange(t *testing.T) {
	for _, name := range []string{"addition", "deletion", "change"} {
		t.Run(name, func(t *testing.T) {
			database := manualTestDB(t)
			root := t.TempDir()
			file := filepath.Join(root, "file")
			if err := os.WriteFile(file, []byte("data"), 0644); err != nil {
				t.Fatal(err)
			}
			task := manualTask(t, database, root)
			pass := 0
			scanner := quota.Scanner{
				BeforeFinalValidation: func() {
					pass++
					if name == "addition" && pass == 1 {
						if err := os.WriteFile(filepath.Join(root, "added"), []byte("new"), 0644); err != nil {
							t.Fatal(err)
						}
					}
				},
				LookupHook: func(_ string, observation int) {
					if pass != 1 || observation != 1 {
						return
					}
					if name == "deletion" {
						if err := os.Remove(file); err != nil {
							t.Fatal(err)
						}
					}
					if name == "change" {
						if err := os.Remove(file); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(file, []byte("changed"), 0644); err != nil {
							t.Fatal(err)
						}
					}
				},
			}
			service := NewService(database)
			service.Scanner = scanner
			service.Start()
			request := manualRequest(task.ID, name)
			request.SourcePath = root
			result, err := service.CreateAnalyze(request)
			if err != nil {
				t.Fatal(err)
			}
			failed := waitManualRun(t, service, result.Run.ID, ManualRunStateAnalysisFailed)
			if failed.SnapshotCount != 0 {
				t.Fatalf("failed run retained aggregate: %#v", failed)
			}
			service.Stop()
		})
	}
}

func TestTaskFenceAndAtomicStartFailure(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	task := manualTask(t, database, root)
	fence := &recordingFence{Err: errors.New("task is active")}
	service := NewService(database)
	service.TaskFence = fence
	request := manualRequest(task.ID, "fenced")
	request.SourcePath = root
	if _, err := service.CreateAnalyze(request); err == nil || fence.Calls != 1 {
		t.Fatalf("task fence was not enforced: calls=%d err=%v", fence.Calls, err)
	}

	seed := seedManualRun(t, database, task.ID, root, ManualRunStateAnalyzing)
	service = NewService(database)
	service.BeforeStartEvent = func(*gorm.DB) error { return errors.New("audit insert failed") }
	service.Start()
	if err := service.Enqueue(seed.ID); err != nil {
		t.Fatal(err)
	}
	failed := waitManualRun(t, service, seed.ID, ManualRunStateAnalysisFailed)
	if failed.AnalysisStartedAt != nil {
		t.Fatal("start timestamp escaped failed start transaction")
	}
	var starts int64
	if err := database.Model(&ManualRunEvent{}).Where("run_id = ? AND event_type = ?", seed.ID, ManualRunEventAnalysisStarted).Count(&starts).Error; err != nil {
		t.Fatal(err)
	}
	if starts != 0 {
		t.Fatal("start audit event escaped failed start transaction")
	}
	service.Stop()
}

func TestShutdownCancellationIsBounded(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	task := manualTask(t, database, root)
	entered, release := make(chan struct{}), make(chan struct{})
	scanner := quota.Scanner{LookupHook: func(_ string, observation int) {
		if observation == 1 {
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-release
		}
	}}
	service := NewService(database)
	service.Scanner = scanner
	service.Start()
	request := manualRequest(task.ID, "cancel")
	request.SourcePath = root
	if _, err := service.CreateAnalyze(request); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("analysis did not enter scanner")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := service.StopContext(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown was not bounded: %v", err)
	}
	close(release)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := service.StopContext(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAccountsAreTrustedDeduplicatedAndCopyMoveOnly(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	task := manualTask(t, database, root)
	for _, account := range map[uint]models.QuotaAccount{
		3: {ID: 3, QuotaKey: "account-c", RemoteName: "remote-c", ConfigIdentity: filepath.Join(root, "config-a"), Enabled: true},
		4: {ID: 4, QuotaKey: "account-d", RemoteName: "remote-a", ConfigIdentity: filepath.Join(root, "config-c"), Enabled: true},
		5: {ID: 5, QuotaKey: "account-e", RemoteName: "remote-a", ConfigIdentity: filepath.Join(root, "config-a"), Enabled: true},
		6: {ID: 6, QuotaKey: "account-f", RemoteName: "remote-f", ConfigIdentity: filepath.Join(root, "config-f"), Enabled: false},
	} {
		if err := database.Create(&account).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Model(&models.QuotaAccount{}).Where("id = ?", 6).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(database)
	request := manualRequest(task.ID, "duplicate-pair")
	request.SourcePath = root
	request.Accounts = []AccountInput{{AccountID: 1}, {AccountID: 5}}
	if _, err := service.CreateAnalyze(request); err == nil {
		t.Fatal("duplicate trusted config/remote pair was accepted")
	}
	request.IdempotencyKey = "disabled"
	request.Accounts = []AccountInput{{AccountID: 1}, {AccountID: 6}}
	if _, err := service.CreateAnalyze(request); err == nil {
		t.Fatal("disabled account was accepted")
	}
	request.IdempotencyKey = "sync"
	request.TransferMode = "sync"
	request.Accounts = []AccountInput{{AccountID: 1}}
	if _, err := service.CreateAnalyze(request); err == nil {
		t.Fatal("sync mode was accepted")
	}
	request.IdempotencyKey = "canonical"
	request.TransferMode = models.TransferModeCopy
	request.Accounts = []AccountInput{{AccountID: 1, AccountIdentity: "caller-defined", RemoteName: "caller-remote", ConfigIdentity: "caller-config"}, {AccountID: 3}}
	service.Start()
	result, err := service.CreateAnalyze(request)
	if err != nil {
		t.Fatal(err)
	}
	prior := waitManualRun(t, service, result.Run.ID, ManualRunStateAnalyzed)
	var account ManualRunAccount
	if err := database.Where("run_id = ? AND account_id = ?", result.Run.ID, 1).First(&account).Error; err != nil {
		t.Fatal(err)
	}
	if account.AccountIdentity != "account-a" || account.RemoteName != "remote-a" || account.ConfigIdentity != filepath.Join(root, "config-a") {
		t.Fatalf("caller identity was not replaced by trusted account: %#v", account)
	}
	request.IdempotencyKey = "same-remote-different-config"
	request.Accounts = []AccountInput{{AccountID: 1}, {AccountID: 4}}
	request.ExpectedRunID = &prior.ID
	request.ExpectedRevision = &prior.Revision
	second, err := service.CreateAnalyze(request)
	if err != nil {
		t.Fatal(err)
	}
	waitManualRun(t, service, second.Run.ID, ManualRunStateAnalyzed)
	service.Stop()
}

func seedManualRun(t *testing.T, database *gorm.DB, taskID uint, root, state string) ManualTransferRun {
	t.Helper()
	rootHandle, err := quota.OpenSourceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	run := ManualTransferRun{TaskID: taskID, State: state, Revision: 1, ActorIdentity: "operator", ActorType: "admin_session", SourcePath: root, DestinationPath: "/dest", TransferMode: models.TransferModeCopy, ConfigIdentity: "config", FrozenInput: "{}", IdempotencyKey: "seed-" + state + time.Now().Format("150405.000000"), RequestFingerprint: "seed", SourceRootDevice: rootHandle.Device, SourceRootInode: rootHandle.Inode}
	_ = rootHandle.Close()
	if err := database.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	return run
}
