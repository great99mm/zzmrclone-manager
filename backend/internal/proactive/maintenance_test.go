package proactive

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"rclone-manager/internal/models"
	"rclone-manager/internal/quota"
)

func TestCoordinatorSerializesScannerAndManualMerge(t *testing.T) {
	db := dispatcherDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}, &models.Task{}, &models.QuotaAccount{}); err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "rclone.conf")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	config := file.Name()
	task := models.Task{ID: 42, TaskType: "rotation", RotationStrategy: "proactive_quota", RcloneConfig: config, RotationRemotes: `["remote"]`, RemoteDir: "/dest"}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{DB: db, Quota: &quota.Service{DB: db, ConfigResolver: func(string) (string, error) { return config, nil }}, Now: func() time.Time { return time.Unix(100, 0) }}
	scope := models.DestinationScope(config, task.RemoteDir)
	token, err := d.acquireScannerLease(scope, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.claimManualMerge(task); !errors.Is(err, ErrCoordinatorConflict) {
		t.Fatalf("manual claim while scanner owns coordinator = %v", err)
	}
	if err := d.releaseScannerLease(scope, token); err != nil {
		t.Fatal(err)
	}
	epoch, err := d.claimManualMerge(task)
	if err != nil {
		t.Fatal(err)
	}
	if epoch.Reason != models.MaintenanceReasonManualMerge {
		t.Fatalf("manual epoch = %#v", epoch)
	}
	if _, err := d.claimManualMerge(task); !errors.Is(err, ErrManualMergeConflict) {
		t.Fatalf("duplicate manual claim = %v", err)
	}
	if _, err := d.acquireScannerLease(scope, time.Unix(100, 0)); !errors.Is(err, ErrCoordinatorConflict) {
		t.Fatalf("scanner acquired during manual epoch = %v", err)
	}
}

func TestManualMaintenanceFencesTaskAndSharedDestinationScope(t *testing.T) {
	db := dispatcherDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}); err != nil {
		t.Fatal(err)
	}
	resolver := func(string) (string, error) { return "/config", nil }
	d := &Dispatcher{DB: db, ConfigResolver: resolver}
	task := models.Task{ID: 1, RcloneConfig: "/config", RemoteDir: "/shared"}
	shared := models.Task{ID: 2, RcloneConfig: "/config", RemoteDir: "/shared"}
	other := models.Task{ID: 3, RcloneConfig: "/config", RemoteDir: "/other"}
	if err := db.Create(&models.DestinationScopeMaintenance{DestinationScope: models.DestinationScope("/config", "/shared"), Epoch: 1, OwnerTaskID: task.ID, State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStatePending, Reason: models.MaintenanceReasonManualMerge, Revision: 1}).Error; err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []models.Task{task, shared} {
		blocked, err := d.MutationBlocked(candidate)
		if err != nil || !blocked {
			t.Fatalf("task %d fence=%v err=%v", candidate.ID, blocked, err)
		}
	}
	blocked, err := d.MutationBlocked(other)
	if err != nil {
		t.Fatal(err)
	}
	if blocked {
		t.Fatal("unrelated destination scope was fenced")
	}
	legacy := models.Task{ID: 4, RcloneConfig: "/config", RemoteDir: "/legacy"}
	if err := db.Create(&models.DestinationScopeMaintenance{DestinationScope: models.DestinationScope("/config", "/legacy"), Epoch: 2, OwnerTaskID: legacy.ID, State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateRunning, Reason: models.MaintenanceReasonQuotaExhaustion, Revision: 1}).Error; err != nil {
		t.Fatal(err)
	}
	blocked, err = d.MutationBlocked(legacy)
	if err != nil || !blocked {
		t.Fatalf("legacy active fence=%v err=%v", blocked, err)
	}
}

func TestScannerLeaseHeartbeatBlocksManualClaimPastInitialLease(t *testing.T) {
	db := dispatcherDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}, &models.Task{}, &models.QuotaAccount{}); err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "rclone.conf")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	task := models.Task{ID: 43, TaskType: "rotation", RotationStrategy: "proactive_quota", RcloneConfig: file.Name(), RotationRemotes: `["remote"]`, RemoteDir: "/dest"}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{DB: db, Quota: &quota.Service{DB: db, ConfigResolver: func(string) (string, error) { return file.Name(), nil }}, ScannerLeaseDuration: 500 * time.Millisecond, ScannerLeaseHeartbeat: 50 * time.Millisecond}
	scope := models.DestinationScope(file.Name(), task.RemoteDir)
	token, err := d.acquireScannerLease(scope, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := d.startScannerLeaseHeartbeat(scope, token)
	defer func() {
		heartbeat.Stop()
		_ = d.releaseScannerLease(scope, token)
	}()
	var initial models.DestinationScopeCoordinator
	if err := db.Where("destination_scope = ?", scope).First(&initial).Error; err != nil {
		t.Fatal(err)
	}
	if initial.ScannerLeaseUntil == nil {
		t.Fatal("initial scanner lease has no expiry")
	}
	deadline := time.Now().Add(5 * time.Second)
	renewedPastInitial := false
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		var current models.DestinationScopeCoordinator
		if err := db.Where("destination_scope = ?", scope).First(&current).Error; err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if current.Revision > initial.Revision && current.ScannerLeaseUntil != nil && current.ScannerLeaseUntil.After(*initial.ScannerLeaseUntil) && now.After(*initial.ScannerLeaseUntil) && current.ScannerLeaseUntil.After(now.Add(200*time.Millisecond)) {
			renewedPastInitial = true
			break
		}
		<-ticker.C
	}
	if !renewedPastInitial {
		t.Fatal("scanner lease heartbeat was not observed after the initial lease expired")
	}
	if err := heartbeat.Err(); err != nil {
		t.Fatal(err)
	}
	var beforeClaim models.DestinationScopeCoordinator
	if err := db.Where("destination_scope = ?", scope).First(&beforeClaim).Error; err != nil {
		t.Fatal(err)
	}
	if beforeClaim.ScannerLeaseToken != token || beforeClaim.ScannerLeaseUntil == nil || !beforeClaim.ScannerLeaseUntil.After(time.Now().Add(200*time.Millisecond)) {
		t.Fatalf("scanner lease lacked safety margin before manual claim: %#v", beforeClaim)
	}
	if _, err := d.claimManualMerge(task); !errors.Is(err, ErrCoordinatorConflict) {
		t.Fatalf("manual claim after initial lease lifetime = %v", err)
	}
}

func TestManualClaimRejectsAccountWideBlockerBeforeEpoch(t *testing.T) {
	db := dispatcherDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}, &models.Task{}, &models.QuotaAccount{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &models.QuotaReservation{}); err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "rclone.conf")
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	account := models.QuotaAccount{QuotaKey: "shared", RemoteName: "remote", ConfigIdentity: file.Name(), BudgetBytes: 100, WindowSeconds: 60, Enabled: true}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	task := models.Task{ID: 44, TaskType: "rotation", RotationStrategy: "proactive_quota", RcloneConfig: file.Name(), RotationRemotes: `["remote"]`, RotationQuotaKeys: `{"remote":"shared"}`, RemoteDir: "/dest"}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	other := models.RotationQuotaBatch{TaskID: 99, QuotaAccountID: account.ID, DestinationScope: models.DestinationScope(file.Name(), "/other"), State: models.BatchStateUnknown, RequestKey: "other", OwnerToken: "owner"}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{DB: db, Quota: &quota.Service{DB: db, ConfigResolver: func(string) (string, error) { return file.Name(), nil }}}
	if _, err := d.claimManualMerge(task); !errors.Is(err, ErrManualMergeConflict) {
		t.Fatalf("manual claim with shared-account blocker = %v", err)
	}
	var count int64
	if err := db.Model(&models.DestinationScopeMaintenance{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("blocked manual claim created %d epochs", count)
	}
}

func TestManualClaimRecoveryClosesInterruptedClaim(t *testing.T) {
	db := dispatcherDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}, &models.Task{}); err != nil {
		t.Fatal(err)
	}
	lease := time.Unix(90, 0)
	epoch := models.DestinationScopeMaintenance{DestinationScope: "scope", Epoch: 1, OwnerTaskID: 1, FirstRemote: "remote", RemoteDir: "/dest", ResolvedConfigPath: "/config", ResolvedConfigIdentity: "/config", State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateClaimed, Reason: models.MaintenanceReasonManualMerge, LeaseToken: "claim", LeaseUntil: &lease, Revision: 1}
	if err := db.Create(&epoch).Error; err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{DB: db, Now: func() time.Time { return time.Unix(100, 0) }}
	if err := d.recoverMaintenanceDedupe(nil, false); err != nil {
		t.Fatal(err)
	}
	var stored models.DestinationScopeMaintenance
	if err := db.First(&stored, epoch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.MaintenanceStateClosed || stored.DedupeState != models.DedupeStateFailed {
		t.Fatalf("recovered manual claim = %#v", stored)
	}
}

func TestManualCompletionWakesTasksUsingResolvedDefaultScope(t *testing.T) {
	db := dispatcherDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}, &models.Task{}); err != nil {
		t.Fatal(err)
	}
	resolved := "/resolved/rclone.conf"
	for _, task := range []models.Task{
		{ID: 51, Enabled: true, TaskType: "rotation", RotationStrategy: "proactive_quota", RcloneConfig: "alias.conf", RemoteDir: "/dest"},
		{ID: 52, Enabled: true, TaskType: "rotation", RotationStrategy: "proactive_quota", RcloneConfig: "", RemoteDir: "/dest"},
	} {
		if err := db.Create(&task).Error; err != nil {
			t.Fatal(err)
		}
	}
	lease := time.Unix(200, 0)
	epoch := models.DestinationScopeMaintenance{DestinationScope: models.DestinationScope(resolved, "/dest"), Epoch: 1, OwnerTaskID: 51, FirstRemote: "remote", RemoteDir: "/dest", ResolvedConfigPath: resolved, ResolvedConfigIdentity: resolved, State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateRunning, Reason: models.MaintenanceReasonManualMerge, LeaseToken: "run", LeaseUntil: &lease, Revision: 2}
	if err := db.Create(&epoch).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	resolver := func(string) (string, error) { return resolved, nil }
	if err := completeManualMaintenance(db, epoch, models.DedupeStateFailed, models.DedupeStateFailed, 1, "known failure", now, resolver); err != nil {
		t.Fatal(err)
	}
	var tasks []models.Task
	if err := db.Order("id").Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if !task.RotationRescanPending || task.RotationQuotaWakeAt == nil || !task.RotationQuotaWakeAt.Equal(now) {
			t.Fatalf("task %d was not immediately woken: %#v", task.ID, task)
		}
	}
}

func TestManualCompletionUsesOwnerTokenAfterLeaseRenewal(t *testing.T) {
	db := dispatcherDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}, &models.Task{}); err != nil {
		t.Fatal(err)
	}
	oldLease := time.Unix(120, 0)
	newLease := time.Unix(300, 0)
	epoch := models.DestinationScopeMaintenance{DestinationScope: "renewed", Epoch: 1, OwnerTaskID: 1, FirstRemote: "remote", RemoteDir: "/dest", ResolvedConfigPath: "/config", ResolvedConfigIdentity: "/config", State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateRunning, Reason: models.MaintenanceReasonManualMerge, LeaseToken: "owner-token", LeaseUntil: &oldLease, Revision: 1}
	if err := db.Create(&epoch).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&epoch).Updates(map[string]interface{}{"lease_until": newLease}).Error; err != nil {
		t.Fatal(err)
	}
	stale := epoch
	if err := completeManualMaintenance(db, stale, models.DedupeStateFailed, models.DedupeStateFailed, 1, "completed", time.Unix(200, 0), nil); err != nil {
		t.Fatal(err)
	}
	var stored models.DestinationScopeMaintenance
	if err := db.First(&stored, epoch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.MaintenanceStateClosed || stored.DedupeState != models.DedupeStateFailed {
		t.Fatalf("renewed manual completion = %#v", stored)
	}
}

type manualCompletionErrorExecutor struct{ message string }

func (manualCompletionErrorExecutor) RunBatch(context.Context, uint) error { return nil }
func (e manualCompletionErrorExecutor) RunDedupe(context.Context, models.DestinationScopeMaintenance) error {
	return errors.New(e.message)
}

func TestManualCompletionWriteFailurePersistsRedactedEpochAndTaskEvidence(t *testing.T) {
	db := dispatcherDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}, &models.Task{}, &models.QuotaAccount{}); err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "manual.conf")
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	config := file.Name()
	task := models.Task{ID: 61, Enabled: true, TaskType: "rotation", RotationStrategy: "proactive_quota", RcloneConfig: config, RotationRemotes: `["remote"]`, RotationQuotaKeys: `{"remote":"manual-key"}`, RemoteDir: "/dest"}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.QuotaAccount{QuotaKey: "manual-key", RemoteName: "remote", ConfigIdentity: config, BudgetBytes: 100, WindowSeconds: 60, Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{DB: db, Quota: &quota.Service{DB: db, ConfigResolver: func(string) (string, error) { return config, nil }}, Executor: manualCompletionErrorExecutor{message: config + " 0123456789abcdef0123456789abcdef0123456789abcdef"}, ConfigResolver: func(string) (string, error) { return config, nil }, Now: func() time.Time { return time.Unix(100, 0) }}
	epoch, err := d.claimManualMerge(task)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RunClaimedManualMerge(context.Background(), task, epoch); err == nil {
		t.Fatal("completion error was swallowed")
	}
	var storedEpoch models.DestinationScopeMaintenance
	if err := db.First(&storedEpoch, epoch.ID).Error; err != nil {
		t.Fatal(err)
	}
	var storedTask models.Task
	if err := db.First(&storedTask, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"epoch": storedEpoch.LastError, "task": storedTask.LastError} {
		if value == "" || strings.Contains(value, config) || strings.Contains(value, "0123456789abcdef0123456789abcdef0123456789abcdef") {
			t.Fatalf("%s evidence missing/redacted: %q", name, value)
		}
	}
}
