package api

import (
	"rclone-manager/internal/models"
	"rclone-manager/internal/proactive"
	"testing"
)

func TestValidateProactiveQuotaContract(t *testing.T) {
	base := models.Task{TaskType: "rotation", RotationStrategy: "proactive_quota", SourceType: "local", SourceDir: "/src", DestType: "remote", RemoteName: "drive", RemoteDir: "dst", TransferMode: models.TransferModeCopy, RotationRemotes: "[\"drive\"]", RotationQuotaLimitBytes: models.DefaultRotationQuotaLimitBytes}
	if err := validateAndNormalizeTask(&base); err != nil {
		t.Fatalf("valid proactive task rejected: %v", err)
	}
	for name, mutate := range map[string]func(*models.Task){"remote source": func(x *models.Task) { x.SourceType = "remote" }, "move": func(x *models.Task) { x.TransferMode = "move" }, "sync": func(x *models.Task) { x.TransferMode = "sync" }, "no remotes": func(x *models.Task) { x.RotationRemotes = "[]" }, "budget": func(x *models.Task) { x.RotationQuotaLimitBytes = models.DefaultRotationQuotaLimitBytes + 1 }, "batch files": func(x *models.Task) { x.RotationBatchFiles = 129 }, "negative batch files": func(x *models.Task) { x.RotationBatchFiles = -1 }} {
		candidate := base
		mutate(&candidate)
		if err := validateAndNormalizeTask(&candidate); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestProactiveValidationPreservesQBAndHonorsMoveGate(t *testing.T) {
	previousDB, previousCfg := db, cfgGlobal
	db = proactiveStatusTestDB(t)
	cfgGlobal = nil
	defer func() { db, cfgGlobal = previousDB, previousCfg }()
	if proactiveMoveEnabled() {
		t.Fatal("missing proactive move setting unexpectedly enabled the feature")
	}

	task := models.Task{TaskType: "rotation", RotationStrategy: "proactive_quota", SourceType: "local", SourceDir: "/src", DestType: "remote", RemoteName: "drive", RemoteDir: "dst", TransferMode: models.TransferModeCopy, RotationRemotes: `["drive"]`, QBEnabled: true, QBURL: "http://qb", QBUsername: "user", QBPassword: "secret", QBPollInterval: 30, QBDeleteFiles: true, RotationQuotaLimitBytes: models.DefaultRotationQuotaLimitBytes}
	if err := validateAndNormalizeTask(&task); err != nil {
		t.Fatal(err)
	}
	if !task.QBEnabled || task.QBURL != "http://qb" || task.QBUsername != "user" || task.QBPassword != "secret" || task.QBPollInterval != 30 || !task.QBDeleteFiles {
		t.Fatalf("proactive qB config was not preserved: %#v", task)
	}

	task.TransferMode = models.TransferModeMove
	if err := validateAndNormalizeTask(&task); err == nil {
		t.Fatal("move accepted while feature gate is disabled")
	}
	if err := db.Create(&models.SystemSetting{Key: models.ProactiveMoveSettingKey, Value: "true"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := validateAndNormalizeTask(&task); err != nil {
		t.Fatalf("move rejected with enabled gate: %v", err)
	}
	task.TransferMode = "sync"
	if err := validateAndNormalizeTask(&task); err == nil {
		t.Fatal("sync accepted for proactive task")
	}
}

func TestLegacyRotationQBRemainsRejected(t *testing.T) {
	task := models.Task{TaskType: "rotation", RotationStrategy: "legacy_error", QBEnabled: true}
	if err := validateAndNormalizeTask(&task); err == nil {
		t.Fatal("legacy rotation qB was accepted")
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

func TestValidateProactiveQuotaPreservesZeroCapacity(t *testing.T) {
	task := models.Task{TaskType: "rotation", RotationStrategy: "proactive_quota", SourceType: "local", SourceDir: "/src", DestType: "remote", RemoteName: "drive", RemoteDir: "dst", TransferMode: models.TransferModeCopy, RotationRemotes: `["drive"]`, RotationQuotaLimitBytes: 0}
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
