package api

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
)

var db *gorm.DB

func InitDB(dataDir string) error {
	os.MkdirAll(dataDir, 0755)

	dbPath := filepath.Join(dataDir, "rclone-manager.db")

	var err error
	db, err = gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("failed to connect database: %v", err)
	}

	// Auto migrate
	err = db.AutoMigrate(
		&models.Task{},
		&models.TaskLog{},
		&models.SystemSetting{},
		&models.User{},
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
