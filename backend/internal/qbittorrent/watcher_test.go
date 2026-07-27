package qbittorrent

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/rclone"
)

func TestRunNextRejectsConvertedManualTaskBeforeLegacyExecutor(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "qb-manual.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	task := models.Task{Name: "qb", SourceType: "local", SourceDir: t.TempDir(), DestType: "remote", RemoteName: "remote", RemoteDir: "/dest", TransferMode: models.TransferModeCopy, TaskType: "normal", QBEnabled: true, Enabled: true}
	if err := database.AutoMigrate(&models.Task{}); err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	executor := rclone.NewExecutor(nil, nil)
	watcher := NewWatcher(executor, database)
	watcher.enqueue(task.ID, queuedTorrent{Hash: "hash", Name: "torrent", SourcePath: task.SourceDir})
	if err := database.Model(&models.Task{}).Where("id = ?", task.ID).Updates(map[string]interface{}{"task_type": models.TaskTypeManual, "manual_strategy": models.ManualStrategyAllocation, "qb_enabled": false}).Error; err != nil {
		t.Fatal(err)
	}
	watcher.runNext(&task)
	if executor.IsRunning(task.ID) {
		t.Fatal("converted manual task reached legacy executor")
	}
	if _, ok := watcher.popQueue(task.ID); !ok {
		t.Fatal("watcher unexpectedly consumed the queued legacy item")
	}
}
