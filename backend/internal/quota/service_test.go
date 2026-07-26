package quota

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
)

func newQuotaTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "quota.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.QuotaAccount{}, &models.RotationQuotaDirectoryAssignment{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &models.QuotaReservation{}); err != nil {
		t.Fatal(err)
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(4)
		sqlDB.SetMaxIdleConns(2)
		t.Cleanup(func() { _ = sqlDB.Close() })
	} else {
		t.Fatal(err)
	}
	return db
}

func quotaTask(remotes []string, keys map[string]string, limit int64) models.Task {
	return models.Task{
		ID: 7, RcloneConfig: "config-identity", TransferMode: models.TransferModeCopy, RotationRemotes: models.EncodeRotationRemotes(remotes),
		RotationQuotaKeys: models.EncodeRotationQuotaKeys(keys), RotationQuotaLimitBytes: limit,
	}
}

func addAccount(t *testing.T, db *gorm.DB, key string, budget int64) models.QuotaAccount {
	t.Helper()
	account := models.QuotaAccount{QuotaKey: key, RemoteName: key, BudgetBytes: budget, Enabled: true, WindowSeconds: 3600}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	return account
}

func TestReserveKeepsZeroTaskQuotaIndependentOfFixedAccountBudget(t *testing.T) {
	db := newQuotaTestDB(t)
	addAccount(t, db, "key", 100)
	service := testService(db, time.Unix(100, 0))
	task := quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 0)
	result, err := service.Reserve(PackReserveRequest{Task: task, Snapshots: []LocalSnapshot{snapshot("file", 1)}, SourceRoot: "/source", DestinationPath: "/dest"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Batches) != 0 || len(result.Pending) != 1 {
		t.Fatalf("zero task ceiling was ignored: batches=%d pending=%d", len(result.Batches), len(result.Pending))
	}
	var current models.QuotaAccount
	if err := db.First(&current, "quota_key = ?", "key").Error; err != nil {
		t.Fatal(err)
	}
	if current.BudgetBytes != models.DefaultRotationQuotaLimitBytes || current.WindowSeconds != models.DefaultQuotaWindowSeconds {
		t.Fatalf("account budget/window = %d/%d", current.BudgetBytes, current.WindowSeconds)
	}
}

func TestReserveBlocksExhaustedRecoveryAfterLegacyProviderBlockExpires(t *testing.T) {
	db := newQuotaTestDB(t)
	account := addAccount(t, db, "recovery-key", 100)
	now := time.Unix(100, 0)
	past := now.Add(-time.Minute)
	if err := db.Model(&models.QuotaAccount{}).Where("id = ?", account.ID).Updates(map[string]interface{}{
		"provider_blocked_until": past,
		"recovery_state":         models.QuotaRecoveryStateExhausted,
		"recovery_generation":    1,
		"window_started_at":      now.Add(-time.Hour),
	}).Error; err != nil {
		t.Fatal(err)
	}
	service := testService(db, now)
	result, err := service.Reserve(PackReserveRequest{
		Task:      quotaTask([]string{"remote"}, map[string]string{"remote": account.QuotaKey}, 100),
		Snapshots: []LocalSnapshot{snapshot("file", 1)}, SourceRoot: "/source", DestinationPath: "/dest",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Classification != models.ReserveClassProviderBlocked || len(result.Batches) != 0 || len(result.Pending) != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestReserveClassifiesOtherScopeAccountBlockerBeforeBudgetExhaustion(t *testing.T) {
	db := newQuotaTestDB(t)
	account := addAccount(t, db, "key", 1)
	service := testService(db, time.Unix(100, 0))
	otherScope := models.DestinationScope("config-identity", "/other")
	lease := time.Unix(130, 0)
	if err := db.Create(&models.RotationQuotaBatch{TaskID: 99, QuotaAccountID: account.ID, DestinationScope: otherScope, State: models.BatchStateRunning, RequestKey: "other-scope", LeaseUntil: &lease}).Error; err != nil {
		t.Fatal(err)
	}
	result, err := service.Reserve(PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 1), Snapshots: []LocalSnapshot{snapshot("file", 1)}, SourceRoot: "/source", DestinationPath: "/dest"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Classification != models.ReserveClassAccountBlocked || len(result.Batches) != 0 || len(result.Pending) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if result.RetryAt == nil || !result.RetryAt.Equal(lease) {
		t.Fatalf("blocker retry wake = %v", result.RetryAt)
	}
	var count int64
	if err := db.Model(&models.RotationQuotaBatch{}).Where("task_id = ?", 7).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("blocked reservation created %d batches", count)
	}
}

func TestReserveClassifiesAllCrossScopeActiveLedgerStatesAsBlockers(t *testing.T) {
	db := newQuotaTestDB(t)
	account := addAccount(t, db, "key", 100)
	service := testService(db, time.Unix(100, 0))
	otherScope := models.DestinationScope("config-identity", "/other")
	lease := time.Unix(150, 0)
	for i, state := range []string{models.BatchStatePlanned, models.BatchStateReserved, models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown} {
		if err := db.Create(&models.RotationQuotaBatch{TaskID: uint(100 + i), QuotaAccountID: account.ID, DestinationScope: otherScope, State: state, RequestKey: fmt.Sprintf("batch-%d", i), OwnerToken: fmt.Sprintf("owner-%d", i), LeaseUntil: &lease}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for i, state := range []string{models.ReservationStateHeld, models.ReservationStateActive, models.ReservationStateUnknown} {
		expires := time.Unix(140+int64(i), 0)
		if err := db.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchID: uint(i + 1), BatchFileID: uint(i + 1), Bytes: 1, State: state, IdempotencyKey: fmt.Sprintf("reservation-%d", i), ExpiresAt: &expires}).Error; err != nil {
			t.Fatal(err)
		}
	}
	result, err := service.Reserve(PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 100), Snapshots: []LocalSnapshot{snapshot("file", 1)}, SourceRoot: "/source", DestinationPath: "/dest"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Classification != models.ReserveClassAccountBlocked || result.RetryAt == nil || !result.RetryAt.After(time.Unix(100, 0)) {
		t.Fatalf("result = %#v", result)
	}
}

func TestReserveCrossScopeBlockerWithoutLeaseOrExpiryGetsFutureWake(t *testing.T) {
	db := newQuotaTestDB(t)
	account := addAccount(t, db, "key", 100)
	service := testService(db, time.Unix(100, 0))
	otherScope := models.DestinationScope("config-identity", "/other")
	batch := models.RotationQuotaBatch{TaskID: 500, QuotaAccountID: account.ID, DestinationScope: otherScope, State: models.BatchStateReconciling, RequestKey: "nil-lease", OwnerToken: "nil-owner"}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchID: batch.ID, Bytes: 1, State: models.ReservationStateUnknown, IdempotencyKey: "nil-expiry"}).Error; err != nil {
		t.Fatal(err)
	}
	result, err := service.Reserve(PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 100), Snapshots: []LocalSnapshot{snapshot("file", 1)}, SourceRoot: "/source", DestinationPath: "/dest"})
	if err != nil {
		t.Fatal(err)
	}
	want := time.Unix(100, 0).Add(time.Duration(models.DefaultQuotaWindowSeconds) * time.Second)
	if result.Classification != models.ReserveClassAccountBlocked || result.RetryAt == nil || !result.RetryAt.After(time.Unix(100, 0)) || !result.RetryAt.Equal(want) {
		t.Fatalf("result = %#v, want future fallback wake %v", result, want)
	}
}

func TestReserveCrossScopeReservedBatchWithoutLeaseGetsReconciliationWake(t *testing.T) {
	db := newQuotaTestDB(t)
	account := addAccount(t, db, "key", 100)
	service := testService(db, time.Unix(100, 0))
	foreign := models.RotationQuotaBatch{TaskID: 500, QuotaAccountID: account.ID, DestinationScope: models.DestinationScope("config-identity", "/other"), State: models.BatchStateReserved, RequestKey: "reserved-no-lease", OwnerToken: "reserved-owner"}
	if err := db.Create(&foreign).Error; err != nil {
		t.Fatal(err)
	}
	result, err := service.Reserve(PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 100), Snapshots: []LocalSnapshot{snapshot("file", 1)}, SourceRoot: "/source", DestinationPath: "/dest"})
	if err != nil {
		t.Fatal(err)
	}
	want := time.Unix(100, 0).Add(time.Minute)
	if result.Classification != models.ReserveClassAccountBlocked || result.RetryAt == nil || !result.RetryAt.Equal(want) {
		t.Fatalf("reserved no-lease blocker result = %#v, want wake %v", result, want)
	}
}

func TestReserveCrossScopeUnknownReservationWakesAtReconciliationExpiry(t *testing.T) {
	db := newQuotaTestDB(t)
	start := time.Unix(100, 0)
	account := models.QuotaAccount{QuotaKey: "key", RemoteName: "remote", BudgetBytes: models.DefaultRotationQuotaLimitBytes, WindowSeconds: models.DefaultQuotaWindowSeconds, Enabled: true, WindowStartedAt: &start}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	foreign := models.RotationQuotaBatch{TaskID: 99, QuotaAccountID: account.ID, DestinationScope: models.DestinationScope("config-identity", "/foreign"), State: models.BatchStateRunning, RequestKey: "foreign", OwnerToken: "foreign-owner"}
	if err := db.Create(&foreign).Error; err != nil {
		t.Fatal(err)
	}
	reconcileAt := time.Unix(140, 0)
	if err := db.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchID: foreign.ID, BatchFileID: 1, Bytes: 1, State: models.ReservationStateUnknown, IdempotencyKey: "foreign-unknown", ExpiresAt: &reconcileAt}).Error; err != nil {
		t.Fatal(err)
	}
	service := testService(db, start)
	result, err := service.Reserve(PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": account.QuotaKey}, 10), Snapshots: []LocalSnapshot{snapshot("file", 1)}, SourceRoot: "/source", DestinationPath: "/local"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Classification != models.ReserveClassAccountBlocked || result.RetryAt == nil || !result.RetryAt.Equal(reconcileAt) {
		t.Fatalf("unknown reservation wake = %#v, want %v", result, reconcileAt)
	}
	var reservation models.QuotaReservation
	if err := db.Where("batch_id = ?", foreign.ID).First(&reservation).Error; err != nil {
		t.Fatal(err)
	}
	wantDeadline := AccountWindowEnd(account)
	if reservation.ExpiresAt == nil || !reservation.ExpiresAt.Equal(wantDeadline) {
		t.Fatalf("unknown reservation deadline = %v, want account boundary %v", reservation.ExpiresAt, wantDeadline)
	}
	var storedBatch models.RotationQuotaBatch
	if err := db.First(&storedBatch, foreign.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedBatch.LeaseUntil == nil || !storedBatch.LeaseUntil.Equal(reconcileAt) {
		t.Fatalf("unknown reservation retry hint = %v, want %v", storedBatch.LeaseUntil, reconcileAt)
	}
}

func TestReserveRejectsLostScannerCoordinatorOwnership(t *testing.T) {
	db := newQuotaTestDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeCoordinator{}); err != nil {
		t.Fatal(err)
	}
	account := addAccount(t, db, "key", 100)
	service := testService(db, time.Unix(100, 0))
	task := quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 100)
	resolved, err := service.ResolveConfigPath(task.RcloneConfig)
	if err != nil {
		t.Fatal(err)
	}
	scope := models.DestinationScope(resolved, "/dest")
	lease := time.Unix(200, 0)
	if err := db.Create(&models.DestinationScopeCoordinator{DestinationScope: scope, ScannerLeaseToken: "other-scanner", ScannerLeaseUntil: &lease}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.Reserve(PackReserveRequest{Task: task, Snapshots: []LocalSnapshot{snapshot("file", 1)}, RequestIdempotencyKey: "lost-scanner", SourceRoot: "/source", DestinationPath: "/dest", CoordinatorLeaseToken: "stale-scanner"})
	if !errors.Is(err, ErrCoordinatorConflict) {
		t.Fatalf("lost scanner ownership error = %v", err)
	}
	var count int64
	if err := db.Model(&models.RotationQuotaBatch{}).Where("quota_account_id = ?", account.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("lost scanner created %d batches", count)
	}
}

func TestReserveFinalCoordinatorCheckRejectsLeaseLostAfterInitialCheck(t *testing.T) {
	db := newQuotaTestDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeCoordinator{}); err != nil {
		t.Fatal(err)
	}
	account := addAccount(t, db, "key", 100)
	service := testService(db, time.Unix(100, 0))
	task := quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 100)
	resolved, err := service.ResolveConfigPath(task.RcloneConfig)
	if err != nil {
		t.Fatal(err)
	}
	scope := models.DestinationScope(resolved, "/dest")
	lease := time.Unix(200, 0)
	coordinator := models.DestinationScopeCoordinator{DestinationScope: scope, ScannerLeaseToken: "scanner", ScannerLeaseUntil: &lease}
	if err := db.Create(&coordinator).Error; err != nil {
		t.Fatal(err)
	}
	service.BeforeFinalReservationCheck = func(tx *gorm.DB) {
		expired := time.Unix(90, 0)
		_ = tx.Model(&models.DestinationScopeCoordinator{}).Where("destination_scope = ? AND scanner_lease_token = ?", scope, "scanner").Updates(map[string]interface{}{"scanner_lease_token": "replacement", "scanner_lease_until": expired}).Error
	}
	_, err = service.Reserve(PackReserveRequest{Task: task, Snapshots: []LocalSnapshot{snapshot("file", 1)}, RequestIdempotencyKey: "final-loss", SourceRoot: "/source", DestinationPath: "/dest", CoordinatorLeaseToken: "scanner"})
	if !errors.Is(err, ErrCoordinatorConflict) {
		t.Fatalf("final ownership loss error = %v", err)
	}
	var count int64
	if err := db.Model(&models.RotationQuotaBatch{}).Where("quota_account_id = ?", account.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("final ownership loss created %d batches", count)
	}
}

func snapshot(path string, size int64) LocalSnapshot {
	return LocalSnapshot{RelativePath: path, SizeBytes: size, MtimeNS: 1, Device: 2, Inode: size + 10, RootDevice: 3, RootInode: 4, SnapshotKey: path + "-snapshot"}
}

func testService(db *gorm.DB, now time.Time) Service {
	file, err := os.CreateTemp("", "phase2-rclone-*.conf")
	if err != nil {
		panic(err)
	}
	configPath := file.Name()
	if _, err := file.WriteString("[test]\n"); err != nil {
		panic(err)
	}
	if err := file.Close(); err != nil {
		panic(err)
	}
	next := 0
	var tokenMu sync.Mutex
	return Service{DB: db, Now: func() time.Time { return now }, TokenGenerator: func() string {
		tokenMu.Lock()
		defer tokenMu.Unlock()
		next++
		return fmt.Sprintf("token-%d", next)
	}, ConfigResolver: func(string) (string, error) { return configPath, nil }, MaxRetries: 4}
}

func TestReserveFirstFitSharedCapacityAndIdempotency(t *testing.T) {
	db := newQuotaTestDB(t)
	addAccount(t, db, "key-a", 10)
	addAccount(t, db, "key-b", 10)
	service := testService(db, time.Unix(100, 0))
	req := PackReserveRequest{
		Task:                  quotaTask([]string{"remote-b", "remote-a"}, map[string]string{"remote-a": "key-a", "remote-b": "key-b"}, 10),
		Snapshots:             []LocalSnapshot{snapshot("z", 7), snapshot("a", 4), snapshot("b", 3)},
		RequestIdempotencyKey: "request-1", SourceRoot: t.TempDir(), DestinationPath: "/dest",
	}
	result, err := service.Reserve(req)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if len(result.Batches) != 1 || len(result.Pending) != 1 || result.Pending[0].RelativePath != "z" {
		t.Fatalf("result = batches %d pending %d", len(result.Batches), len(result.Pending))
	}
	if result.Batches[0].DestinationRemote != "remote-b" || result.Batches[0].ReservedBytes != 7 {
		t.Fatalf("first-fit batch = %#v", result.Batches[0])
	}

	retry, err := service.Reserve(req)
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if !retry.Existing || len(retry.Batches) != 1 || len(retry.Pending) != 1 {
		t.Fatalf("retry result = %#v", retry)
	}
	var reservationCount int64
	if err := db.Model(&models.QuotaReservation{}).Count(&reservationCount).Error; err != nil {
		t.Fatal(err)
	}
	if reservationCount != 2 {
		t.Fatalf("reservation count = %d, want 3", reservationCount)
	}
}

func TestPartialReserveRetryRebuildsPending(t *testing.T) {
	db := newQuotaTestDB(t)
	addAccount(t, db, "key", 5)
	service := testService(db, time.Unix(100, 0))
	request := PackReserveRequest{
		Task:                  quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 5),
		Snapshots:             []LocalSnapshot{snapshot("a", 3), snapshot("b", 3)},
		RequestIdempotencyKey: "partial-request", SourceRoot: t.TempDir(), DestinationPath: "/partial",
	}
	first, err := service.Reserve(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Batches) != 1 || len(first.Pending) != 1 || first.Pending[0].RelativePath != "b" {
		t.Fatalf("first partial result = %#v", first)
	}
	retry, err := service.Reserve(request)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Existing || len(retry.Pending) != 1 || retry.Pending[0].RelativePath != "b" {
		t.Fatalf("retry partial result = %#v", retry)
	}
}

func TestReserveRespectsConfiguredBatchFileLimit(t *testing.T) {
	db := newQuotaTestDB(t)
	addAccount(t, db, "key", 20)
	service := testService(db, time.Unix(100, 0))
	task := quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 20)
	task.RotationBatchFiles = 2
	task.RotationConcurrentBatches = 4
	task.Transfers = 16
	result, err := service.Reserve(PackReserveRequest{
		Task: task, Snapshots: []LocalSnapshot{snapshot("a", 3), snapshot("b", 3), snapshot("c", 3)},
		RequestIdempotencyKey: "batch-file-limit", SourceRoot: t.TempDir(), DestinationPath: "/dest",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Batches) != 1 || len(result.Pending) != 1 || result.Pending[0].RelativePath != "c" {
		t.Fatalf("batches=%d pending=%#v", len(result.Batches), result.Pending)
	}
	var files int64
	if err := db.Model(&models.RotationQuotaBatchFile{}).Where("batch_id = ?", result.Batches[0].ID).Count(&files).Error; err != nil {
		t.Fatal(err)
	}
	if files != 2 || result.Batches[0].RcloneTransfers != task.Transfers || result.Batches[0].RotationConcurrentBatches != task.RotationConcurrentBatches {
		t.Fatalf("files=%d transfers=%d concurrent=%d", files, result.Batches[0].RcloneTransfers, result.Batches[0].RotationConcurrentBatches)
	}
}

func TestReserveKeepsLeafDirectoryOnOneQuotaAccount(t *testing.T) {
	db := newQuotaTestDB(t)
	accountA := addAccount(t, db, "key-a", 100)
	addAccount(t, db, "key-b", 100)
	service := testService(db, time.Unix(100, 0))
	task := quotaTask([]string{"remote-a", "remote-b"}, map[string]string{"remote-a": "key-a", "remote-b": "key-b"}, 100)
	task.RotationBatchFiles = 1
	request := PackReserveRequest{
		Task: task,
		Snapshots: []LocalSnapshot{
			snapshot("series-a/Season 1/episode-01.mkv", 1),
			snapshot("series-a/Season 1/episode-02.mkv", 1),
			snapshot("series-b/Season 1/episode-01.mkv", 1),
		},
		RequestIdempotencyKey: "directory-affinity-1", SourceRoot: t.TempDir(), DestinationPath: "/dest",
	}
	first, err := service.Reserve(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Batches) != 2 || len(first.Pending) != 1 || first.Pending[0].RelativePath != "series-a/Season 1/episode-02.mkv" {
		t.Fatalf("first=%#v", first)
	}
	var assignment models.RotationQuotaDirectoryAssignment
	if err := db.Where("task_id = ? AND directory = ?", task.ID, "series-a/Season 1").First(&assignment).Error; err != nil {
		t.Fatal(err)
	}
	if assignment.QuotaAccountID != accountA.ID {
		t.Fatalf("directory account=%d, want %d", assignment.QuotaAccountID, accountA.ID)
	}
	if err := db.Model(&models.RotationQuotaBatch{}).Where("task_id = ?", task.ID).Update("state", models.BatchStateSucceeded).Error; err != nil {
		t.Fatal(err)
	}
	second, err := service.Reserve(PackReserveRequest{Task: task, Snapshots: first.Pending, RequestIdempotencyKey: "directory-affinity-2", SourceRoot: request.SourceRoot, DestinationPath: request.DestinationPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Batches) != 1 || second.Batches[0].QuotaAccountID != accountA.ID {
		t.Fatalf("second=%#v", second)
	}
}

func TestRequestIdentityFingerprintAndMissingKey(t *testing.T) {
	db := newQuotaTestDB(t)
	addAccount(t, db, "key", 20)
	service := testService(db, time.Unix(100, 0))
	root := t.TempDir()
	req := PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 20), Snapshots: []LocalSnapshot{snapshot("file", 2)}, RequestIdempotencyKey: "same-key", SourceRoot: root, DestinationPath: "/dest"}
	if _, err := service.Reserve(req); err != nil {
		t.Fatal(err)
	}
	retry, err := service.Reserve(req)
	if err != nil || !retry.Existing {
		t.Fatalf("same fingerprint was not idempotent: result=%#v err=%v", retry, err)
	}
	unchanged := req
	unchanged.Task.Transfers = 32
	unchanged.Task.Checkers = 64
	unchanged.Task.Retries = 9
	if result, err := service.Reserve(unchanged); err != nil || !result.Existing {
		t.Fatalf("execution tuning changed request identity: result=%#v err=%v", result, err)
	}
	conflict := req
	conflict.Snapshots = []LocalSnapshot{snapshot("different", 2)}
	if _, err := service.Reserve(conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("fingerprint conflict error = %v", err)
	}
	quotaChanged := req
	quotaChanged.Task.RotationQuotaLimitBytes = 10
	if _, err := service.Reserve(quotaChanged); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("quota ceiling fingerprint conflict error = %v", err)
	}
	moveRequest := req
	moveRequest.Task.TransferMode = "move"
	if _, err := service.Reserve(moveRequest); err == nil {
		t.Fatal("move transfer mode was accepted")
	}
	if err := db.Model(&models.RotationQuotaBatch{}).Where("task_id = ?", req.Task.ID).Update("state", models.BatchStateSucceeded).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.QuotaReservation{}).Where("batch_id IN (SELECT id FROM rotation_quota_batches WHERE task_id = ?)", req.Task.ID).Update("state", models.ReservationStateReleased).Error; err != nil {
		t.Fatal(err)
	}

	missing := req
	missing.RequestIdempotencyKey = ""
	missing.Task.ID = 8
	missing.DestinationPath = "/other"
	missingResult, err := service.Reserve(missing)
	if err != nil || missingResult.Existing {
		t.Fatalf("missing key was not a new request: result=%#v err=%v", missingResult, err)
	}
	var batches []models.RotationQuotaBatch
	if err := db.Order("id").Find(&batches).Error; err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || batches[0].RequestKey == batches[1].RequestKey || batches[0].OwnerToken == batches[1].OwnerToken {
		t.Fatalf("request/owner identities = %#v", batches)
	}
}

func TestReserveEnabledMovePersistsMoveBatchMode(t *testing.T) {
	db := newQuotaTestDB(t)
	addAccount(t, db, "move-key", 20)
	service := testService(db, time.Unix(100, 0))
	service.MoveEnabled = func() bool { return true }
	task := quotaTask([]string{"remote"}, map[string]string{"remote": "move-key"}, 20)
	task.TransferMode = models.TransferModeMove
	result, err := service.Reserve(PackReserveRequest{Task: task, Snapshots: []LocalSnapshot{snapshot("file", 2)}, RequestIdempotencyKey: "enabled-move", SourceRoot: t.TempDir(), DestinationPath: "/dest"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Batches) != 1 || result.Batches[0].TransferMode != models.TransferModeMove {
		t.Fatalf("reserved move batch = %#v", result.Batches)
	}
}

func TestDestinationScopeCanonicalizationRejectsCrossTaskCollision(t *testing.T) {
	db := newQuotaTestDB(t)
	addAccount(t, db, "key", 20)
	service := testService(db, time.Unix(100, 0))
	root := t.TempDir()
	firstTask := quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 20)
	firstTask.ID = 101
	firstTask.RcloneConfig = ""
	if _, err := service.Reserve(PackReserveRequest{Task: firstTask, Snapshots: []LocalSnapshot{snapshot("first", 1)}, RequestIdempotencyKey: "first", SourceRoot: root, DestinationPath: "foo/../dest"}); err != nil {
		t.Fatal(err)
	}
	secondTask := firstTask
	secondTask.ID = 102
	secondTask.RcloneConfig = models.DefaultRcloneConfigPath
	if _, err := service.Reserve(PackReserveRequest{Task: secondTask, Snapshots: []LocalSnapshot{snapshot("second", 1)}, RequestIdempotencyKey: "second", SourceRoot: root, DestinationPath: "/dest"}); !errors.Is(err, ErrActiveBatch) {
		t.Fatalf("canonical destination scope error = %v", err)
	}
	if models.DestinationScope("/resolved/default/rclone.conf", "foo/../dest") != models.DestinationScope("/resolved/default/rclone.conf", "/dest") {
		t.Fatal("default and explicit rclone config scopes differ")
	}
}

func TestConfigPinningAndSymlinkRetarget(t *testing.T) {
	db := newQuotaTestDB(t)
	addAccount(t, db, "key", 20)
	temp := t.TempDir()
	target1 := filepath.Join(temp, "config-one.conf")
	target2 := filepath.Join(temp, "config-two.conf")
	link := filepath.Join(temp, "config-link.conf")
	if err := os.WriteFile(target1, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target2, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target1, link); err != nil {
		t.Fatal(err)
	}
	ownerNumber := 0
	service := Service{DB: db, Now: func() time.Time { return time.Unix(100, 0) }, TokenGenerator: func() string { ownerNumber++; return fmt.Sprintf("unique-owner-%d", ownerNumber) }}
	task := quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 20)
	task.ID = 301
	task.RcloneConfig = link
	first, err := service.Reserve(PackReserveRequest{Task: task, Snapshots: []LocalSnapshot{snapshot("one", 1)}, RequestIdempotencyKey: "pin-one", SourceRoot: t.TempDir(), DestinationPath: "/same"})
	if err != nil {
		t.Fatal(err)
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, first.Batches[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	absTarget1, _ := filepath.Abs(target1)
	if stored.RcloneConfigPath != filepath.Clean(absTarget1) {
		t.Fatalf("pinned config = %q, want %q", stored.RcloneConfigPath, absTarget1)
	}

	directTask := task
	directTask.ID = 302
	directTask.RcloneConfig = target1
	if _, err := service.Reserve(PackReserveRequest{Task: directTask, Snapshots: []LocalSnapshot{snapshot("two", 1)}, RequestIdempotencyKey: "pin-two", SourceRoot: t.TempDir(), DestinationPath: "/same"}); !errors.Is(err, ErrActiveBatch) {
		t.Fatalf("alias/direct scope collision error = %v", err)
	}
	if err := db.Model(&models.RotationQuotaBatch{}).Where("id = ?", stored.ID).Update("state", models.BatchStateSucceeded).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.QuotaReservation{}).Where("batch_id = ?", stored.ID).Update("state", models.ReservationStateReleased).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target2, link); err != nil {
		t.Fatal(err)
	}
	retargetedTask := task
	retargetedTask.ID = 303
	third, err := service.Reserve(PackReserveRequest{Task: retargetedTask, Snapshots: []LocalSnapshot{snapshot("three", 1)}, RequestIdempotencyKey: "pin-three", SourceRoot: t.TempDir(), DestinationPath: "/same"})
	if err != nil {
		t.Fatal(err)
	}
	var retargeted models.RotationQuotaBatch
	if err := db.First(&retargeted, third.Batches[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	absTarget2, _ := filepath.Abs(target2)
	if retargeted.RcloneConfigPath != filepath.Clean(absTarget2) || stored.RcloneConfigPath != filepath.Clean(absTarget1) {
		t.Fatalf("config pinning changed: old=%q new=%q", stored.RcloneConfigPath, retargeted.RcloneConfigPath)
	}
}

func TestScannerSnapshotsPersistRootIdentity(t *testing.T) {
	db := newQuotaTestDB(t)
	addAccount(t, db, "key", 20)
	root := t.TempDir()
	filePath := filepath.Join(root, "file.txt")
	if err := os.WriteFile(filePath, []byte("scanner"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshots, err := (Scanner{}).Scan(root, 0)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	service := testService(db, time.Unix(100, 0))
	task := quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 20)
	task.ID = 401
	result, err := service.Reserve(PackReserveRequest{Task: task, Snapshots: snapshots, RequestIdempotencyKey: "scanner-root", SourceRoot: root, DestinationPath: "/dest"})
	if err != nil {
		t.Fatal(err)
	}
	var batch models.RotationQuotaBatch
	if err := db.First(&batch, result.Batches[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	if batch.SourceRootDevice != snapshots[0].RootDevice || batch.SourceRootInode != snapshots[0].RootInode {
		t.Fatalf("persisted root identity=(%d,%d), scan=(%d,%d)", batch.SourceRootDevice, batch.SourceRootInode, snapshots[0].RootDevice, snapshots[0].RootInode)
	}
	bad := append([]LocalSnapshot(nil), snapshots...)
	bad[0].RootInode++
	if _, err := service.Reserve(PackReserveRequest{Task: task, Snapshots: bad, RequestIdempotencyKey: "scanner-root-bad", SourceRoot: root, DestinationPath: "/other"}); err == nil {
		t.Fatal("inconsistent root identity was accepted")
	}
}

func TestServiceRejectsNonCanonicalSnapshotAliases(t *testing.T) {
	db := newQuotaTestDB(t)
	addAccount(t, db, "key", 20)
	service := testService(db, time.Unix(100, 0))
	for _, relative := range []string{"./b", "a/../b", "a//b"} {
		_, err := service.Reserve(PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 20), Snapshots: []LocalSnapshot{snapshot(relative, 1)}, RequestIdempotencyKey: relative, SourceRoot: t.TempDir(), DestinationPath: "/dest"})
		if err == nil {
			t.Fatalf("non-canonical relative path %q was accepted", relative)
		}
	}
}

func TestReserveNoSplitAndPendingAtBoundary(t *testing.T) {
	db := newQuotaTestDB(t)
	addAccount(t, db, "key", 10)
	service := testService(db, time.Unix(100, 0))
	result, err := service.Reserve(PackReserveRequest{
		Task:                  quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 10),
		Snapshots:             []LocalSnapshot{snapshot("fits", 10), snapshot("oversize", 1)},
		RequestIdempotencyKey: "boundary", SourceRoot: t.TempDir(), DestinationPath: "/dest",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Batches) != 1 || result.Batches[0].ReservedBytes != 10 || len(result.Pending) != 1 || result.Pending[0].RelativePath != "oversize" {
		t.Fatalf("boundary result = %#v", result)
	}
}

func TestReserveDefaultQuotaLimitBoundary(t *testing.T) {
	db := newQuotaTestDB(t)
	addAccount(t, db, "key", models.DefaultRotationQuotaLimitBytes)
	service := testService(db, time.Unix(100, 0))
	result, err := service.Reserve(PackReserveRequest{
		Task:                  quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, models.DefaultRotationQuotaLimitBytes),
		Snapshots:             []LocalSnapshot{snapshot("700-gib", models.DefaultRotationQuotaLimitBytes)},
		RequestIdempotencyKey: "700-gib-boundary", SourceRoot: t.TempDir(), DestinationPath: "/dest",
	})
	if err != nil || len(result.Batches) != 1 || result.Batches[0].ReservedBytes != models.DefaultRotationQuotaLimitBytes {
		t.Fatalf("default quota boundary result=%#v err=%v", result, err)
	}
}

func TestReserveActiveStateUsageAndRelease(t *testing.T) {
	db := newQuotaTestDB(t)
	account := addAccount(t, db, "key", 10)
	service := testService(db, time.Unix(100, 0))
	root := t.TempDir()
	base := models.RotationQuotaBatch{TaskID: 99, QuotaAccountID: account.ID, State: models.BatchStateSucceeded, OwnerToken: "history", RequestKey: "history", DestinationRemote: "history"}
	if err := db.Create(&base).Error; err != nil {
		t.Fatal(err)
	}
	file := models.RotationQuotaBatchFile{BatchID: base.ID, RelativePath: "old", SnapshotKey: "old"}
	if err := db.Create(&file).Error; err != nil {
		t.Fatal(err)
	}
	expired := time.Unix(1, 0)
	if err := db.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchID: base.ID, BatchFileID: file.ID, Bytes: 10, State: "active", ExpiresAt: &expired, IdempotencyKey: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	result, err := service.Reserve(PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 10), Snapshots: []LocalSnapshot{snapshot("pending", 1)}, RequestIdempotencyKey: "state", SourceRoot: root, DestinationPath: "/dest"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Batches) != 0 || len(result.Pending) != 1 {
		t.Fatalf("active usage result = %#v", result)
	}

	var committed models.QuotaReservation
	if err := db.First(&committed, "idempotency_key = ?", "active").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&committed).Update("state", "released").Error; err != nil {
		t.Fatal(err)
	}
	result, err = service.Reserve(PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 10), Snapshots: []LocalSnapshot{snapshot("pending", 1)}, RequestIdempotencyKey: "state-2", SourceRoot: root, DestinationPath: "/dest-2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Batches) != 1 {
		t.Fatalf("released usage result = %#v", result)
	}

	if err := service.ReleaseHeldBatch(result.Batches[0].ID); err != nil {
		t.Fatal(err)
	}
	result, err = service.Reserve(PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 10), Snapshots: []LocalSnapshot{snapshot("next", 1)}, RequestIdempotencyKey: "state-3", SourceRoot: root, DestinationPath: "/dest-3"})
	if err != nil || len(result.Batches) != 1 {
		t.Fatalf("release did not free capacity: result=%#v err=%v", result, err)
	}
}

func TestStateAwareUsage(t *testing.T) {
	db := newQuotaTestDB(t)
	account := addAccount(t, db, "key", 10)
	expired := time.Unix(1, 0)
	for i, state := range []string{"active", "unknown", "held", "committed"} {
		batch := models.RotationQuotaBatch{TaskID: 900 + uint(i), QuotaAccountID: account.ID, State: models.BatchStateSucceeded, OwnerToken: "state-owner-" + string(rune('a'+i)), RequestKey: "state-key-" + string(rune('a'+i)), DestinationRemote: "state-remote-" + string(rune('a'+i))}
		if err := db.Create(&batch).Error; err != nil {
			t.Fatal(err)
		}
		file := models.RotationQuotaBatchFile{BatchID: batch.ID, RelativePath: "state-" + string(rune('a'+i)), SnapshotKey: "state-" + string(rune('a'+i))}
		if err := db.Create(&file).Error; err != nil {
			t.Fatal(err)
		}
		var expiresAt *time.Time
		if state != models.ReservationStateCommitted {
			expiresAt = &expired
		}
		if err := db.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchID: batch.ID, BatchFileID: file.ID, Bytes: 3, State: state, ExpiresAt: expiresAt, IdempotencyKey: "state-reservation-" + string(rune('a'+i))}).Error; err != nil {
			t.Fatal(err)
		}
	}

	service := testService(db, time.Unix(100, 0))
	request := PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 10), Snapshots: []LocalSnapshot{snapshot("pending", 2)}, RequestIdempotencyKey: "state-aware", SourceRoot: t.TempDir(), DestinationPath: "/state"}
	result, err := service.Reserve(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Batches) != 0 || len(result.Pending) != 1 {
		t.Fatalf("state-aware reservations did not count: %#v", result)
	}
	if err := db.Model(&models.QuotaReservation{}).Where("state IN ?", []string{"active", "unknown", "held"}).Updates(map[string]interface{}{"state": "released"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.QuotaReservation{}).Where("state = ?", models.ReservationStateCommitted).Update("expires_at", expired).Error; err != nil {
		t.Fatal(err)
	}
	result, err = service.Reserve(PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 10), Snapshots: []LocalSnapshot{snapshot("next", 2)}, RequestIdempotencyKey: "state-aware-2", SourceRoot: t.TempDir(), DestinationPath: "/state-2"})
	if err != nil || len(result.Batches) != 1 {
		t.Fatalf("released and expired committed usage did not free budget: result=%#v err=%v", result, err)
	}
}

func TestHeldReservationsCarryForwardAtFixedWindowBoundary(t *testing.T) {
	db := newQuotaTestDB(t)
	account := addAccount(t, db, "key", 10)
	expired := time.Unix(1, 0)
	batch := models.RotationQuotaBatch{TaskID: 500, QuotaAccountID: account.ID, State: "reserved", OwnerToken: "cleanup-owner", RequestKey: "cleanup-key", DestinationRemote: "remote"}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		file := models.RotationQuotaBatchFile{BatchID: batch.ID, RelativePath: fmt.Sprintf("cleanup-%d", i), SnapshotKey: fmt.Sprintf("cleanup-%d", i)}
		if err := db.Create(&file).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchID: batch.ID, BatchFileID: file.ID, Bytes: 5, State: "held", ExpiresAt: &expired, IdempotencyKey: fmt.Sprintf("cleanup-reservation-%d", i)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	service := testService(db, time.Unix(100, 0))
	result, err := service.Reserve(PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 10), Snapshots: []LocalSnapshot{snapshot("next", 10)}, RequestIdempotencyKey: "cleanup-next", SourceRoot: t.TempDir(), DestinationPath: "/next"})
	if err != nil || len(result.Batches) != 0 || len(result.Pending) != 1 {
		t.Fatalf("held batch was incorrectly released: result=%#v err=%v", result, err)
	}
	var oldReservations []models.QuotaReservation
	if err := db.Where("batch_id = ?", batch.ID).Find(&oldReservations).Error; err != nil {
		t.Fatal(err)
	}
	if len(oldReservations) != 2 {
		t.Fatalf("old reservation count = %d", len(oldReservations))
	}
	for _, reservation := range oldReservations {
		if reservation.State != models.ReservationStateHeld || reservation.ExpiresAt == nil || !reservation.ExpiresAt.Equal(time.Unix(100, 0).Add(time.Duration(models.DefaultQuotaWindowSeconds)*time.Second)) {
			t.Fatalf("old reservation was not carried forward: %#v", reservation)
		}
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateReserved {
		t.Fatalf("old batch state = %q", stored.State)
	}
}

func TestInitializedWindowNormalizesLegacyHeldExpiry(t *testing.T) {
	db := newQuotaTestDB(t)
	anchor := time.Unix(100, 0)
	account := models.QuotaAccount{QuotaKey: "initialized", RemoteName: "remote", BudgetBytes: models.DefaultRotationQuotaLimitBytes, WindowSeconds: models.DefaultQuotaWindowSeconds, Enabled: true, WindowStartedAt: &anchor}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	legacyExpiry := time.Unix(1, 0)
	if err := db.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchFileID: 1, Bytes: 5, State: models.ReservationStateHeld, IdempotencyKey: "legacy-held-expiry", ExpiresAt: &legacyExpiry}).Error; err != nil {
		t.Fatal(err)
	}
	advanced, err := AdvanceAccountWindow(db, account.ID, time.Unix(200, 0))
	if err != nil {
		t.Fatal(err)
	}
	if advanced.WindowStartedAt == nil || !advanced.WindowStartedAt.Equal(anchor) {
		t.Fatalf("initialized anchor changed: %#v", advanced)
	}
	var reservation models.QuotaReservation
	if err := db.First(&reservation, "idempotency_key = ?", "legacy-held-expiry").Error; err != nil {
		t.Fatal(err)
	}
	want := anchor.Add(time.Duration(models.DefaultQuotaWindowSeconds) * time.Second)
	if reservation.State != models.ReservationStateHeld || reservation.ExpiresAt == nil || !reservation.ExpiresAt.Equal(want) {
		t.Fatalf("legacy held reservation deadline = %#v, want %v", reservation, want)
	}
}

func TestMixedHeldActiveBatchIsNotCleaned(t *testing.T) {
	db := newQuotaTestDB(t)
	account := addAccount(t, db, "key", 10)
	expired := time.Unix(1, 0)
	batch := models.RotationQuotaBatch{TaskID: 700, QuotaAccountID: account.ID, State: models.BatchStateReserved, OwnerToken: "mixed-owner", RequestKey: "mixed-key", DestinationRemote: "remote"}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	for i, state := range []string{models.ReservationStateHeld, models.ReservationStateActive} {
		file := models.RotationQuotaBatchFile{BatchID: batch.ID, RelativePath: fmt.Sprintf("mixed-%d", i), SnapshotKey: fmt.Sprintf("mixed-%d", i)}
		if err := db.Create(&file).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchID: batch.ID, BatchFileID: file.ID, Bytes: 5, State: state, ExpiresAt: &expired, IdempotencyKey: fmt.Sprintf("mixed-reservation-%d", i)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	service := testService(db, time.Unix(100, 0))
	result, err := service.Reserve(PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 10), Snapshots: []LocalSnapshot{snapshot("next", 1)}, RequestIdempotencyKey: "mixed-next", SourceRoot: t.TempDir(), DestinationPath: "/mixed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Batches) != 0 || len(result.Pending) != 1 {
		t.Fatalf("mixed batch was released: %#v", result)
	}
	var stored models.RotationQuotaBatch
	if err := db.First(&stored, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.BatchStateReserved {
		t.Fatalf("mixed batch state = %q", stored.State)
	}
	if err := service.ReleaseHeldBatch(batch.ID); err == nil {
		t.Fatal("mixed held/active batch was released")
	}
}

func TestServiceCorruptReservationFailsClosed(t *testing.T) {
	db := newQuotaTestDB(t)
	account := addAccount(t, db, "key", 10)
	batch := models.RotationQuotaBatch{TaskID: 701, QuotaAccountID: account.ID, State: models.BatchStateSucceeded, OwnerToken: "corrupt-owner", RequestKey: "corrupt-key", DestinationRemote: "remote"}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	file := models.RotationQuotaBatchFile{BatchID: batch.ID, RelativePath: "corrupt", SnapshotKey: "corrupt"}
	if err := db.Create(&file).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("PRAGMA ignore_check_constraints = ON").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO quota_reservations (quota_account_id,batch_id,batch_file_id,bytes,state,idempotency_key) VALUES (?,?,?,?,?,?)", account.ID, batch.ID, file.ID, -1, "invalid", "corrupt-reservation").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("PRAGMA ignore_check_constraints = OFF").Error; err != nil {
		t.Fatal(err)
	}
	service := testService(db, time.Unix(100, 0))
	if _, err := service.Reserve(PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 10), Snapshots: []LocalSnapshot{snapshot("next", 1)}, RequestIdempotencyKey: "corrupt-next", SourceRoot: t.TempDir(), DestinationPath: "/corrupt"}); err == nil {
		t.Fatal("corrupt reservation was ignored")
	}
}

func TestReserveConcurrentRequestsOneActiveBatch(t *testing.T) {
	db := newQuotaTestDB(t)
	addAccount(t, db, "key", 20)
	service := testService(db, time.Unix(100, 0))
	request := func(key string) error {
		_, err := service.Reserve(PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 20), Snapshots: []LocalSnapshot{snapshot(key, 1)}, RequestIdempotencyKey: key, SourceRoot: t.TempDir(), DestinationPath: "/same"})
		return err
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = request("concurrent-" + string(rune('a'+i))) }(i)
	}
	wg.Wait()
	var success, active int
	for _, err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrActiveBatch) {
			active++
		}
	}
	if success != 1 || active != 1 {
		t.Fatalf("concurrent results success=%d active=%d errors=%v", success, active, errs)
	}
}

func TestSharedQuotaKeyCapacityAcrossTasks(t *testing.T) {
	db := newQuotaTestDB(t)
	addAccount(t, db, "shared-key", 10)
	service := testService(db, time.Unix(100, 0))
	firstTask := quotaTask([]string{"remote-a"}, map[string]string{"remote-a": "shared-key"}, 10)
	firstTask.ID = 801
	if _, err := service.Reserve(PackReserveRequest{Task: firstTask, Snapshots: []LocalSnapshot{snapshot("first", 7)}, RequestIdempotencyKey: "shared-first", SourceRoot: t.TempDir(), DestinationPath: "/first"}); err != nil {
		t.Fatal(err)
	}
	secondTask := quotaTask([]string{"remote-b"}, map[string]string{"remote-b": "shared-key"}, 10)
	secondTask.ID = 802
	result, err := service.Reserve(PackReserveRequest{Task: secondTask, Snapshots: []LocalSnapshot{snapshot("second", 4)}, RequestIdempotencyKey: "shared-second", SourceRoot: t.TempDir(), DestinationPath: "/second"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Batches) != 0 || len(result.Pending) != 1 {
		t.Fatalf("shared quota capacity was not enforced: %#v", result)
	}
}

func TestConcurrentSharedKeyDifferentTasksAndDestinationsRespectBudget(t *testing.T) {
	db := newQuotaTestDB(t)
	addAccount(t, db, "shared-key", 10)
	service := testService(db, time.Unix(100, 0))
	var wg sync.WaitGroup
	results := make(chan PackReserveResult, 2)
	errs := make(chan error, 2)
	for i, destination := range []string{"/concurrent-a", "/concurrent-b"} {
		wg.Add(1)
		go func(i int, destination string) {
			defer wg.Done()
			task := quotaTask([]string{fmt.Sprintf("remote-%d", i)}, map[string]string{fmt.Sprintf("remote-%d", i): "shared-key"}, 10)
			task.ID = uint(850 + i)
			result, err := service.Reserve(PackReserveRequest{Task: task, Snapshots: []LocalSnapshot{snapshot(fmt.Sprintf("file-%d", i), 7)}, RequestIdempotencyKey: fmt.Sprintf("concurrent-shared-%d", i), SourceRoot: t.TempDir(), DestinationPath: destination})
			results <- result
			errs <- err
		}(i, destination)
	}
	wg.Wait()
	close(results)
	close(errs)
	var batches, pending int
	for result := range results {
		batches += len(result.Batches)
		pending += len(result.Pending)
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent shared-key reservation error: %v", err)
		}
	}
	if batches != 1 || pending != 1 {
		t.Fatalf("concurrent shared-key result batches=%d pending=%d", batches, pending)
	}
	var reserved int64
	if err := db.Model(&models.QuotaReservation{}).Where("state = ?", models.ReservationStateHeld).Select("COALESCE(SUM(bytes),0)").Scan(&reserved).Error; err != nil {
		t.Fatal(err)
	}
	if reserved > models.DefaultRotationQuotaLimitBytes {
		t.Fatalf("shared-key reservations exceeded budget: %d", reserved)
	}
}

func TestReconcileAccountWindowAnchorIsFixedAndNeverCleared(t *testing.T) {
	db := newQuotaTestDB(t)
	account := addAccount(t, db, "key", 100)
	task := quotaTask([]string{"remote"}, map[string]string{"remote": "key"}, 100)
	service := testService(db, time.Unix(100, 0))

	// The first reservation initializes the fixed account window.
	if _, err := service.Reserve(PackReserveRequest{Task: task, Snapshots: []LocalSnapshot{snapshot("a", 40), snapshot("b", 60)}, SourceRoot: "/source", DestinationPath: "/dest"}); err != nil {
		t.Fatal(err)
	}
	var current models.QuotaAccount
	if err := db.First(&current, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.WindowStartedAt == nil || !current.WindowStartedAt.Equal(time.Unix(100, 0)) {
		t.Fatalf("window anchor = %v", current.WindowStartedAt)
	}

	// Releasing usage must not clear or move the fixed anchor.
	now := time.Unix(200, 0)
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.QuotaReservation{}).Where("quota_account_id = ?", account.ID).Update("state", models.ReservationStateReleased).Error; err != nil {
			return err
		}
		return ReconcileAccountWindowAnchor(tx, account.ID, now)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&current, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.WindowStartedAt == nil || !current.WindowStartedAt.Equal(time.Unix(100, 0)) {
		t.Fatalf("anchor changed after release: %v", current.WindowStartedAt)
	}

	// Idempotency: calling reconcile again must keep the original anchor.
	if err := db.Transaction(func(tx *gorm.DB) error {
		return ReconcileAccountWindowAnchor(tx, account.ID, time.Unix(300, 0))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&current, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !current.WindowStartedAt.Equal(time.Unix(100, 0)) {
		t.Fatalf("anchor drifted on idempotent reconcile: %v", current.WindowStartedAt)
	}
}

func TestReconcileAccountWindowAnchorRefillPreservesAnchor(t *testing.T) {
	db := newQuotaTestDB(t)
	account := addAccount(t, db, "key", 100)
	now := time.Unix(200, 0)

	// Anchor the account at `now` via the simplest possible flow.
	if err := db.Transaction(func(tx *gorm.DB) error {
		// No reservations yet, so the first reconcile sets the anchor.
		return ReconcileAccountWindowAnchor(tx, account.ID, now)
	}); err != nil {
		t.Fatal(err)
	}
	var current models.QuotaAccount
	if err := db.First(&current, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.WindowStartedAt == nil {
		t.Fatal("expected anchor set after first reconcile")
	}
	// Adding a reservation does not move the fixed window.
	if err := db.Transaction(func(tx *gorm.DB) error {
		expires := now.Add(3600 * time.Second)
		if err := tx.Create(&models.RotationQuotaBatch{TaskID: 999, QuotaAccountID: current.ID, DestinationScope: "scope", State: models.BatchStateReserved, RequestKey: "inline-refill", OwnerToken: "o", ReservedBytes: 30}).Error; err != nil {
			return err
		}
		if err := tx.Create(&models.QuotaReservation{QuotaAccountID: current.ID, Bytes: 30, State: models.ReservationStateHeld, IdempotencyKey: "k", ReservedAt: &now, ExpiresAt: &expires}).Error; err != nil {
			return err
		}
		if err := ReconcileAccountWindowAnchor(tx, current.ID, now); err != nil {
			return err
		}
		// GORM First reuses the same struct; use a fresh struct here.
		var fresh models.QuotaAccount
		if err := tx.Raw("SELECT * FROM quota_accounts WHERE id = ?", current.ID).Scan(&fresh).Error; err != nil {
			return err
		}
		current = fresh
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if current.WindowStartedAt == nil || !current.WindowStartedAt.Equal(now) {
		t.Fatalf("anchor changed on refill: %v", current.WindowStartedAt)
	}
}

type mutableClock struct {
	value time.Time
}

func (c *mutableClock) now() time.Time          { return c.value }
func (c *mutableClock) advance(d time.Duration) { c.value = c.value.Add(d) }

func newClockedTestService(db *gorm.DB, clock *mutableClock) *Service {
	configFile, err := os.CreateTemp("", "clocked-rclone-*.conf")
	if err != nil {
		panic(err)
	}
	if _, err := configFile.WriteString("[test]\n"); err != nil {
		panic(err)
	}
	if err := configFile.Close(); err != nil {
		panic(err)
	}
	configPath := configFile.Name()
	return &Service{
		DB:             db,
		Now:            clock.now,
		ConfigResolver: func(string) (string, error) { return configPath, nil },
	}
}

func TestFixedWindowRolloverExpiresCommittedAndCarriesUnresolved(t *testing.T) {
	db := newQuotaTestDB(t)
	start := time.Unix(100, 0)
	account := models.QuotaAccount{QuotaKey: "window", BudgetBytes: 10, WindowSeconds: 3600, Enabled: true, WindowStartedAt: &start}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	oldExpiry := start.Add(time.Hour)
	rows := []models.QuotaReservation{
		{QuotaAccountID: account.ID, BatchFileID: 1, Bytes: 2, State: models.ReservationStateCommitted, IdempotencyKey: "committed", ExpiresAt: &oldExpiry},
		{QuotaAccountID: account.ID, BatchFileID: 2, Bytes: 3, State: models.ReservationStateHeld, IdempotencyKey: "held", ExpiresAt: &oldExpiry},
		{QuotaAccountID: account.ID, BatchFileID: 3, Bytes: 4, State: models.ReservationStateUnknown, IdempotencyKey: "unknown", ExpiresAt: &oldExpiry},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	exact := start.Add(24 * time.Hour)
	advanced, err := AdvanceAccountWindow(db, account.ID, exact)
	if err != nil {
		t.Fatal(err)
	}
	if advanced.WindowStartedAt == nil || !advanced.WindowStartedAt.Equal(exact) {
		t.Fatalf("exact rollover anchor = %v", advanced.WindowStartedAt)
	}
	wantEnd := exact.Add(24 * time.Hour)
	var stored []models.QuotaReservation
	if err := db.Where("quota_account_id = ?", account.ID).Order("id").Find(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored[0].State != models.ReservationStateExpired {
		t.Fatalf("committed reservation state = %q", stored[0].State)
	}
	for _, reservation := range stored[1:] {
		if reservation.State != models.ReservationStateHeld && reservation.State != models.ReservationStateUnknown {
			t.Fatalf("unresolved reservation state = %q", reservation.State)
		}
		if reservation.ExpiresAt == nil || !reservation.ExpiresAt.Equal(wantEnd) {
			t.Fatalf("unresolved reservation expiry = %v, want %v", reservation.ExpiresAt, wantEnd)
		}
	}

	late := exact.Add(49*time.Hour + 30*time.Minute)
	advanced, err = AdvanceAccountWindow(db, account.ID, late)
	if err != nil {
		t.Fatal(err)
	}
	wantAnchor := exact.Add(48 * time.Hour)
	if advanced.WindowStartedAt == nil || !advanced.WindowStartedAt.Equal(wantAnchor) {
		t.Fatalf("late rollover anchor = %v, want %v", advanced.WindowStartedAt, wantAnchor)
	}
	if !quotaWindowEndEqual(advanced, wantAnchor.Add(24*time.Hour)) {
		t.Fatalf("late rollover end = %v", AccountWindowEnd(advanced))
	}
}

func quotaWindowEndEqual(account models.QuotaAccount, want time.Time) bool {
	return AccountWindowEnd(account).Equal(want)
}

func TestReserveUsesOneSharedReservationDeadline(t *testing.T) {
	db := newQuotaTestDB(t)
	account := addAccount(t, db, "shared-window", 20)
	clock := time.Unix(100, 0)
	service := testService(db, clock)
	result, err := service.Reserve(PackReserveRequest{
		Task:      quotaTask([]string{"remote"}, map[string]string{"remote": account.QuotaKey}, 20),
		Snapshots: []LocalSnapshot{snapshot("a", 3), snapshot("b", 4)}, RequestIdempotencyKey: "shared-deadline",
		SourceRoot: t.TempDir(), DestinationPath: "/shared-window",
	})
	if err != nil {
		t.Fatal(err)
	}
	var reservations []models.QuotaReservation
	if err := db.Where("batch_id = ?", result.Batches[0].ID).Order("id").Find(&reservations).Error; err != nil {
		t.Fatal(err)
	}
	want := clock.Add(24 * time.Hour)
	for _, reservation := range reservations {
		if reservation.ExpiresAt == nil || !reservation.ExpiresAt.Equal(want) {
			t.Fatalf("reservation deadline = %v, want %v", reservation.ExpiresAt, want)
		}
	}
}

func TestReserveAt700GiBWaitsForRolloverThenReserves(t *testing.T) {
	db := newQuotaTestDB(t)
	start := time.Unix(100, 0)
	account := models.QuotaAccount{QuotaKey: "full", RemoteName: "remote", BudgetBytes: models.DefaultRotationQuotaLimitBytes, WindowSeconds: models.DefaultQuotaWindowSeconds, Enabled: true, WindowStartedAt: &start}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	legacyExpiry := start.Add(time.Hour)
	if err := db.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchFileID: 99, Bytes: models.DefaultRotationQuotaLimitBytes, State: models.ReservationStateCommitted, IdempotencyKey: "full-window", ExpiresAt: &legacyExpiry}).Error; err != nil {
		t.Fatal(err)
	}
	beforeBoundary := start.Add(24*time.Hour - time.Second)
	request := PackReserveRequest{Task: quotaTask([]string{"remote"}, map[string]string{"remote": account.QuotaKey}, models.DefaultRotationQuotaLimitBytes), Snapshots: []LocalSnapshot{snapshot("next", 1)}, RequestIdempotencyKey: "before-boundary", SourceRoot: "/source", DestinationPath: "/before"}
	if result, err := func() (PackReserveResult, error) {
		service := testService(db, beforeBoundary)
		return service.Reserve(request)
	}(); err != nil {
		t.Fatal(err)
	} else if len(result.Batches) != 0 || len(result.Pending) != 1 {
		t.Fatalf("reservation crossed live boundary: %#v", result)
	}

	boundary := start.Add(24 * time.Hour)
	request.RequestIdempotencyKey = "at-boundary"
	request.DestinationPath = "/at-boundary"
	result, err := func() (PackReserveResult, error) {
		service := testService(db, boundary)
		return service.Reserve(request)
	}()
	if err != nil || len(result.Batches) != 1 || len(result.Pending) != 0 {
		t.Fatalf("reservation did not become available at rollover: result=%#v err=%v", result, err)
	}
	var old models.QuotaReservation
	if err := db.Where("id = ?", 1).First(&old).Error; err != nil {
		t.Fatal(err)
	}
	if old.State != models.ReservationStateExpired {
		t.Fatalf("old full-window reservation state = %q", old.State)
	}
	var held int64
	if err := db.Model(&models.QuotaReservation{}).Where("state IN ?", []string{models.ReservationStateHeld, models.ReservationStateActive, models.ReservationStateUnknown}).Select("COALESCE(SUM(bytes), 0)").Scan(&held).Error; err != nil {
		t.Fatal(err)
	}
	if held > models.DefaultRotationQuotaLimitBytes {
		t.Fatalf("live reservations exceeded fixed budget: %d", held)
	}
}

func TestInitializeAccountWindowsRetiresHistoricalProbeRows(t *testing.T) {
	db := newQuotaTestDB(t)
	if err := db.AutoMigrate(&models.QuotaProbeAttempt{}); err != nil {
		t.Fatal(err)
	}
	account := addAccount(t, db, "historical-probe", 10)
	due := time.Unix(200, 0)
	attempt := models.QuotaProbeAttempt{
		QuotaAccountID: account.ID, RecoveryGeneration: 0, ScheduledSlot: 0,
		AttemptKey: models.QuotaProbeAttemptKey(account.ID, 0, 0), ContractVersion: models.ProbeContractVersion,
		Phase: models.ProbePhaseClaimed, ObjectPath: ".historical-probe", ExpectedBytes: models.ProbeExpectedBytes,
		QuotaKey: account.QuotaKey, ConfigIdentity: "/config", RemoteName: "remote", DueAt: due,
	}
	if err := db.Create(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if err := InitializeAccountWindows(db, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	var stored models.QuotaProbeAttempt
	if err := db.First(&stored, attempt.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != models.ProbeAttemptStateCanceled || stored.Phase != models.ProbePhaseFinished {
		t.Fatalf("historical probe row remained executable: %#v", stored)
	}
	var current models.QuotaAccount
	if err := db.First(&current, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.NextProbeAt != nil || current.ProbeClaimToken != "" || current.ProbeClaimUntil != nil {
		t.Fatalf("probe scheduling state remained active: %#v", current)
	}
}
