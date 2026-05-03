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

	// Limit SQLite to a single connection. SQLite handles concurrency best
	// with one writer; multiple open connections from the pool cause lock
	// contention even with WAL. With MaxOpenConns=1 all DB operations are
	// naturally serialized without extra mutexes.
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("failed to get underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
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

	return nil
}

// GetDB exposes the database instance for other packages (e.g. rclone).
func GetDB() *gorm.DB {
	return db
}
