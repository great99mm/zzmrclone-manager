package api

import (
	"fmt"
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
	)
	if err != nil {
		return fmt.Errorf("failed to migrate database: %v", err)
	}

	// Create default admin if no users exist
	var count int64
	db.Model(&models.User{}).Count(&count)
	if count == 0 {
		// Hash the default password
		hashedPassword, err := bcrypt.GenerateFromPassword([]byte("admin123"), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("failed to hash default password: %v", err)
		}
		admin := &models.User{
			Username: "admin",
			Password: string(hashedPassword),
			IsAdmin:  true,
		}
		db.Create(admin)
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

// GetDB exposes the database instance for other packages (e.g. rclone).
func GetDB() *gorm.DB {
	return db
}
