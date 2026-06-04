package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
)

var db *gorm.DB

func InitDB(dataDir string) error {
	os.MkdirAll(dataDir, 0755)

	dbPath := filepath.Join(dataDir, "rclone-manager.db")

	// WAL mode + busy timeout + normal sync for better concurrency.
	// _pragma=journal_mode(WAL)    : write-ahead logging allows readers to proceed while a write is in progress.
	// _pragma=busy_timeout(5000)   : wait up to 5s before returning "database is locked".
	// _pragma=synchronous(NORMAL)  : sufficient durability with WAL, much faster than FULL.
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"

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
		&models.WebhookJob{},
	)
	if err != nil {
		return fmt.Errorf("failed to migrate database: %v", err)
	}
	if err := ensureMountConfigColumns(db); err != nil {
		return fmt.Errorf("failed to migrate mount config columns: %v", err)
	}
	if err := ensureTaskColumns(db); err != nil {
		return fmt.Errorf("failed to migrate task columns: %v", err)
	}
	if err := ensureWebhookJobColumns(db); err != nil {
		return fmt.Errorf("failed to migrate webhook job columns: %v", err)
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
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
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

func ensureTaskColumns(db *gorm.DB) error {
	columns := map[string]string{
		"interface_log_enabled":    "numeric DEFAULT 0",
		"interface_log_url":        "text DEFAULT ''",
		"interface_log_token":      "text DEFAULT ''",
		"interface_log_interval":   "integer DEFAULT 30",
		"interface_log_match_path": "text DEFAULT ''",
		"interface_log_last_id":    "integer DEFAULT 0",
		"completion_action":        "text DEFAULT ''",
		"smartstrm_webhook_url":    "text DEFAULT ''",
		"smartstrm_task_name":      "text DEFAULT ''",
		"smartstrm_path_mapping":   "text DEFAULT ''",
		"smartstrm_delay":          "integer DEFAULT 0",
	}
	for name, definition := range columns {
		exists, err := sqliteColumnExists(db, "tasks", name)
		if err != nil {
			return err
		}
		if !exists {
			if err := db.Exec(fmt.Sprintf("ALTER TABLE tasks ADD COLUMN %s %s", name, definition)).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func ensureWebhookJobColumns(db *gorm.DB) error {
	columns := map[string]string{
		"remote_name":    "text DEFAULT ''",
		"tag":            "text DEFAULT ''",
		"smartstrm_path": "text DEFAULT ''",
		"smartstrm_task": "text DEFAULT ''",
	}
	for name, definition := range columns {
		exists, err := sqliteColumnExists(db, "webhook_jobs", name)
		if err != nil {
			return err
		}
		if !exists {
			if err := db.Exec(fmt.Sprintf("ALTER TABLE webhook_jobs ADD COLUMN %s %s", name, definition)).Error; err != nil {
				return err
			}
		}
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
