package proactive

import (
	"context"
	"errors"
	"fmt"
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

type fixedScanner struct {
	snapshots []quota.LocalSnapshot
	entered   chan struct{}
	release   chan struct{}
}
type errorScanner struct{ err error }

type leaseLossScanner struct{ DB *gorm.DB }

func (s leaseLossScanner) Scan(string, time.Duration) ([]quota.LocalSnapshot, error) {
	expired := time.Unix(90, 0)
	if err := s.DB.Model(&models.DestinationScopeCoordinator{}).Where("scanner_lease_token <> ''").Update("scanner_lease_until", expired).Error; err != nil {
		return nil, err
	}
	return nil, nil
}

func (s errorScanner) Scan(string, time.Duration) ([]quota.LocalSnapshot, error) { return nil, s.err }

func (s fixedScanner) Scan(string, time.Duration) ([]quota.LocalSnapshot, error) {
	if s.entered != nil {
		close(s.entered)
		<-s.release
	}
	return s.snapshots, nil
}

type dispatchFakeExecutor struct {
	DB      *gorm.DB
	mu      sync.Mutex
	calls   []uint
	unknown map[uint]bool
	runErr  error
}

type parallelDispatchExecutor struct {
	mu       sync.Mutex
	calls    []uint
	expected int
	started  chan struct{}
	release  <-chan struct{}
	once     sync.Once
}

func (e *parallelDispatchExecutor) RunBatch(_ context.Context, batchID uint) error {
	e.mu.Lock()
	e.calls = append(e.calls, batchID)
	if len(e.calls) == e.expected {
		e.once.Do(func() { close(e.started) })
	}
	e.mu.Unlock()
	<-e.release
	return errors.New("test batch stop")
}

type startupMoveInspector struct{ stopped bool }

type alwaysLiveDedupeInspector struct{ stops int }

type legacyRecoveryInspector struct {
	statuses map[int]ProcessStatus
	stopped  map[int]bool
	stops    int
}

func (i *startupMoveInspector) Inspect(int, string) (ProcessStatus, error) {
	return ProcessStatus{Confirmed: true, Alive: !i.stopped}, nil
}

func (i *startupMoveInspector) StopVerified(int, string) error {
	i.stopped = true
	return nil
}

func (i *alwaysLiveDedupeInspector) Inspect(int, string) (ProcessStatus, error) {
	return ProcessStatus{Confirmed: true, Alive: true}, nil
}

func (i *alwaysLiveDedupeInspector) StopVerified(int, string) error {
	i.stops++
	return nil
}

func (i *legacyRecoveryInspector) Inspect(pid int, _ string) (ProcessStatus, error) {
	status := i.statuses[pid]
	if i.stopped[pid] {
		status.Alive = false
		status.Confirmed = true
	}
	return status, nil
}

func (i *legacyRecoveryInspector) StopVerified(pid int, _ string) error {
	i.stops++
	i.stopped[pid] = true
	return nil
}

func (f *dispatchFakeExecutor) RunBatch(_ context.Context, id uint) error {
	f.mu.Lock()
	f.calls = append(f.calls, id)
	f.mu.Unlock()
	var b models.RotationQuotaBatch
	if err := f.DB.First(&b, id).Error; err != nil {
		return err
	}
	if f.runErr != nil {
		return f.runErr
	}
	if f.unknown != nil && f.unknown[id] {
		_ = f.DB.Model(&models.RotationQuotaBatch{}).Where("id = ?", id).Update("state", models.BatchStateUnknown).Error
		_ = f.DB.Model(&models.RotationQuotaBatchFile{}).Where("batch_id = ?", id).Update("state", models.BatchFileStateUnknown).Error
		_ = f.DB.Model(&models.QuotaReservation{}).Where("batch_id = ?", id).Update("state", models.ReservationStateUnknown).Error
		return nil
	}
	_ = f.DB.Model(&models.RotationQuotaBatchFile{}).Where("batch_id = ?", id).Update("state", models.BatchFileStateCommitted).Error
	_ = f.DB.Model(&models.QuotaReservation{}).Where("batch_id = ?", id).Update("state", models.ReservationStateCommitted).Error
	return f.DB.Model(&models.RotationQuotaBatch{}).Where("id = ?", id).Update("state", models.BatchStateSucceeded).Error
}

func TestRecoverContinuesAfterRetryableGroupConflicts(t *testing.T) {
	for name, runErr := range map[string]error{"blocked": ErrAccountBlocked, "lease": ErrLeaseConflict, "scope owner": errors.New("unknown scope owner")} {
		t.Run(name, func(t *testing.T) {
			db := dispatcherDB(t)
			task, _, _ := dispatcherFixture(t, db, `["r1"]`)
			batch := models.RotationQuotaBatch{TaskID: task.ID, RequestKey: "recover-" + name, State: models.BatchStatePlanned, DestinationRemote: "r1", RcloneConfigPath: task.RcloneConfig, TransferMode: models.TransferModeCopy, DestinationScopeVersion: 1, DestinationPath: task.RemoteDir, DestinationScope: models.DestinationScope(task.RcloneConfig, task.RemoteDir), OwnerToken: "owner-recover"}
			if err := db.Create(&batch).Error; err != nil {
				t.Fatal(err)
			}
			d := newDispatcher(db, &dispatchFakeExecutor{DB: db, runErr: runErr}, fixedScanner{})
			if err := d.Recover(context.Background()); err != nil {
				t.Fatalf("retryable recovery failed: %v", err)
			}
			var stored models.Task
			if err := db.First(&stored, task.ID).Error; err != nil {
				t.Fatal(err)
			}
			if !stored.RotationRescanPending || stored.RotationQuotaWakeAt == nil || stored.LastError == "" {
				t.Fatalf("retry evidence missing: %#v", stored)
			}
		})
	}
}

func TestRecoverStopsLiveMoveBeforeLocalRecovery(t *testing.T) {
	db := dispatcherDB(t)
	task, _, config := dispatcherFixture(t, db, `["r1"]`)
	started := time.Unix(90, 0)
	batch := models.RotationQuotaBatch{TaskID: task.ID, RequestKey: "live-move", State: models.BatchStateRunning, DestinationRemote: "r1", RcloneConfigPath: config, TransferMode: models.TransferModeMove, DestinationScopeVersion: 1, DestinationPath: task.RemoteDir, DestinationScope: models.DestinationScope(config, task.RemoteDir), OwnerToken: testOwner, LeaseToken: "live-lease", StartedAt: &started, ProcessID: 123, ProcessStartToken: "123:1"}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	inspector := &startupMoveInspector{}
	d := newDispatcher(db, &dispatchFakeExecutor{DB: db}, fixedScanner{})
	d.Inspector = inspector
	if err := d.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !inspector.stopped {
		t.Fatal("live move was not stopped during startup recovery")
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateSucceeded {
		t.Fatalf("state = %s, want succeeded", stored.State)
	}
}

func TestRecoverDedupeSkipsLeaseRenewedBetweenSelectAndClaim(t *testing.T) {
	db := dispatcherDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}); err != nil {
		t.Fatal(err)
	}
	expired := time.Unix(90, 0)
	row := models.DestinationScopeMaintenance{DestinationScope: "scope", Epoch: 1, State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateRunning, Reason: models.MaintenanceReasonManualMerge, LeaseToken: "dedupe-lease", LeaseUntil: &expired, ProcessID: 123, ProcessStartToken: "123:1"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	inspector := &startupMoveInspector{}
	d := &Dispatcher{DB: db, Inspector: inspector, Now: func() time.Time { return time.Unix(100, 0) }}
	d.maintenanceRecoveryBeforeClaim = func(selected models.DestinationScopeMaintenance) {
		renewed := time.Unix(200, 0)
		if err := db.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND lease_token = ?", selected.ID, selected.LeaseToken).Updates(map[string]interface{}{"lease_until": renewed}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := d.recoverMaintenanceDedupe(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if inspector.stopped {
		t.Fatal("live dedupe was stopped after its lease was renewed")
	}
	var stored models.DestinationScopeMaintenance
	if err := db.First(&stored, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.DedupeState != models.DedupeStateRunning || stored.LeaseUntil == nil || !stored.LeaseUntil.Equal(time.Unix(200, 0)) {
		t.Fatalf("renewed dedupe row changed: %#v", stored)
	}
}

func TestRecoverForceClaimsFutureNilAndExpiredDedupeLeases(t *testing.T) {
	db := dispatcherDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	expired := time.Unix(90, 0)
	future := time.Unix(200, 0)
	rows := []models.DestinationScopeMaintenance{
		{DestinationScope: "future", Epoch: 1, State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateRunning, Reason: models.MaintenanceReasonManualMerge, LeaseToken: "future-token", LeaseUntil: &future, ProcessID: 1, ProcessStartToken: "1:1"},
		{DestinationScope: "nil", Epoch: 1, State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateRunning, Reason: models.MaintenanceReasonManualMerge, LeaseToken: "nil-token", ProcessID: 2, ProcessStartToken: "2:1"},
		{DestinationScope: "expired", Epoch: 1, State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateRunning, Reason: models.MaintenanceReasonManualMerge, LeaseToken: "expired-token", LeaseUntil: &expired, ProcessID: 3, ProcessStartToken: "3:1"},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	inspector := &alwaysLiveDedupeInspector{}
	d := &Dispatcher{DB: db, Inspector: inspector, Now: func() time.Time { return now }}
	if err := d.recoverMaintenanceDedupe(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if inspector.stops != len(rows) {
		t.Fatalf("stops = %d, want %d", inspector.stops, len(rows))
	}
	for _, row := range rows {
		var stored models.DestinationScopeMaintenance
		if err := db.First(&stored, row.ID).Error; err != nil {
			t.Fatal(err)
		}
		if stored.DedupeState != models.DedupeStateUnknown {
			t.Fatalf("scope %s state = %s, want unknown", row.DestinationScope, stored.DedupeState)
		}
	}
}

func TestRecoverLegacyQuotaEpochsClosesOnlyKnownStoppedRows(t *testing.T) {
	db := dispatcherDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}); err != nil {
		t.Fatal(err)
	}
	rows := []models.DestinationScopeMaintenance{
		{DestinationScope: "legacy-dead", Epoch: 1, OwnerTaskID: 1, FirstRemote: "remote", RemoteDir: "/dest", ResolvedConfigPath: "/config", ResolvedConfigIdentity: "/config", State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateRunning, Reason: models.MaintenanceReasonQuotaExhaustion, LeaseToken: "dead", ProcessID: 1, ProcessStartToken: "1:1"},
		{DestinationScope: "legacy-live", Epoch: 1, OwnerTaskID: 1, FirstRemote: "remote", RemoteDir: "/dest", ResolvedConfigPath: "/config", ResolvedConfigIdentity: "/config", State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateRunning, Reason: models.MaintenanceReasonQuotaExhaustion, LeaseToken: "live", ProcessID: 2, ProcessStartToken: "2:1"},
		{DestinationScope: "legacy-ambiguous", Epoch: 1, OwnerTaskID: 1, FirstRemote: "remote", RemoteDir: "/dest", ResolvedConfigPath: "/config", ResolvedConfigIdentity: "/config", State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateRunning, Reason: models.MaintenanceReasonQuotaExhaustion, LeaseToken: "ambiguous"},
		{DestinationScope: "legacy-mismatch", Epoch: 1, OwnerTaskID: 1, FirstRemote: "remote", RemoteDir: "/dest", ResolvedConfigPath: "/config", ResolvedConfigIdentity: "/config", State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateRunning, Reason: models.MaintenanceReasonQuotaExhaustion, LeaseToken: "mismatch", ProcessID: 3, ProcessStartToken: "3:1"},
		{DestinationScope: "legacy-pending", Epoch: 1, OwnerTaskID: 1, FirstRemote: "remote", RemoteDir: "/dest", ResolvedConfigPath: "/config", ResolvedConfigIdentity: "/config", State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStatePending, Reason: models.MaintenanceReasonQuotaExhaustion},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	inspector := &legacyRecoveryInspector{statuses: map[int]ProcessStatus{1: {Confirmed: true, Alive: false}, 2: {Confirmed: true, Alive: true}, 3: {Confirmed: false, Alive: true}}, stopped: map[int]bool{}}
	d := &Dispatcher{DB: db, Inspector: inspector, Now: func() time.Time { return time.Unix(100, 0) }}
	if err := d.recoverMaintenanceDedupe(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if inspector.stops != 1 {
		t.Fatalf("legacy recovery stop count=%d, want one identity-safe stop", inspector.stops)
	}
	for scope, want := range map[string]string{"legacy-dead": models.MaintenanceStateClosed, "legacy-live": models.MaintenanceStateClosed, "legacy-ambiguous": models.DedupeStateUnknown, "legacy-mismatch": models.DedupeStateUnknown, "legacy-pending": models.MaintenanceStateClosed} {
		var stored models.DestinationScopeMaintenance
		if err := db.Where("destination_scope = ?", scope).First(&stored).Error; err != nil {
			t.Fatal(err)
		}
		if (scope == "legacy-dead" || scope == "legacy-live" || scope == "legacy-pending") && stored.State != want {
			t.Fatalf("%s state=%s, want closed", scope, stored.State)
		}
		if (scope == "legacy-ambiguous" || scope == "legacy-mismatch") && stored.DedupeState != want {
			t.Fatalf("%s dedupe state=%s, want unknown", scope, stored.DedupeState)
		}
	}
}

func TestRecoverForceDoesNotTakeRenewedFutureDedupeLease(t *testing.T) {
	db := dispatcherDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	future := time.Unix(200, 0)
	row := models.DestinationScopeMaintenance{DestinationScope: "renewed-future", Epoch: 1, State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateRunning, Reason: models.MaintenanceReasonManualMerge, LeaseToken: "live-token", LeaseUntil: &future, ProcessID: 10, ProcessStartToken: "10:1"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	inspector := &alwaysLiveDedupeInspector{}
	d := &Dispatcher{DB: db, Inspector: inspector, Now: func() time.Time { return now }}
	d.maintenanceRecoveryBeforeClaim = func(selected models.DestinationScopeMaintenance) {
		renewed := time.Unix(300, 0)
		if err := db.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND lease_token = ?", selected.ID, selected.LeaseToken).Update("lease_until", renewed).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := d.recoverMaintenanceDedupe(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if inspector.stops != 0 {
		t.Fatal("renewed future dedupe was stopped")
	}
	var stored models.DestinationScopeMaintenance
	if err := db.First(&stored, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.DedupeState != models.DedupeStateRunning || stored.LeaseToken != "live-token" || stored.LeaseUntil == nil || !stored.LeaseUntil.Equal(time.Unix(300, 0)) {
		t.Fatalf("renewed future dedupe changed: %#v", stored)
	}
}

func TestRecoverReturnsPermanentGroupFailure(t *testing.T) {
	db := dispatcherDB(t)
	task, _, _ := dispatcherFixture(t, db, `["r1"]`)
	batch := models.RotationQuotaBatch{TaskID: task.ID, RequestKey: "recover-permanent", State: models.BatchStatePlanned, DestinationRemote: "r1", RcloneConfigPath: task.RcloneConfig, TransferMode: models.TransferModeCopy, DestinationScopeVersion: 1, DestinationPath: task.RemoteDir, DestinationScope: models.DestinationScope(task.RcloneConfig, task.RemoteDir), OwnerToken: "owner-permanent"}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	d := newDispatcher(db, &dispatchFakeExecutor{DB: db, runErr: errors.New("permanent config failure")}, fixedScanner{})
	if err := d.Recover(context.Background()); err == nil {
		t.Fatal("permanent recovery failure was swallowed")
	}
}

func TestRequestScanFencedByActiveManualMaintenance(t *testing.T) {
	db := dispatcherDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}); err != nil {
		t.Fatal(err)
	}
	task, snapshot, config := dispatcherFixture(t, db, `["r1"]`)
	if err := db.Model(&task).Update("status", "idle").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.DestinationScopeMaintenance{DestinationScope: models.DestinationScope(config, task.RemoteDir), Epoch: 1, OwnerTaskID: task.ID, State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStatePending, Reason: models.MaintenanceReasonManualMerge, Revision: 1}).Error; err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{DB: db, Quota: &quota.Service{DB: db, ConfigResolver: func(raw string) (string, error) { return raw, nil }}, Executor: &dispatchFakeExecutor{DB: db}, Scanner: func(models.Task) LocalScanner { return fixedScanner{snapshots: []quota.LocalSnapshot{snapshot}} }, Now: func() time.Time { return time.Unix(100, 0) }}
	// RequestScan should be a no-op when maintenance is active — maintenancePaused
	// will return true and cause the scan to be skipped without error.
	if err := d.RequestScan(context.Background(), task.ID); err != nil {
		t.Fatalf("RequestScan during active maintenance returned err=%v (expected no-op)", err)
	}
	// No batches should have been created since the scan was blocked.
	var count int64
	if err := db.Model(&models.RotationQuotaBatch{}).Where("task_id = ?", task.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("RequestScan created %d batches despite active maintenance fence", count)
	}
}

func TestRequestScanHeartbeatLossDoesNotClearNoEligiblePending(t *testing.T) {
	db := dispatcherDB(t)
	task, _, config := dispatcherFixture(t, db, `["r1"]`)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}); err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{DB: db, Quota: &quota.Service{DB: db, ConfigResolver: func(raw string) (string, error) { return raw, nil }}, Executor: &dispatchFakeExecutor{DB: db}, Scanner: func(models.Task) LocalScanner { return leaseLossScanner{DB: db} }, Now: func() time.Time { return time.Unix(100, 0) }}
	if err := d.RequestScan(context.Background(), task.ID); err == nil {
		t.Fatal("lost scanner lease was ignored")
	}
	var stored models.Task
	if err := db.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !stored.RotationRescanPending || stored.RotationQuotaWakeAt == nil {
		t.Fatalf("pending state was cleared after heartbeat loss: %#v", stored)
	}
	var count int64
	if err := db.Model(&models.RotationQuotaBatch{}).Where("task_id = ?", task.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 || config == "" {
		t.Fatalf("heartbeat loss created reservation batches: count=%d config=%q", count, config)
	}
}

func (f *dispatchFakeExecutor) RecoverBatch(_ context.Context, id uint) error {
	return f.DB.Model(&models.RotationQuotaBatch{}).Where("id = ?", id).Update("state", models.BatchStateSucceeded).Error
}

func dispatcherDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "dispatcher.db") + "?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.Task{}, &models.QuotaAccount{}, &models.RotationQuotaOversize{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &models.QuotaReservation{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func dispatcherFixture(t *testing.T, db *gorm.DB, remotes string) (models.Task, quota.LocalSnapshot, string) {
	t.Helper()
	root := t.TempDir()
	config := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("dispatch"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("[test]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	shots, err := (quota.Scanner{}).Scan(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	task := models.Task{Name: "pool", SourceType: "local", SourceDir: root, DestType: "remote", RemoteName: "r1", RemoteDir: "/shared", TransferMode: "copy", RcloneConfig: config, TaskType: "rotation", RotationStrategy: "proactive_quota", RotationRemotes: remotes, MinAge: "0s", Enabled: true, QBEnabled: false, RotationQuotaLimitBytes: 100}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	return task, shots[0], config
}

func newDispatcher(db *gorm.DB, fake *dispatchFakeExecutor, scanner fixedScanner) *Dispatcher {
	return &Dispatcher{DB: db, Quota: &quota.Service{DB: db, ConfigResolver: func(raw string) (string, error) { return raw, nil }, Now: func() time.Time { return time.Unix(100, 0) }}, Executor: fake, Scanner: func(models.Task) LocalScanner { return scanner }, Now: func() time.Time { return time.Unix(100, 0) }}
}

func TestDispatcherUpsertsDefaultAndExplicitQuotaKeys(t *testing.T) {
	db := dispatcherDB(t)
	task, snapshot, config := dispatcherFixture(t, db, "[\"r1\",\"r2\"]")
	fake := &dispatchFakeExecutor{DB: db}
	d := newDispatcher(db, fake, fixedScanner{snapshots: []quota.LocalSnapshot{snapshot}})
	if err := d.RequestScan(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	var accounts []models.QuotaAccount
	if err := db.Find(&accounts).Error; err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 || accounts[0].ConfigIdentity != config {
		t.Fatalf("accounts=%#v", accounts)
	}
	db2 := dispatcherDB(t)
	task2, snapshot2, _ := dispatcherFixture(t, db2, "[\"r1\",\"r2\"]")
	if err := db2.Model(&models.Task{}).Where("id = ?", task2.ID).Update("rotation_quota_keys", `{"r1":"shared","r2":"shared"}`).Error; err != nil {
		t.Fatal(err)
	}
	fake2 := &dispatchFakeExecutor{DB: db2}
	if err := newDispatcher(db2, fake2, fixedScanner{snapshots: []quota.LocalSnapshot{snapshot2}}).RequestScan(context.Background(), task2.ID); err != nil {
		t.Fatal(err)
	}
	var shared []models.QuotaAccount
	if err := db2.Find(&shared).Error; err != nil {
		t.Fatal(err)
	}
	if len(shared) != 1 || shared[0].QuotaKey != "shared" {
		t.Fatalf("shared=%#v", shared)
	}
}

func TestBudgetExhaustionLeavesPendingWakeWithoutMaintenanceOrDedupe(t *testing.T) {
	db := dispatcherDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}); err != nil {
		t.Fatal(err)
	}
	task, snapshot, config := dispatcherFixture(t, db, `["r1"]`)
	key := defaultQuotaKey(config, "r1")
	if err := db.Create(&models.QuotaAccount{QuotaKey: key, RemoteName: "r1", ConfigIdentity: config, BudgetBytes: 100, WindowSeconds: 3600, Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	account := models.QuotaAccount{}
	if err := db.Where("quota_key = ?", key).First(&account).Error; err != nil {
		t.Fatal(err)
	}
	expires := time.Unix(3700, 0)
	if err := db.Create(&models.QuotaReservation{QuotaAccountID: account.ID, Bytes: 100, State: models.ReservationStateCommitted, ExpiresAt: &expires}).Error; err != nil {
		t.Fatal(err)
	}
	d := newDispatcher(db, &dispatchFakeExecutor{DB: db}, fixedScanner{snapshots: []quota.LocalSnapshot{snapshot}})
	if err := d.RequestScan(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	var epochs []models.DestinationScopeMaintenance
	if err := db.Find(&epochs).Error; err != nil {
		t.Fatal(err)
	}
	if len(epochs) != 0 {
		t.Fatalf("automatic maintenance epochs = %#v", epochs)
	}
	var stored models.Task
	if err := db.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !stored.RotationRescanPending || stored.RotationQuotaWakeAt == nil || !stored.RotationQuotaWakeAt.Equal(time.Unix(3700, 0)) {
		t.Fatalf("budget exhaustion did not preserve pending ledger wake: %#v", stored)
	}
}

func TestPersistPendingStateZeroQuotaDoesNotRestoreDefaultCapacity(t *testing.T) {
	db := dispatcherDB(t)
	task, snapshot, config := dispatcherFixture(t, db, `["r1"]`)
	task.RotationQuotaLimitBytes = 0
	key := defaultQuotaKey(config, "r1")
	if err := db.Create(&models.QuotaAccount{QuotaKey: key, RemoteName: "r1", ConfigIdentity: config, BudgetBytes: models.DefaultRotationQuotaLimitBytes, WindowSeconds: 86400, Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	d := newDispatcher(db, &dispatchFakeExecutor{DB: db}, fixedScanner{snapshots: []quota.LocalSnapshot{snapshot}})
	if err := d.persistPendingState(task, map[string]string{"r1": key}, []quota.LocalSnapshot{snapshot}, time.Unix(100, 0), 0, true); err != nil {
		t.Fatal(err)
	}
	var oversized int64
	if err := db.Model(&models.RotationQuotaOversize{}).Where("task_id = ?", task.ID).Count(&oversized).Error; err != nil {
		t.Fatal(err)
	}
	if oversized != 1 {
		t.Fatalf("zero quota did not classify file as oversized: %d", oversized)
	}
}

func TestRequestScanPersistsBusinessErrorAndRetryWake(t *testing.T) {
	db := dispatcherDB(t)
	task, _, _ := dispatcherFixture(t, db, "[\"r1\"]")
	scanErr := errors.New("scan failed")
	d := newDispatcher(db, &dispatchFakeExecutor{DB: db}, fixedScanner{})
	d.Scanner = func(models.Task) LocalScanner { return errorScanner{err: scanErr} }
	if err := d.RequestScan(context.Background(), task.ID); !errors.Is(err, scanErr) {
		t.Fatalf("error=%v", err)
	}
	var stored models.Task
	if err := db.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.LastError == "" || stored.RotationQuotaWakeAt == nil {
		t.Fatalf("error persistence missing: %#v", stored)
	}
}

func TestDispatcherConcurrentScanOnlyRunsOnceAndLeavesPending(t *testing.T) {
	db := dispatcherDB(t)
	task, snapshot, _ := dispatcherFixture(t, db, "[\"r1\"]")
	fake := &dispatchFakeExecutor{DB: db}
	entered := make(chan struct{})
	release := make(chan struct{})
	d := newDispatcher(db, fake, fixedScanner{snapshots: []quota.LocalSnapshot{snapshot}, entered: entered, release: release})
	done := make(chan error, 2)
	go func() { done <- d.RequestScan(context.Background(), task.ID) }()
	<-entered
	go func() { done <- d.RequestScan(context.Background(), task.ID) }()
	if err := <-done; err != nil && !errors.Is(err, ErrPendingSuperseded) {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil && !errors.Is(err, ErrPendingSuperseded) {
		t.Fatal(err)
	}
	if d.IsActive(task.ID) {
		t.Fatal("dispatcher remained active")
	}
	var stored models.Task
	if err := db.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !stored.RotationRescanPending || stored.RotationRescanGeneration <= stored.RotationRescanHandledGeneration {
		t.Fatalf("pending generation was lost: %#v", stored)
	}
	if stored.RotationQuotaWakeAt == nil || stored.RotationQuotaWakeAt.After(time.Unix(160, 0)) {
		t.Fatalf("superseded generation lost immediate wake: %#v", stored)
	}
}

func TestPendingGenerationCASContentionRetriesWithFreshGeneration(t *testing.T) {
	db := dispatcherDB(t)
	task, _, _ := dispatcherFixture(t, db, "[\"r1\"]")
	d := newDispatcher(db, &dispatchFakeExecutor{DB: db}, fixedScanner{})
	d.RetryMax = 12
	d.RetrySleep = func(time.Duration) { time.Sleep(50 * time.Millisecond) }
	const workers = 8
	done := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() { _, err := d.markPending(task.ID); done <- err }()
	}
	for i := 0; i < workers; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	var stored models.Task
	if err := db.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RotationRescanGeneration != workers || !stored.RotationRescanPending {
		t.Fatalf("generation=%d pending=%v", stored.RotationRescanGeneration, stored.RotationRescanPending)
	}
}

func TestDispatcherOversizedFileSetsErrorAndDoesNotLoop(t *testing.T) {
	db := dispatcherDB(t)
	task, snapshot, _ := dispatcherFixture(t, db, "[\"r1\"]")
	if err := db.Model(&models.Task{}).Where("id = ?", task.ID).Update("rotation_quota_limit_bytes", 1).Error; err != nil {
		t.Fatal(err)
	}
	fake := &dispatchFakeExecutor{DB: db}
	d := newDispatcher(db, fake, fixedScanner{snapshots: []quota.LocalSnapshot{snapshot}})
	if err := d.RequestScan(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	var stored models.Task
	if err := db.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.LastError == "" || stored.RotationRescanPending {
		t.Fatalf("task=%#v", stored)
	}
	if len(fake.calls) != 0 {
		t.Fatal("oversized file was dispatched")
	}
}

func TestDispatcherSchedulesImmediateFollowUpForBatchLimitPendingFiles(t *testing.T) {
	db := dispatcherDB(t)
	task, snapshot, _ := dispatcherFixture(t, db, "[\"r1\"]")
	snapshots := make([]quota.LocalSnapshot, 6)
	for i := range snapshots {
		snapshots[i] = snapshot
		snapshots[i].RelativePath = fmt.Sprintf("file-%d.mkv", i)
		snapshots[i].SnapshotKey = fmt.Sprintf("snapshot-%d", i)
	}
	wake := &recordingWake{}
	fake := &dispatchFakeExecutor{DB: db}
	d := newDispatcher(db, fake, fixedScanner{snapshots: snapshots})
	d.Wake = wake

	if err := d.RequestScan(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("completed batches=%d, want 1", len(fake.calls))
	}

	var stored models.Task
	if err := db.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	want := time.Unix(100, 0)
	if !stored.RotationRescanPending || stored.RotationQuotaWakeAt == nil || !stored.RotationQuotaWakeAt.Equal(want) || !wake.at.Equal(want) {
		t.Fatalf("remaining files did not schedule an immediate follow-up: task=%#v wake=%#v", stored, wake)
	}
}

type recordingWake struct {
	taskID uint
	at     time.Time
}

func (w *recordingWake) ScheduleWake(id uint, at time.Time) { w.taskID = id; w.at = at }

func TestDispatcherTemporaryNoFitPersistsWake(t *testing.T) {
	db := dispatcherDB(t)
	task, snapshot, config := dispatcherFixture(t, db, "[\"r1\"]")
	wakeAt := time.Unix(200, 0)
	account := models.QuotaAccount{QuotaKey: defaultQuotaKey(config, "r1"), BudgetBytes: 100, WindowSeconds: 86400, Enabled: true, ProviderBlockedUntil: &wakeAt}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	wake := &recordingWake{}
	fake := &dispatchFakeExecutor{DB: db}
	d := newDispatcher(db, fake, fixedScanner{snapshots: []quota.LocalSnapshot{snapshot}})
	d.Wake = wake
	if err := d.RequestScan(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	var stored models.Task
	if err := db.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RotationQuotaWakeAt == nil || !stored.RotationQuotaWakeAt.Equal(wakeAt) || !wake.at.Equal(wakeAt) {
		t.Fatalf("task=%#v wake=%#v", stored, wake)
	}
}

func TestDispatcherWakeIncludesHeldReservationExpiry(t *testing.T) {
	db := dispatcherDB(t)
	task, snapshot, config := dispatcherFixture(t, db, "[\"r1\"]")
	wakeAt := time.Unix(200, 0)
	account := models.QuotaAccount{QuotaKey: defaultQuotaKey(config, "r1"), BudgetBytes: 100, WindowSeconds: 86400, Enabled: true}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	dummy := models.RotationQuotaBatch{TaskID: 999, DestinationScope: "dummy", DestinationRemote: "dummy", TransferMode: models.TransferModeCopy, DestinationScopeVersion: 1, RcloneConfigPath: config, RequestKey: "dummy", RequestFingerprint: "dummy", State: models.BatchStateSucceeded, OwnerToken: testOwner}
	if err := db.Create(&dummy).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchID: dummy.ID, BatchFileID: 999, Bytes: 95, State: models.ReservationStateHeld, IdempotencyKey: "held-only", ExpiresAt: &wakeAt}).Error; err != nil {
		t.Fatal(err)
	}
	wake := &recordingWake{}
	d := newDispatcher(db, &dispatchFakeExecutor{DB: db}, fixedScanner{snapshots: []quota.LocalSnapshot{snapshot}})
	d.Wake = wake
	if err := d.RequestScan(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	var stored models.Task
	if err := db.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RotationQuotaWakeAt == nil || !stored.RotationQuotaWakeAt.Equal(wakeAt) {
		t.Fatalf("held expiry wake missing: %#v", stored)
	}
}

func TestRetryWakePreservesEarliestCandidate(t *testing.T) {
	db := dispatcherDB(t)
	task, _, _ := dispatcherFixture(t, db, "[\"r1\"]")
	later := time.Unix(100+24*60*60, 0)
	if err := db.Model(&models.Task{}).Where("id = ?", task.ID).Updates(map[string]interface{}{"rotation_rescan_pending": true, "rotation_rescan_generation": 1, "rotation_quota_wake_at": later}).Error; err != nil {
		t.Fatal(err)
	}
	d := newDispatcher(db, &dispatchFakeExecutor{DB: db}, fixedScanner{})
	if err := d.persistRetryWake(task.ID); err != nil {
		t.Fatal(err)
	}
	var stored models.Task
	if err := db.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RotationQuotaWakeAt == nil || !stored.RotationQuotaWakeAt.Equal(time.Unix(160, 0)) {
		t.Fatalf("retry wake was not earliest: %v", stored.RotationQuotaWakeAt)
	}
}

func TestDispatcherDoesNotRechargeCommittedSnapshotButAcceptsChangedKey(t *testing.T) {
	db := dispatcherDB(t)
	task, snapshot, _ := dispatcherFixture(t, db, "[\"r1\"]")
	current := []quota.LocalSnapshot{snapshot}
	fake := &dispatchFakeExecutor{DB: db}
	d := newDispatcher(db, fake, fixedScanner{})
	d.Scanner = func(models.Task) LocalScanner { return fixedScanner{snapshots: current} }
	if err := d.RequestScan(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&models.RotationQuotaBatch{}).Where("task_id = ?", task.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("initial batches=%d", count)
	}
	if err := d.RequestScan(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.RotationQuotaBatch{}).Where("task_id = ?", task.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("committed snapshot was recharged: %d", count)
	}
	changed := snapshot
	changed.SnapshotKey = "changed-snapshot-key"
	current = []quota.LocalSnapshot{changed}
	if err := d.RequestScan(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.RotationQuotaBatch{}).Where("task_id = ?", task.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("changed snapshot was not reserved: %d", count)
	}
}

func TestConcurrentFirstAliasUpsertIsBoundedAndDeterministic(t *testing.T) {
	db := dispatcherDB(t)
	task, snapshot, _ := dispatcherFixture(t, db, "[\"r1\",\"r2\"]")
	d1 := newDispatcher(db, &dispatchFakeExecutor{DB: db}, fixedScanner{snapshots: []quota.LocalSnapshot{snapshot}})
	d2 := newDispatcher(db, &dispatchFakeExecutor{DB: db}, fixedScanner{snapshots: []quota.LocalSnapshot{snapshot}})
	d1.Scanner = func(models.Task) LocalScanner { return fixedScanner{snapshots: []quota.LocalSnapshot{snapshot}} }
	d2.Scanner = d1.Scanner
	if err := db.Model(&models.Task{}).Where("id = ?", task.ID).Update("rotation_quota_keys", `{"r1":"shared","r2":"shared"}`).Error; err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { done <- d1.RequestScan(context.Background(), task.ID) }()
	go func() { done <- d2.RequestScan(context.Background(), task.ID) }()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil && (strings.Contains(strings.ToLower(err.Error()), "busy") || strings.Contains(strings.ToLower(err.Error()), "unique")) {
			t.Fatal(err)
		}
	}
	var accounts []models.QuotaAccount
	if err := db.Find(&accounts).Error; err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].RemoteName != "r1" {
		t.Fatalf("accounts=%#v", accounts)
	}
}

func TestDispatcherUnknownSiblingReleasesLaterHeldOnly(t *testing.T) {
	db := dispatcherDB(t)
	task, _, config := dispatcherFixture(t, db, "[\"r1\"]")
	account := models.QuotaAccount{QuotaKey: "manual", BudgetBytes: 100, WindowSeconds: 86400, Enabled: true}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	for i := 0; i < 2; i++ {
		remote := "r1"
		if i == 1 {
			remote = "r2"
		}
		b := models.RotationQuotaBatch{TaskID: task.ID, QuotaAccountID: account.ID, DestinationScope: models.DestinationScope(config, "/shared"), DestinationRemote: remote, TransferMode: models.TransferModeCopy, DestinationScopeVersion: 1, RcloneConfigPath: config, RequestKey: "group", RequestFingerprint: "f", DestinationPath: "/shared", State: models.BatchStateReserved, OwnerToken: testOwner}
		if i == 1 {
			b.OwnerToken = "0123456789abcdef0123456789abcdef0123456789abcdee"
		}
		if err := db.Create(&b).Error; err != nil {
			t.Fatal(err)
		}
		bf := models.RotationQuotaBatchFile{BatchID: b.ID, RelativePath: "file.txt", SnapshotKey: "k" + string(rune('0'+i)), State: models.BatchFileStateHeld}
		if err := db.Create(&bf).Error; err != nil {
			t.Fatal(err)
		}
		exp := now.Add(time.Hour)
		if err := db.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchID: b.ID, BatchFileID: bf.ID, Bytes: 1, State: models.ReservationStateHeld, IdempotencyKey: "id" + string(rune('0'+i)), ExpiresAt: &exp}).Error; err != nil {
			t.Fatal(err)
		}
	}
	fake := &dispatchFakeExecutor{DB: db, unknown: map[uint]bool{1: true}}
	d := newDispatcher(db, fake, fixedScanner{})
	if err := db.Model(&models.Task{}).Where("id = ?", task.ID).Updates(map[string]interface{}{"rotation_rescan_pending": true, "rotation_rescan_generation": 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := d.executeGroup(context.Background(), task.ID, "group"); err != nil {
		t.Fatal(err)
	}
	var later models.RotationQuotaBatch
	if err := db.First(&later, 2).Error; err != nil {
		t.Fatal(err)
	}
	if later.State != models.BatchStateCanceled {
		t.Fatalf("later=%s", later.State)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("calls=%v", fake.calls)
	}
	var taskState models.Task
	if err := db.First(&taskState, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if taskState.RotationQuotaWakeAt == nil || taskState.RotationQuotaWakeAt.After(time.Unix(160, 0)) {
		t.Fatalf("failed group retry wake missing: %#v", taskState)
	}
}

func TestDispatcherRunsDistinctAccountsInParallelUpToTaskLimit(t *testing.T) {
	db := dispatcherDB(t)
	task, _, config := dispatcherFixture(t, db, `["r1","r2","r3","r4"]`)
	if err := db.Model(&models.Task{}).Where("id = ?", task.ID).Update("rotation_concurrent_batches", 4).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		batch := models.RotationQuotaBatch{TaskID: task.ID, QuotaAccountID: uint(i + 1), DestinationScope: models.DestinationScope(config, "/shared"), DestinationRemote: fmt.Sprintf("r%d", i+1), TransferMode: models.TransferModeCopy, DestinationScopeVersion: 1, RcloneConfigPath: config, RequestKey: "parallel", RequestFingerprint: "parallel", DestinationPath: "/shared", State: models.BatchStateReserved, OwnerToken: fmt.Sprintf("%040d", i+1), RotationConcurrentBatches: 4}
		if err := db.Create(&batch).Error; err != nil {
			t.Fatal(err)
		}
	}
	release := make(chan struct{})
	fake := &parallelDispatchExecutor{expected: 4, started: make(chan struct{}), release: release}
	dispatcher := &Dispatcher{DB: db, Executor: fake}
	done := make(chan error, 1)
	go func() { done <- dispatcher.executeGroup(context.Background(), task.ID, "parallel") }()
	select {
	case <-fake.started:
	case <-time.After(time.Second):
		t.Fatal("four batches did not enter execution concurrently")
	}
	close(release)
	if err := <-done; err == nil || err.Error() != "test batch stop" {
		t.Fatalf("executeGroup error = %v", err)
	}
}

type failingInspector struct{}

func (failingInspector) Inspect(int, string) (ProcessStatus, error) {
	return ProcessStatus{}, errors.New("pid unavailable")
}

func TestRecoveryInspectorFailureMarksStartedBatchUnknownWithoutReconcile(t *testing.T) {
	db := dispatcherDB(t)
	task, _, config := dispatcherFixture(t, db, "[\"r1\"]")
	started := time.Unix(100, 0)
	batch := models.RotationQuotaBatch{TaskID: task.ID, DestinationScope: models.DestinationScope(config, "/shared"), DestinationRemote: "r1", TransferMode: models.TransferModeCopy, DestinationScopeVersion: 1, RcloneConfigPath: config, RequestKey: "recovery", RequestFingerprint: "recovery", DestinationPath: "/shared", State: models.BatchStateReserved, OwnerToken: testOwner, LeaseToken: testOwner, StartedAt: &started, ProcessID: 999, ProcessStartToken: testOwner}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	fake := &dispatchFakeExecutor{DB: db}
	d := newDispatcher(db, fake, fixedScanner{})
	d.Inspector = failingInspector{}
	if err := d.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateUnknown {
		t.Fatalf("state=%s", stored.State)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("recovery re-copied: %v", fake.calls)
	}
}

func TestComputeWakeAnchoredToFirstExhaustion(t *testing.T) {
	db := dispatcherDB(t)
	if err := db.AutoMigrate(&models.QuotaAccount{}); err != nil {
		t.Fatal(err)
	}
	exhaustedAt := time.Unix(500, 0)
	anchor := models.QuotaAccount{QuotaKey: "drive", RemoteName: "drive", BudgetBytes: 100, WindowSeconds: 86400, Enabled: true, WindowStartedAt: &exhaustedAt}
	if err := db.Create(&anchor).Error; err != nil {
		t.Fatal(err)
	}
	other := models.QuotaAccount{QuotaKey: "team-1", RemoteName: "team-1", BudgetBytes: 100, WindowSeconds: 86400, Enabled: true}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{DB: db, Now: func() time.Time { return time.Unix(1000, 0) }}
	wake, err := d.computeWake(map[string]string{"drive": "drive", "team-1": "team-1"}, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if wake == nil {
		t.Fatal("expected wake anchored to first exhaustion")
	}
	expected := exhaustedAt.Add(24 * time.Hour)
	if !wake.Equal(expected) {
		t.Fatalf("wake = %v, want %v", wake.UTC(), expected.UTC())
	}
}

func TestComputeWakeReturnsNilWhenNoAnchor(t *testing.T) {
	db := dispatcherDB(t)
	if err := db.AutoMigrate(&models.QuotaAccount{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.QuotaAccount{QuotaKey: "drive", RemoteName: "drive", BudgetBytes: 100, WindowSeconds: 86400, Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.QuotaAccount{QuotaKey: "team-1", RemoteName: "team-1", BudgetBytes: 100, WindowSeconds: 86400, Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{DB: db, Now: func() time.Time { return time.Unix(1000, 0) }}
	wake, err := d.computeWake(map[string]string{"drive": "drive", "team-1": "team-1"}, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if wake != nil {
		t.Fatalf("expected nil wake when no anchor is set, got %v", wake)
	}
}
