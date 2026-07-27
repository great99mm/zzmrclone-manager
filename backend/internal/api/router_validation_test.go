package api

import (
	"rclone-manager/internal/models"
	"rclone-manager/internal/proactive"
	"testing"
)

func TestValidateProactiveQuotaStrategyIsDisabled(t *testing.T) {
	task := models.Task{TaskType: "rotation", RotationStrategy: "proactive_quota"}
	if err := validateAndNormalizeTask(&task); err == nil {
		t.Fatal("proactive_quota strategy was accepted")
	}
}

func TestProactiveQuotaStrategyIsRejectedBeforeTaskSpecificValidation(t *testing.T) {
	task := models.Task{TaskType: "rotation", RotationStrategy: "proactive_quota", TransferMode: "sync", QBEnabled: true}
	if err := validateAndNormalizeTask(&task); err == nil {
		t.Fatal("proactive_quota strategy was accepted")
	}
}

func TestLegacyRotationQBRemainsRejected(t *testing.T) {
	task := models.Task{TaskType: "rotation", RotationStrategy: "legacy_error", QBEnabled: true}
	if err := validateAndNormalizeTask(&task); err == nil {
		t.Fatal("legacy rotation qB was accepted")
	}
}

func TestManualValidationClearsAllLegacyRegistrationFlags(t *testing.T) {
	task := models.Task{TaskType: models.TaskTypeManual, ManualStrategy: models.ManualStrategyAllocation, SourceType: "local", SourceDir: "/src", DestType: "remote", RemoteDir: "/dest", TransferMode: models.TransferModeCopy, WatchEnabled: true, ScheduleEnabled: true, QBEnabled: true, AutoDedupe: true}
	if err := validateAndNormalizeTask(&task); err != nil {
		t.Fatal(err)
	}
	if task.WatchEnabled || task.ScheduleEnabled || task.QBEnabled || task.AutoDedupe {
		t.Fatalf("manual task retained legacy registration flags: %#v", task)
	}
}

func TestProactiveMoveGateDefaultsEnabledAndPreservesExplicitDisable(t *testing.T) {
	database := proactiveStatusTestDB(t)
	previous := db
	db = database
	defer func() { db = previous }()
	if err := ensureProactiveMoveSetting(database); err != nil {
		t.Fatal(err)
	}
	var setting models.SystemSetting
	if err := database.Where("`key` = ?", models.ProactiveMoveSettingKey).First(&setting).Error; err != nil {
		t.Fatal(err)
	}
	if setting.Value != "true" || !proactiveMoveEnabled() {
		t.Fatalf("unexpected default move gate: %#v", setting)
	}
	if err := database.Model(&setting).Update("value", "false").Error; err != nil {
		t.Fatal(err)
	}
	if proactiveMoveEnabled() {
		t.Fatal("explicit move gate disable was not preserved")
	}
}

func TestProactiveMoveGateUpgradesLegacyDefaultFalseOnce(t *testing.T) {
	database := proactiveStatusTestDB(t)
	previous := db
	db = database
	defer func() { db = previous }()
	if err := database.Create(&models.SystemSetting{Key: models.ProactiveMoveSettingKey, Value: "false"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := ensureProactiveMoveSetting(database); err != nil {
		t.Fatal(err)
	}
	var setting models.SystemSetting
	if err := database.Where("`key` = ?", models.ProactiveMoveSettingKey).First(&setting).Error; err != nil {
		t.Fatal(err)
	}
	if setting.Value != "true" {
		t.Fatalf("legacy default was not upgraded: %#v", setting)
	}
	if err := database.Model(&setting).Update("value", "false").Error; err != nil {
		t.Fatal(err)
	}
	if err := ensureProactiveMoveSetting(database); err != nil {
		t.Fatal(err)
	}
	if proactiveMoveEnabled() {
		t.Fatal("explicit post-migration disable was not preserved")
	}
}

func TestValidateLegacyRotationPreservesZeroCapacity(t *testing.T) {
	task := models.Task{TaskType: "rotation", RotationStrategy: "legacy_error", SourceType: "local", SourceDir: "/src", DestType: "remote", RemoteName: "drive", RemoteDir: "dst", TransferMode: models.TransferModeCopy, RotationRemotes: `["drive"]`, RotationQuotaLimitBytes: 0}
	if err := validateAndNormalizeTask(&task); err != nil {
		t.Fatal(err)
	}
	if task.RotationQuotaLimitBytes != 0 {
		t.Fatalf("zero quota was normalized to %d", task.RotationQuotaLimitBytes)
	}
}

func TestProactiveSourceCannotOverlapManagerRoot(t *testing.T) {
	for _, source := range []string{"/srv/manager", "/srv/manager/src", "/srv"} {
		if err := proactive.ValidateSourceOutsideManager(source, "/srv/manager"); err == nil {
			t.Fatalf("overlapping source accepted: %s", source)
		}
	}
	if err := proactive.ValidateSourceOutsideManager("/source", "/srv/manager"); err != nil {
		t.Fatalf("non-overlapping source rejected: %v", err)
	}
}
