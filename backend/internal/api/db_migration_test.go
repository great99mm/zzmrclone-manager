package api

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
)

const migrationValidOwner = "0123456789abcdef0123456789abcdef0123456789abcdef"

func TestRotationQuotaSchemaMigration(t *testing.T) {
	if err := InitDB(t.TempDir()); err != nil {
		t.Fatalf("init temporary database: %v", err)
	}
	stopMaintenance := maintenanceStop
	t.Cleanup(func() {
		if stopMaintenance != nil {
			close(stopMaintenance)
			if maintenanceStop == stopMaintenance {
				maintenanceStop = nil
			}
		}
		if db != nil {
			if sqlDB, err := db.DB(); err == nil {
				_ = sqlDB.Close()
			}
			db = nil
		}
	})

	for _, model := range []interface{}{
		&models.Task{},
		&models.QuotaAccount{},
		&models.RotationQuotaDirectoryAssignment{},
		&models.RotationQuotaBatch{},
		&models.RotationQuotaBatchFile{},
		&models.QuotaReservation{},
	} {
		if !db.Migrator().HasTable(model) {
			t.Fatalf("migrated table for %T is missing", model)
		}
	}
	for _, column := range []string{"rotation_quota_limit_bytes", "rotation_quota_keys", "rotation_rescan_pending", "rotation_last_scan_at", "rotation_quota_wake_at"} {
		if !db.Migrator().HasColumn(&models.Task{}, column) {
			t.Fatalf("task column %q is missing", column)
		}
	}
	for _, column := range []string{"request_key", "request_fingerprint", "rclone_config_path", "source_root_device", "source_root_inode", "transfer_mode", "completion_evidence", "completion_evidence_version", "move_handoff_contract_version", "move_quarantine_path", "move_quarantine_device", "move_quarantine_inode", "destination_scope_version", "lease_token", "process_start_token", "marker_detected_at"} {
		if !db.Migrator().HasColumn(&models.RotationQuotaBatch{}, column) {
			t.Fatalf("batch column %q is missing", column)
		}
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get temporary sql database: %v", err)
	}
	var synchronous int
	if err := sqlDB.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatalf("read SQLite synchronous pragma: %v", err)
	}
	if synchronous != 2 {
		t.Fatalf("SQLite synchronous = %d, want FULL (2)", synchronous)
	}

	assertMigrationIndex(t, &models.RotationQuotaBatchFile{}, "uq_rotation_quota_batch_files_batch_snapshot")
	assertMigrationIndex(t, &models.RotationQuotaBatch{}, "uq_rotation_quota_batches_request_identity")
	assertMigrationIndex(t, &models.QuotaReservation{}, "idx_quota_reservations_account_state_expiry")
	assertMigrationIndex(t, &models.QuotaReservation{}, "idx_quota_reservations_batch_state")
	assertMigrationIndex(t, &models.QuotaReservation{}, "idx_quota_reservations_batch_file")

	account := models.QuotaAccount{QuotaKey: "quota-key-1", RemoteName: "remote-1"}
	if err := db.Create(&account).Error; err != nil {
		t.Fatalf("create quota account: %v", err)
	}
	if account.BudgetBytes != models.DefaultRotationQuotaLimitBytes || !account.Enabled || account.WindowSeconds != 86400 {
		t.Fatalf("quota account defaults = budget %d enabled %t window %d", account.BudgetBytes, account.Enabled, account.WindowSeconds)
	}
	if err := db.Create(&models.QuotaAccount{QuotaKey: "quota-key-1"}).Error; err == nil {
		t.Fatal("duplicate quota key was accepted")
	}
	if err := db.Create(&models.QuotaAccount{}).Error; err == nil {
		t.Fatal("blank quota key was accepted")
	}
	if err := db.Create(&models.QuotaAccount{QuotaKey: "negative-budget", BudgetBytes: -1}).Error; err == nil {
		t.Fatal("negative quota budget was accepted")
	}

	batch1 := models.RotationQuotaBatch{OwnerToken: "owner-1", QuotaAccountID: account.ID, TaskID: 1, RequestKey: "request-1", DestinationRemote: "remote-1", RcloneConfigPath: "/tmp/rclone.conf", State: models.BatchStateSucceeded}
	batch2 := models.RotationQuotaBatch{OwnerToken: "owner-2", QuotaAccountID: account.ID, TaskID: 1, RequestKey: "request-2", DestinationRemote: "remote-1", RcloneConfigPath: "/tmp/rclone.conf", State: models.BatchStateSucceeded}
	if err := db.Create(&batch1).Error; err != nil {
		t.Fatalf("create first batch: %v", err)
	}
	if err := db.Create(&batch2).Error; err != nil {
		t.Fatalf("create second batch: %v", err)
	}
	if err := db.Create(&models.RotationQuotaBatch{OwnerToken: "owner-1", TaskID: 2, RequestKey: "request-3", DestinationRemote: "remote-2", RcloneConfigPath: "/tmp/rclone.conf", State: models.BatchStateSucceeded}).Error; err == nil {
		t.Fatal("duplicate batch owner token was accepted")
	}
	if err := db.Create(&models.RotationQuotaBatch{OwnerToken: "owner-3", TaskID: 1, RequestKey: "request-1", DestinationRemote: "remote-1", RcloneConfigPath: "/tmp/rclone.conf", State: models.BatchStateSucceeded}).Error; err == nil {
		t.Fatal("duplicate request identity was accepted")
	}
	if err := db.Create(&models.RotationQuotaBatch{OwnerToken: "owner-negative", TaskID: 3, RequestKey: "request-4", DestinationRemote: "remote-3", ReservedBytes: -1, RcloneConfigPath: "/tmp/rclone.conf", State: models.BatchStateSucceeded}).Error; err == nil {
		t.Fatal("negative batch reserved bytes were accepted")
	}

	file1 := models.RotationQuotaBatchFile{BatchID: batch1.ID, RelativePath: "one", SnapshotKey: "snapshot-1"}
	if err := db.Create(&file1).Error; err != nil {
		t.Fatalf("create first batch file: %v", err)
	}
	if err := db.Create(&models.RotationQuotaBatchFile{BatchID: batch1.ID, RelativePath: "two", SnapshotKey: "snapshot-1"}).Error; err == nil {
		t.Fatal("duplicate snapshot within a batch was accepted")
	}
	if err := db.Create(&models.RotationQuotaBatchFile{BatchID: batch1.ID, RelativePath: "one", SnapshotKey: "snapshot-2"}).Error; err == nil {
		t.Fatal("duplicate relative path within a batch was accepted")
	}
	file2 := models.RotationQuotaBatchFile{BatchID: batch2.ID, RelativePath: "two", SnapshotKey: "snapshot-1"}
	if err := db.Create(&file2).Error; err != nil {
		t.Fatalf("same snapshot in a different batch was rejected: %v", err)
	}
	if err := db.Create(&models.RotationQuotaBatchFile{BatchID: batch2.ID, RelativePath: "negative", SnapshotKey: "negative", SizeBytes: -1}).Error; err == nil {
		t.Fatal("negative batch file size was accepted")
	}
	if err := db.Create(&models.RotationQuotaBatchFile{BatchID: batch2.ID, RelativePath: "invalid-state", SnapshotKey: "invalid-state", State: "invalid"}).Error; err == nil {
		t.Fatal("invalid batch-file state was accepted")
	}

	if err := db.Create(&models.QuotaReservation{BatchFileID: file1.ID, IdempotencyKey: "reservation-1", State: models.ReservationStateHeld}).Error; err != nil {
		t.Fatalf("create first reservation: %v", err)
	}
	if err := db.Create(&models.QuotaReservation{BatchFileID: file1.ID, IdempotencyKey: "reservation-2", State: models.ReservationStateHeld}).Error; err == nil {
		t.Fatal("duplicate reservation batch-file id was accepted")
	}
	if err := db.Create(&models.QuotaReservation{BatchFileID: file2.ID, IdempotencyKey: "reservation-1", State: models.ReservationStateHeld}).Error; err == nil {
		t.Fatal("duplicate reservation idempotency key was accepted")
	}
	if err := db.Create(&models.QuotaReservation{BatchFileID: 999, IdempotencyKey: "reservation-invalid-state", State: "invalid"}).Error; err == nil {
		t.Fatal("invalid reservation state was accepted")
	}
	if err := db.Create(&models.QuotaReservation{BatchFileID: 1000, IdempotencyKey: "reservation-negative-bytes", State: models.ReservationStateHeld, Bytes: -1}).Error; err == nil {
		t.Fatal("negative reservation bytes were accepted")
	}
}

type oldQuotaBatch struct {
	ID                uint `gorm:"primaryKey"`
	TaskID            uint
	DestinationRemote string
	RequestKey        string
	OwnerToken        string
	State             string
}

func (oldQuotaBatch) TableName() string { return "rotation_quota_batches" }

type oldQuotaReservation struct {
	ID             uint `gorm:"primaryKey"`
	QuotaAccountID uint
	BatchID        uint
	BatchFileID    uint
	Bytes          int64
	State          string
	IdempotencyKey string
}

func (oldQuotaReservation) TableName() string { return "quota_reservations" }

func openMigrationTestDB(dir string) (*gorm.DB, error) {
	dsn := filepath.Join(dir, "rclone-manager.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)"
	return gorm.Open(sqlite.Open(dsn), &gorm.Config{})
}

func expectLegacyInitFailure(t *testing.T, setup func(*gorm.DB) error) {
	t.Helper()
	dir := t.TempDir()
	legacyDB, err := openMigrationTestDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacyDB.AutoMigrate(&oldQuotaBatch{}, &oldQuotaReservation{}); err != nil {
		t.Fatal(err)
	}
	if err := setup(legacyDB); err != nil {
		t.Fatal(err)
	}
	legacySQL, _ := legacyDB.DB()
	_ = legacySQL.Close()
	err = InitDB(dir)
	if err == nil {
		t.Fatal("legacy ledger corruption was accepted during startup")
	}
	if db != nil {
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
		db = nil
	}
}

func TestRotationQuotaLegacyActiveMissingPinFails(t *testing.T) {
	expectLegacyInitFailure(t, func(database *gorm.DB) error {
		return database.Create(&oldQuotaBatch{TaskID: 12, DestinationRemote: "legacy", OwnerToken: "active-owner", State: models.BatchStateReserved}).Error
	})
}

func TestRotationQuotaLegacyInvalidStateFails(t *testing.T) {
	expectLegacyInitFailure(t, func(database *gorm.DB) error {
		return database.Create(&oldQuotaBatch{TaskID: 13, DestinationRemote: "legacy", OwnerToken: "bad-state", State: "history"}).Error
	})
}

func TestRotationQuotaLegacyNegativeReservationFails(t *testing.T) {
	expectLegacyInitFailure(t, func(database *gorm.DB) error {
		return database.Create(&oldQuotaReservation{BatchID: 1, BatchFileID: 1, Bytes: -1, State: models.ReservationStateHeld, IdempotencyKey: "negative"}).Error
	})
}

func TestRotationQuotaSchemaMigratesOldBatchShape(t *testing.T) {
	dir := t.TempDir()
	legacyDB, err := openMigrationTestDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacyDB.AutoMigrate(&oldQuotaBatch{}); err != nil {
		t.Fatal(err)
	}
	if err := legacyDB.Create(&oldQuotaBatch{TaskID: 11, DestinationRemote: "legacy", OwnerToken: "legacy-owner", State: models.BatchStateSucceeded}).Error; err != nil {
		t.Fatal(err)
	}
	legacySQL, _ := legacyDB.DB()
	_ = legacySQL.Close()
	if err := InitDB(dir); err != nil {
		t.Fatalf("migrate old quota shape: %v", err)
	}
	stopMaintenance := maintenanceStop
	t.Cleanup(func() {
		if stopMaintenance != nil {
			close(stopMaintenance)
			maintenanceStop = nil
		}
		if db != nil {
			if sqlDB, err := db.DB(); err == nil {
				_ = sqlDB.Close()
			}
			db = nil
		}
	})
	for _, column := range []string{"request_fingerprint", "rclone_config_path"} {
		if !db.Migrator().HasColumn(&models.RotationQuotaBatch{}, column) {
			t.Fatalf("migrated old batch column %q is missing", column)
		}
	}
	var preserved models.RotationQuotaBatch
	if err := db.Where("owner_token = ?", "legacy-owner").First(&preserved).Error; err != nil {
		t.Fatalf("legacy terminal batch was not preserved: %v", err)
	}
	if preserved.State != models.BatchStateSucceeded {
		t.Fatalf("legacy terminal state changed to %q", preserved.State)
	}
}

func TestRotationQuotaMigrationAllowsRuntimeConfigDrift(t *testing.T) {
	dir := t.TempDir()
	legacyDB, err := openMigrationTestDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacyDB.AutoMigrate(&models.QuotaAccount{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &models.QuotaReservation{}); err != nil {
		t.Fatal(err)
	}
	if err := legacyDB.Create(&models.RotationQuotaBatch{
		TaskID: 21, QuotaAccountID: 0, DestinationRemote: "remote", RequestKey: "runtime-drift",
		RequestFingerprint: "fingerprint", RcloneConfigPath: "/missing/rclone.conf",
		SourceRootDevice: 1, SourceRootInode: 2, TransferMode: models.TransferModeCopy, DestinationScopeVersion: 1,
		DestinationScope: models.DestinationScope("/missing/rclone.conf", "/dest"), DestinationPath: "/dest",
		State: models.BatchStateReserved, OwnerToken: "0123456789abcdef0123456789abcdef0123456789abcdef",
	}).Error; err != nil {
		t.Fatal(err)
	}
	legacySQL, _ := legacyDB.DB()
	_ = legacySQL.Close()
	if err := InitDB(dir); err != nil {
		t.Fatalf("runtime config drift blocked migration: %v", err)
	}
	stopMaintenance := maintenanceStop
	t.Cleanup(func() {
		if stopMaintenance != nil {
			close(stopMaintenance)
			maintenanceStop = nil
		}
		if db != nil {
			if sqlDB, err := db.DB(); err == nil {
				_ = sqlDB.Close()
			}
			db = nil
		}
	})
}

func expectModernNonterminalAuditFailure(t *testing.T, mutate func(*models.RotationQuotaBatch)) {
	t.Helper()
	dir := t.TempDir()
	legacyDB, err := openMigrationTestDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacyDB.AutoMigrate(&models.QuotaAccount{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &models.QuotaReservation{}); err != nil {
		t.Fatal(err)
	}
	batch := models.RotationQuotaBatch{
		TaskID: 31, DestinationRemote: "remote", RcloneConfigPath: "/missing/config.conf",
		RequestKey: "audit-key", RequestFingerprint: "audit-fingerprint", SourceRootDevice: 1, SourceRootInode: 2,
		TransferMode: models.TransferModeCopy, DestinationScopeVersion: 1, DestinationPath: "/dest",
		DestinationScope: models.DestinationScope("/missing/config.conf", "/dest"), State: models.BatchStateReserved, OwnerToken: migrationValidOwner,
	}
	mutate(&batch)
	if err := legacyDB.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	legacySQL, _ := legacyDB.DB()
	_ = legacySQL.Close()
	if err := InitDB(dir); err == nil {
		t.Fatal("invalid non-terminal batch audit was accepted")
	}
	if db != nil {
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
		db = nil
	}
}

func TestRotationQuotaAuditRejectsNonCopyBatch(t *testing.T) {
	expectModernNonterminalAuditFailure(t, func(batch *models.RotationQuotaBatch) {
		batch.TransferMode = models.TransferModeMove
		started := time.Unix(100, 0)
		batch.StartedAt = &started
		batch.State = models.BatchStateRunning
	})
}

func TestRotationQuotaAuditAcceptsValidMoveContract(t *testing.T) {
	database, err := openMigrationTestDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.QuotaAccount{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &models.QuotaReservation{}); err != nil {
		t.Fatal(err)
	}
	batch := models.RotationQuotaBatch{ID: 61, TaskID: 61, DestinationRemote: "remote", RcloneConfigPath: "/config", RequestKey: "move-contract", RequestFingerprint: "move-fingerprint", SourceRoot: "/source", SourceRootDevice: 1, SourceRootInode: 2, TransferMode: models.TransferModeMove, DestinationScopeVersion: 1, DestinationPath: "/dest", DestinationScope: models.DestinationScope("/config", "/dest"), State: models.BatchStatePlanned, OwnerToken: migrationValidOwner, MoveHandoffContractVersion: models.MoveHandoffVersion, MoveQuarantinePath: "/source/.rclone-manager-move/61-" + migrationValidOwner, MoveQuarantineDevice: 1, MoveQuarantineInode: 3}
	if err := database.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.RotationQuotaBatchFile{BatchID: batch.ID, RelativePath: "nested/file.bin", SnapshotKey: "move-snapshot", SizeBytes: 5, MtimeNS: 7, Device: 8, Inode: 9, State: models.BatchFileStateHeld, MoveHandoffState: models.MoveHandoffReady, MoveHandoffSize: 5, MoveHandoffMtimeNS: 7}).Error; err != nil {
		t.Fatal(err)
	}
	if err := validateQuotaLedgerMigration(database); err != nil {
		t.Fatalf("valid move contract rejected: %v", err)
	}
}

func TestRotationQuotaAuditAllowsMoveBeforeHandoffWithoutContract(t *testing.T) {
	database, err := openMigrationTestDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.QuotaAccount{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &models.QuotaReservation{}); err != nil {
		t.Fatal(err)
	}
	batch := models.RotationQuotaBatch{TaskID: 62, DestinationRemote: "remote", RcloneConfigPath: "/config", RequestKey: "pre-handoff", RequestFingerprint: "pre-handoff", SourceRoot: "/source", SourceRootDevice: 1, SourceRootInode: 2, TransferMode: models.TransferModeMove, DestinationScopeVersion: 1, DestinationPath: "/dest", DestinationScope: models.DestinationScope("/config", "/dest"), State: models.BatchStatePlanned, OwnerToken: migrationValidOwner}
	if err := database.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.RotationQuotaBatchFile{BatchID: batch.ID, RelativePath: "nested/file.bin", SnapshotKey: "pre-handoff-snapshot", SizeBytes: 5, MtimeNS: 7, Device: 8, Inode: 9, State: models.BatchFileStateHeld, MoveHandoffState: models.MoveHandoffReady, MoveHandoffSize: 5, MoveHandoffMtimeNS: 7}).Error; err != nil {
		t.Fatal(err)
	}
	if err := validateQuotaLedgerMigration(database); err != nil {
		t.Fatalf("pre-handoff move rejected: %v", err)
	}
}

func TestRotationQuotaAuditRequiresMoveContractAfterStart(t *testing.T) {
	database, err := openMigrationTestDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.QuotaAccount{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &models.QuotaReservation{}); err != nil {
		t.Fatal(err)
	}
	started := time.Unix(100, 0)
	batch := models.RotationQuotaBatch{TaskID: 63, DestinationRemote: "remote", RcloneConfigPath: "/config", RequestKey: "started-without-contract", RequestFingerprint: "started-without-contract", SourceRoot: "/source", SourceRootDevice: 1, SourceRootInode: 2, TransferMode: models.TransferModeMove, DestinationScopeVersion: 1, DestinationPath: "/dest", DestinationScope: models.DestinationScope("/config", "/dest"), State: models.BatchStateRunning, OwnerToken: migrationValidOwner, StartedAt: &started, ProcessID: 77, ProcessStartToken: "77:1"}
	if err := database.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	if err := validateQuotaLedgerMigration(database); err == nil {
		t.Fatal("started move without handoff contract was accepted")
	}
}

func TestRotationQuotaAuditRejectsWrongScopeVersion(t *testing.T) {
	expectModernNonterminalAuditFailure(t, func(batch *models.RotationQuotaBatch) { batch.DestinationScopeVersion = 2 })
}

func TestRotationQuotaAuditRejectsInvalidLeaseToken(t *testing.T) {
	expectModernNonterminalAuditFailure(t, func(batch *models.RotationQuotaBatch) { batch.LeaseToken = "../lease" })
}

func TestRotationQuotaAuditRejectsWrongScopeHash(t *testing.T) {
	expectModernNonterminalAuditFailure(t, func(batch *models.RotationQuotaBatch) { batch.DestinationScope = "remote-derived-scope" })
}

func TestRotationQuotaAuditRejectsNonterminalBatchFileState(t *testing.T) {
	for _, state := range []string{"", "invalid"} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			legacyDB, err := openMigrationTestDB(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := legacyDB.AutoMigrate(&models.QuotaAccount{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &models.QuotaReservation{}); err != nil {
				t.Fatal(err)
			}
			batch := models.RotationQuotaBatch{TaskID: 41, DestinationRemote: "remote", RcloneConfigPath: "/missing/config.conf", RequestKey: "file-state-key", RequestFingerprint: "file-state-fingerprint", SourceRootDevice: 1, SourceRootInode: 2, TransferMode: models.TransferModeCopy, DestinationScopeVersion: 1, DestinationPath: "/dest", DestinationScope: models.DestinationScope("/missing/config.conf", "/dest"), State: models.BatchStateReserved, OwnerToken: migrationValidOwner}
			if err := legacyDB.Create(&batch).Error; err != nil {
				t.Fatal(err)
			}
			if state == "invalid" {
				if err := legacyDB.Exec("PRAGMA ignore_check_constraints = ON").Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := legacyDB.Create(&models.RotationQuotaBatchFile{BatchID: batch.ID, RelativePath: "file", SnapshotKey: "file", State: state}).Error; err != nil {
				t.Fatal(err)
			}
			if state == "invalid" {
				_ = legacyDB.Exec("PRAGMA ignore_check_constraints = OFF")
			}
			legacySQL, _ := legacyDB.DB()
			_ = legacySQL.Close()
			if err := InitDB(dir); err == nil {
				t.Fatal("illegal non-terminal batch-file state was accepted")
			}
			if db != nil {
				if sqlDB, dbErr := db.DB(); dbErr == nil {
					_ = sqlDB.Close()
				}
				db = nil
			}
		})
	}
}

func TestRotationQuotaAuditRejectsNoncanonicalBatchFilePath(t *testing.T) {
	for _, relative := range []string{"", "/absolute", "../escape", "dir//file", "dir\\file", "dir/./file"} {
		t.Run(relative, func(t *testing.T) {
			dir := t.TempDir()
			legacyDB, err := openMigrationTestDB(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := legacyDB.AutoMigrate(&models.QuotaAccount{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &models.QuotaReservation{}); err != nil {
				t.Fatal(err)
			}
			batch := models.RotationQuotaBatch{TaskID: 51, DestinationRemote: "remote", RcloneConfigPath: "/missing/config.conf", RequestKey: "path-" + relative, RequestFingerprint: "fingerprint", SourceRootDevice: 1, SourceRootInode: 2, TransferMode: models.TransferModeCopy, DestinationScopeVersion: 1, DestinationPath: "/dest", DestinationScope: models.DestinationScope("/missing/config.conf", "/dest"), State: models.BatchStateReserved, OwnerToken: migrationValidOwner}
			if err := legacyDB.Create(&batch).Error; err != nil {
				t.Fatal(err)
			}
			if err := legacyDB.Create(&models.RotationQuotaBatchFile{BatchID: batch.ID, RelativePath: relative, SnapshotKey: "path-" + relative, State: models.BatchFileStateHeld}).Error; err != nil {
				t.Fatal(err)
			}
			legacySQL, _ := legacyDB.DB()
			_ = legacySQL.Close()
			if err := InitDB(dir); err == nil {
				t.Fatal("noncanonical batch-file path was accepted")
			}
			if db != nil {
				if sqlDB, dbErr := db.DB(); dbErr == nil {
					_ = sqlDB.Close()
				}
				db = nil
			}
		})
	}
}

func assertMigrationIndex(t *testing.T, model interface{}, name string) {
	t.Helper()
	if !db.Migrator().HasIndex(model, name) {
		t.Fatalf("index %q is missing on %T", name, model)
	}
}
