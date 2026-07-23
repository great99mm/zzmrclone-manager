package api

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
)

func TestMaintenanceReasonBackfillAndStrictAudit(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "maintenance.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.DestinationScopeMaintenance{DestinationScope: "scope", Epoch: 1, OwnerTaskID: 1, FirstRemote: "remote", RemoteDir: "/dest", ResolvedConfigPath: "/config", ResolvedConfigIdentity: "/config", State: models.MaintenanceStateClosed, DedupeState: models.DedupeStateSucceeded, Reason: ""}).Error; err != nil {
		t.Fatal(err)
	}
	if err := validateMaintenanceMigration(db); err != nil {
		t.Fatal(err)
	}
	var row models.DestinationScopeMaintenance
	if err := db.First(&row).Error; err != nil || row.Reason != models.MaintenanceReasonQuotaExhaustion {
		t.Fatalf("backfilled reason = %q err=%v", row.Reason, err)
	}
	if err := db.Exec("PRAGMA ignore_check_constraints = ON").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&row).Update("reason", "unexpected").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("PRAGMA ignore_check_constraints = OFF").Error; err != nil {
		t.Fatal(err)
	}
	if err := validateMaintenanceMigration(db); err == nil {
		t.Fatal("unknown maintenance reason passed audit")
	}
}

func TestLegacyAutoDedupeMigrationForcesFalse(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tasks.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.Task{}); err != nil {
		t.Fatal(err)
	}
	task := models.Task{Name: "legacy", AutoDedupe: true}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := disableLegacyAutoDedupe(db); err != nil {
		t.Fatal(err)
	}
	var stored models.Task
	if err := db.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.AutoDedupe {
		t.Fatal("legacy auto_dedupe value was not forced false")
	}
}
