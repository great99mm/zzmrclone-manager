package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/quota"
)

var (
	db              *gorm.DB
	maintenanceStop chan struct{}
)

func InitDB(dataDir string) error {
	os.MkdirAll(dataDir, 0755)

	dbPath := filepath.Join(dataDir, "rclone-manager.db")

	// WAL mode + busy timeout + full sync for durable quota reservations.
	// _pragma=journal_mode(WAL)    : write-ahead logging allows readers to proceed while a write is in progress.
	// _pragma=busy_timeout(5000)   : wait up to 5s before returning "database is locked".
	// _pragma=synchronous(FULL)    : flushes quota reservation commits durably before returning.
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)"

	var err error
	db, err = gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("failed to connect database: %v", err)
	}

	// With WAL mode + busy_timeout we no longer need the extreme
	// MaxOpenConns=1 setting.  A small pool (4) allows concurrent reads
	// (dashboard, task list, logs) while writes are still serialized by
	// SQLite itself.  This eliminates the starvation caused by logWorker
	// monopolising the single connection.
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("failed to get underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(2)
	sqlDB.SetConnMaxLifetime(time.Hour)

	// Auto migrate
	err = db.AutoMigrate(
		&models.Task{},
		&models.TaskLog{},
		&models.SystemSetting{},
		&models.User{},
		&models.OutputLog{},
		&models.OpenlistConfig{},
		&models.MountConfig{},
		&models.QuotaAccount{},
		&models.RotationQuotaOversize{},
		&models.RotationQuotaBatch{},
		&models.RotationQuotaBatchFile{},
		&models.QuotaReservation{},
		&models.DestinationScopeMaintenance{},
		&models.DestinationScopeCoordinator{},
	)
	if err != nil {
		return fmt.Errorf("failed to migrate database: %v", err)
	}
	if err := validateQuotaLedgerMigration(db); err != nil {
		return fmt.Errorf("quota ledger migration audit failed: %v", err)
	}
	if err := disableLegacyAutoDedupe(db); err != nil {
		return fmt.Errorf("failed to disable legacy auto dedupe: %v", err)
	}
	if err := ensureMountConfigColumns(db); err != nil {
		return fmt.Errorf("failed to migrate mount config columns: %v", err)
	}
	if err := ensureProactiveMoveSetting(db); err != nil {
		return fmt.Errorf("failed to initialize proactive move setting: %v", err)
	}

	// Create default admin if no users exist
	var count int64
	db.Model(&models.User{}).Count(&count)
	if count == 0 {
		password := generateRandomPassword(12)
		hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("failed to hash default password: %v", err)
		}
		admin := &models.User{
			Username: "admin",
			Password: string(hashedPassword),
			IsAdmin:  true,
		}
		db.Create(admin)

		// Print prominently so the user can find it in docker logs
		banner := fmt.Sprintf("\n======================================================\n  INITIAL ADMIN PASSWORD\n  Username: admin\n  Password: %s\n  Change this password after first login!\n======================================================\n", password)
		fmt.Println(banner)
		log.Print(banner)

		// Also write to a dedicated file for easy discovery
		pwFile := filepath.Join(dataDir, "initial-password.txt")
		os.WriteFile(pwFile, []byte(fmt.Sprintf("Username: admin\nPassword: %s\n", password)), 0644)
	}

	// ---- periodic maintenance (goroutine) ----
	// SQLite WAL files grow unbounded over time.  A periodic checkpoint
	// truncates the WAL and keeps the DB file size predictable.
	// OutputLog records older than 30 days are also pruned — this is the
	// *structured DB table*, NOT the task_N.log files which are untouched.
	if maintenanceStop != nil {
		close(maintenanceStop)
	}
	maintenanceStop = make(chan struct{})
	stopMaintenance := maintenanceStop
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-stopMaintenance:
				return
			case <-ticker.C:
			}
			// WAL checkpoint: move WAL pages back into the main DB file
			if sqlDB != nil {
				sqlDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
			}
			// Prune old structured output logs (keep 30 days)
			cutoff := time.Now().AddDate(0, 0, -30)
			db.Where("date < ?", cutoff).Delete(&models.OutputLog{})
		}
	}()

	return nil
}

func disableLegacyAutoDedupe(database *gorm.DB) error {
	return database.Model(&models.Task{}).Where("auto_dedupe = ?", true).Update("auto_dedupe", false).Error
}

func validateQuotaLedgerMigration(database *gorm.DB) error {
	var batches []models.RotationQuotaBatch
	if err := database.Find(&batches).Error; err != nil {
		return err
	}
	for _, batch := range batches {
		if !models.IsKnownBatchState(batch.State) {
			return fmt.Errorf("batch %d has illegal state %q", batch.ID, batch.State)
		}
		if !models.IsTerminalBatchState(batch.State) {
			if !models.IsValidOwnerToken(batch.OwnerToken) {
				return fmt.Errorf("non-terminal batch %d has invalid owner token; manual reconciliation required", batch.ID)
			}
			if batch.LeaseToken != "" && !models.IsValidLeaseToken(batch.LeaseToken) {
				return fmt.Errorf("non-terminal batch %d has invalid lease token; manual reconciliation required", batch.ID)
			}
			if batch.TransferMode == models.TransferModeMove {
				if moveContractRequired(batch) {
					if err := validateMoveBatchContract(database, batch); err != nil {
						return err
					}
				} else if moveContractPresent(batch) {
					return fmt.Errorf("move batch %d has a partial handoff contract before handoff; manual reconciliation required", batch.ID)
				}
			} else if batch.TransferMode != "" && batch.TransferMode != models.TransferModeCopy {
				return fmt.Errorf("non-terminal batch %d lacks copy-only transfer/scope contract; manual reconciliation required", batch.ID)
			}
			if batch.DestinationScopeVersion != 1 {
				return fmt.Errorf("non-terminal batch %d lacks copy-only transfer/scope contract; manual reconciliation required", batch.ID)
			}
			if strings.TrimSpace(batch.RequestKey) == "" || strings.TrimSpace(batch.RequestFingerprint) == "" || batch.SourceRootDevice <= 0 || batch.SourceRootInode <= 0 {
				return fmt.Errorf("non-terminal batch %d lacks request identity; manual reconciliation required", batch.ID)
			}
			if err := validatePinnedConfigStructure(batch.RcloneConfigPath); err != nil {
				return fmt.Errorf("non-terminal batch %d has invalid pinned config: %w; manual reconciliation required", batch.ID, err)
			}
			if batch.DestinationScope != models.DestinationScope(batch.RcloneConfigPath, batch.DestinationPath) {
				return fmt.Errorf("non-terminal batch %d has an invalid destination scope; manual reconciliation required", batch.ID)
			}
			var files []models.RotationQuotaBatchFile
			if err := database.Where("batch_id = ?", batch.ID).Find(&files).Error; err != nil {
				return err
			}
			for _, file := range files {
				if err := quota.ValidateRelativePath(file.RelativePath); err != nil {
					return fmt.Errorf("non-terminal batch %d has invalid relative path: %w; manual reconciliation required", batch.ID, err)
				}
				if !models.IsKnownBatchFileState(file.State) || file.State == "" {
					return fmt.Errorf("non-terminal batch %d has illegal batch-file state %q; manual reconciliation required", batch.ID, file.State)
				}
			}
		}
	}
	for _, batch := range batches {
		if batch.TransferMode != "" && batch.TransferMode != models.TransferModeCopy && batch.TransferMode != models.TransferModeMove {
			return fmt.Errorf("batch %d has illegal transfer mode %q", batch.ID, batch.TransferMode)
		}
		if batch.CompletionEvidence != "" && batch.CompletionEvidence != models.CompletionEvidenceRemote && batch.CompletionEvidence != models.CompletionEvidenceLocal {
			return fmt.Errorf("batch %d has illegal completion evidence %q", batch.ID, batch.CompletionEvidence)
		}
		if batch.TransferMode == models.TransferModeMove {
			if moveContractRequired(batch) {
				if err := validateMoveBatchContract(database, batch); err != nil {
					return err
				}
			} else if moveContractPresent(batch) {
				return fmt.Errorf("move batch %d has a partial handoff contract before handoff; manual reconciliation required", batch.ID)
			}
		}
	}

	var reservations []models.QuotaReservation
	if err := database.Find(&reservations).Error; err != nil {
		return err
	}
	for _, reservation := range reservations {
		if !isKnownReservationState(reservation.State) {
			return fmt.Errorf("reservation %d has illegal state %q", reservation.ID, reservation.State)
		}
		if reservation.Bytes < 0 {
			return fmt.Errorf("reservation %d has negative bytes", reservation.ID)
		}
	}
	if err := validateMaintenanceMigration(database); err != nil {
		return err
	}
	return nil
}

func validateMaintenanceMigration(database *gorm.DB) error {
	if !database.Migrator().HasTable(&models.DestinationScopeMaintenance{}) {
		return nil
	}
	// Rows created before Phase6 had no reason column. Backfill those legacy
	// epochs before enforcing the audited enum for every persisted row.
	if err := database.Model(&models.DestinationScopeMaintenance{}).Where("reason IS NULL OR trim(reason) = ''").Update("reason", models.MaintenanceReasonQuotaExhaustion).Error; err != nil {
		return err
	}
	var rows []models.DestinationScopeMaintenance
	if err := database.Order("destination_scope, epoch").Find(&rows).Error; err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return nil
		}
		return err
	}
	last := map[string]int64{}
	for _, row := range rows {
		if row.DestinationScope == "" || row.Epoch <= 0 || row.OwnerTaskID == 0 || row.FirstRemote == "" || row.RemoteDir == "" || row.ResolvedConfigPath == "" || row.ResolvedConfigIdentity == "" {
			return fmt.Errorf("maintenance epoch %d has incomplete immutable scope contract; manual reconciliation required", row.ID)
		}
		if !models.IsKnownMaintenanceReason(row.Reason) {
			return fmt.Errorf("maintenance epoch %d has invalid reason %q; manual reconciliation required", row.ID, row.Reason)
		}
		if row.Revision <= 0 {
			return fmt.Errorf("maintenance epoch %d has invalid revision; manual reconciliation required", row.ID)
		}
		if row.Epoch <= last[row.DestinationScope] {
			return fmt.Errorf("maintenance scope %q has non-monotonic epoch", row.DestinationScope)
		}
		last[row.DestinationScope] = row.Epoch
		switch row.State {
		case models.MaintenanceStateExhausted, models.MaintenanceStateClosed:
		default:
			return fmt.Errorf("maintenance epoch %d has invalid state %q", row.ID, row.State)
		}
		switch row.DedupeState {
		case "", models.DedupeStatePending, models.DedupeStateClaimed, models.DedupeStateRunning, models.DedupeStateSucceeded, models.DedupeStateFailed, models.DedupeStateUnknown:
		default:
			return fmt.Errorf("maintenance epoch %d has invalid dedupe state %q", row.ID, row.DedupeState)
		}
		if (row.DedupeState == models.DedupeStateClaimed || row.DedupeState == models.DedupeStateRunning) && row.LeaseToken == "" {
			return fmt.Errorf("maintenance epoch %d lacks a lease", row.ID)
		}
	}
	return nil
}

func validateMoveBatchContract(database *gorm.DB, batch models.RotationQuotaBatch) error {
	if batch.MoveHandoffContractVersion != models.MoveHandoffVersion || batch.MoveQuarantineDevice <= 0 || batch.MoveQuarantineInode <= 0 {
		return fmt.Errorf("move batch %d lacks a valid handoff contract; manual reconciliation required", batch.ID)
	}
	if !filepath.IsAbs(batch.SourceRoot) || filepath.Clean(batch.SourceRoot) != batch.SourceRoot || !filepath.IsAbs(batch.MoveQuarantinePath) || filepath.Clean(batch.MoveQuarantinePath) != batch.MoveQuarantinePath {
		return fmt.Errorf("move batch %d has an invalid quarantine path; manual reconciliation required", batch.ID)
	}
	expected := filepath.Join(batch.SourceRoot, ".rclone-manager-move", fmt.Sprintf("%d-%s", batch.ID, batch.OwnerToken))
	if batch.MoveQuarantinePath != expected {
		return fmt.Errorf("move batch %d has an unexpected quarantine path; manual reconciliation required", batch.ID)
	}
	if !models.IsTerminalBatchState(batch.State) && batch.StartedAt != nil && batch.State != models.BatchStateUnknown && batch.ProcessID <= 0 {
		return fmt.Errorf("move batch %d lacks durable process identity; manual reconciliation required", batch.ID)
	}
	var files []models.RotationQuotaBatchFile
	if err := database.Where("batch_id = ?", batch.ID).Find(&files).Error; err != nil {
		return err
	}
	for _, file := range files {
		switch file.MoveHandoffState {
		case models.MoveHandoffReady:
		case models.MoveHandoffQuarantined, models.MoveHandoffRestored, models.MoveHandoffMoved, models.MoveHandoffUnknown:
			if file.MoveHandoffDevice <= 0 || file.MoveHandoffInode <= 0 {
				return fmt.Errorf("move batch %d file %d lacks handoff identity; manual reconciliation required", batch.ID, file.ID)
			}
		default:
			return fmt.Errorf("move batch %d file %d has invalid handoff state %q; manual reconciliation required", batch.ID, file.ID, file.MoveHandoffState)
		}
		if file.MoveHandoffSize != file.SizeBytes || file.MoveHandoffMtimeNS != file.MtimeNS {
			return fmt.Errorf("move batch %d file %d has invalid handoff metadata; manual reconciliation required", batch.ID, file.ID)
		}
	}
	return nil
}

func moveContractPresent(batch models.RotationQuotaBatch) bool {
	return batch.MoveHandoffContractVersion != 0 || batch.MoveQuarantinePath != "" || batch.MoveQuarantineDevice != 0 || batch.MoveQuarantineInode != 0
}

func moveContractRequired(batch models.RotationQuotaBatch) bool {
	if batch.StartedAt != nil || batch.State == models.BatchStateRunning || batch.State == models.BatchStateReconciling || batch.State == models.BatchStateUnknown {
		return true
	}
	return moveContractPresent(batch)
}

func ensureProactiveMoveSetting(database *gorm.DB) error {
	var setting models.SystemSetting
	result := database.Where("`key` = ?", models.ProactiveMoveSettingKey).First(&setting)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		setting = models.SystemSetting{Key: models.ProactiveMoveSettingKey, Value: "true"}
		if err := database.Create(&setting).Error; err != nil {
			return err
		}
	} else if result.Error != nil {
		return result.Error
	}
	var initialized models.SystemSetting
	marker := database.Where("`key` = ?", models.ProactiveMoveSettingMigrationKey).First(&initialized)
	if errors.Is(marker.Error, gorm.ErrRecordNotFound) {
		if setting.Value == "false" {
			if err := database.Model(&setting).Update("value", "true").Error; err != nil {
				return err
			}
		}
		return database.Create(&models.SystemSetting{Key: models.ProactiveMoveSettingMigrationKey, Value: "true"}).Error
	}
	return marker.Error
}

func isKnownReservationState(state string) bool {
	switch state {
	case models.ReservationStateHeld, models.ReservationStateActive, models.ReservationStateCommitted, models.ReservationStateUnknown, models.ReservationStateReleased, models.ReservationStateExpired:
		return true
	default:
		return false
	}
}

func validatePinnedConfigStructure(raw string) error {
	if strings.TrimSpace(raw) == "" || !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
		return fmt.Errorf("path must be absolute and clean")
	}
	return nil
}

func ensureMountConfigColumns(db *gorm.DB) error {
	columns := map[string]string{
		"name":            "text",
		"remote_name":     "text",
		"remote_path":     "text DEFAULT '/'",
		"mount_path":      "text",
		"rclone_config":   "text",
		"enabled":         "numeric DEFAULT 0",
		"allow_other":     "numeric DEFAULT 1",
		"read_only":       "numeric DEFAULT 0",
		"vfs_cache_mode":  "text DEFAULT 'writes'",
		"dir_cache_time":  "text DEFAULT '5m'",
		"poll_interval":   "text DEFAULT '1m'",
		"uid":             "integer DEFAULT 0",
		"gid":             "integer DEFAULT 0",
		"extra_args":      "text",
		"status":          "text DEFAULT 'stopped'",
		"last_error":      "text",
		"last_mounted_at": "datetime",
		"created_at":      "datetime",
		"updated_at":      "datetime",
		"deleted_at":      "datetime",
	}
	for name, definition := range columns {
		exists, err := sqliteColumnExists(db, "mount_configs", name)
		if err != nil {
			return err
		}
		if !exists {
			if err := db.Exec(fmt.Sprintf("ALTER TABLE mount_configs ADD COLUMN %s %s", name, definition)).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

type sqliteColumnInfo struct {
	Name string `gorm:"column:name"`
}

func sqliteColumnExists(db *gorm.DB, table string, column string) (bool, error) {
	var columns []sqliteColumnInfo
	if err := db.Raw(fmt.Sprintf("PRAGMA table_info(%s)", table)).Scan(&columns).Error; err != nil {
		return false, err
	}
	for _, info := range columns {
		if info.Name == column {
			return true, nil
		}
	}
	return false, nil
}

// generateRandomPassword creates a cryptographically random alphanumeric string of the given length.
func generateRandomPassword(length int) string {
	bytes := make([]byte, (length+1)/2) // hex encoding doubles the length
	if _, err := rand.Read(bytes); err != nil {
		// Fallback: this should never happen with a modern kernel
		panic(fmt.Sprintf("failed to generate random password: %v", err))
	}
	s := hex.EncodeToString(bytes)
	return s[:length]
}

// GetDB exposes the database instance for other packages (e.g. rclone).
func GetDB() *gorm.DB {
	return db
}

// ResetAdminPassword generates a new random password for the admin user,
// hashes it, updates the database, and returns the new plaintext password.
// Only call this from CLI tools (it prints to stdout).
func ResetAdminPassword(dataDir string) (string, error) {
	if err := InitDB(dataDir); err != nil {
		return "", fmt.Errorf("failed to open database: %v", err)
	}

	var user models.User
	if err := db.Where("username = ?", "admin").First(&user).Error; err != nil {
		return "", fmt.Errorf("admin user not found: %v", err)
	}

	newPassword := generateRandomPassword(12)
	hashed, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("failed to hash password: %v", err)
	}

	user.Password = string(hashed)
	db.Save(&user)

	banner := fmt.Sprintf("\n======================================================\n  ADMIN PASSWORD RESET\n  Username: admin\n  New password: %s\n======================================================\n", newPassword)
	fmt.Println(banner)
	log.Print(banner)

	// Also write to a dedicated file for easy discovery
	pwFile := filepath.Join(dataDir, "initial-password.txt")
	os.WriteFile(pwFile, []byte(fmt.Sprintf("Username: admin\nPassword: %s\n", newPassword)), 0644)

	return newPassword, nil
}
