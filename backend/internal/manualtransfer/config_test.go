package manualtransfer

import (
	"context"
	"errors"
	"os"
	"testing"

	"rclone-manager/internal/models"
)

func TestTaskAccountConfigurationIsCanonicalFencedAndFrozen(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	if err := os.WriteFile(root+"/file", []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	task := manualTask(t, database, root)
	if err := database.Model(&task).Updates(map[string]interface{}{"task_type": models.TaskTypeManual, "manual_strategy": models.ManualStrategyAllocation}).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(database)
	updated, err := service.UpdateTaskAccounts(context.Background(), UpdateTaskAccountsRequest{TaskID: task.ID, ExpectedRevision: 1, IdempotencyKey: "config-1", AccountIDs: []uint{2, 1}, ActorIdentity: "admin", ActorType: "admin_session"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Page.Revision != 2 || len(updated.Page.Accounts) != 2 || updated.Page.Accounts[0].AccountID != 2 || updated.Page.Accounts[1].AccountID != 1 {
		t.Fatalf("configured accounts = %#v", updated.Page)
	}
	replay, err := service.UpdateTaskAccounts(context.Background(), UpdateTaskAccountsRequest{TaskID: task.ID, ExpectedRevision: 1, IdempotencyKey: "config-1", AccountIDs: []uint{2, 1}, ActorIdentity: "admin", ActorType: "admin_session"})
	if err != nil || replay.Page.Revision != 2 {
		t.Fatalf("config replay = %#v, err=%v", replay, err)
	}
	if _, err := service.UpdateTaskAccounts(context.Background(), UpdateTaskAccountsRequest{TaskID: task.ID, ExpectedRevision: 2, IdempotencyKey: "config-1", AccountIDs: []uint{2, 1}, ActorIdentity: "admin", ActorType: "admin_session"}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("same key with changed expected revision error = %v", err)
	}
	if _, err := service.UpdateTaskAccounts(context.Background(), UpdateTaskAccountsRequest{TaskID: task.ID, ExpectedRevision: 1, IdempotencyKey: "config-1", AccountIDs: []uint{1, 2}, ActorIdentity: "admin", ActorType: "admin_session"}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed config replay error = %v", err)
	}
	if _, err := service.UpdateTaskAccounts(context.Background(), UpdateTaskAccountsRequest{TaskID: task.ID, ExpectedRevision: 1, IdempotencyKey: "config-2", AccountIDs: []uint{1}, ActorIdentity: "admin", ActorType: "admin_session"}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale config revision error = %v", err)
	}

	service.Start()
	defer service.Stop()
	analyze, err := service.CreateAnalyze(AnalyzeRequest{TaskID: task.ID, SourcePath: root, DestinationPath: "/dest", TransferMode: models.TransferModeCopy, IdempotencyKey: "freeze-1", ActorIdentity: "admin", ActorType: "admin_session"})
	if err != nil {
		t.Fatal(err)
	}
	waitManualRun(t, service, analyze.Run.ID, ManualRunStateAnalyzed)
	accounts, err := service.GetRunAccounts(analyze.Run.ID)
	if err != nil || len(accounts) != 2 || accounts[0].AccountID != 2 || accounts[1].AccountID != 1 {
		t.Fatalf("frozen accounts = %#v, err=%v", accounts, err)
	}
}

func TestTaskAccountConfigurationRejectsDisabledAndTechnicalOverflow(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	task := manualTask(t, database, root)
	if err := database.Model(&models.QuotaAccount{}).Where("id = ?", 2).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(database)
	if _, err := service.UpdateTaskAccounts(context.Background(), UpdateTaskAccountsRequest{TaskID: task.ID, ExpectedRevision: 1, IdempotencyKey: "disabled", AccountIDs: []uint{2}}); err == nil {
		t.Fatal("disabled account was accepted")
	}
	if _, err := service.UpdateTaskAccounts(context.Background(), UpdateTaskAccountsRequest{TaskID: task.ID, ExpectedRevision: 1, IdempotencyKey: "too-many", AccountIDs: make([]uint, ManualMaxAccountInputs+1)}); err == nil {
		t.Fatal("technical account bound was not enforced")
	}
}
