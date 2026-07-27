package rclone

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
)

type blockingLaunchFence struct {
	database *gorm.DB
	entered  chan struct{}
	release  chan struct{}
}

func (f *blockingLaunchFence) WithTaskLaunchExclusive(_ context.Context, taskID uint, fn func(*models.Task) error) error {
	close(f.entered)
	<-f.release
	var current models.Task
	if err := f.database.First(&current, taskID).Error; err != nil {
		return err
	}
	return fn(&current)
}

func TestExecuteMoveRejectsConversionDuringBlockedLaunch(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "launch-guard.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Task{}); err != nil {
		t.Fatal(err)
	}
	task := models.Task{Name: "launch", SourceType: "local", SourceDir: t.TempDir(), DestType: "remote", RemoteName: "remote", RemoteDir: "/dest", TransferMode: models.TransferModeCopy, TaskType: "normal", Enabled: true}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	fence := &blockingLaunchFence{database: database, entered: make(chan struct{}), release: make(chan struct{})}
	executor := NewExecutor(nil, nil)
	executor.LaunchFence = fence
	result := make(chan error, 1)
	go func() { result <- executor.ExecuteMove(&task) }()
	<-fence.entered
	if err := database.Model(&models.Task{}).Where("id = ?", task.ID).Updates(map[string]interface{}{"task_type": models.TaskTypeManual, "manual_strategy": models.ManualStrategyAllocation}).Error; err != nil {
		t.Fatal(err)
	}
	close(fence.release)
	if err := <-result; !errors.Is(err, ErrManualTaskLaunch) {
		t.Fatalf("launch after manual conversion error = %v", err)
	}
	if executor.IsRunning(task.ID) {
		t.Fatal("manual conversion left a legacy executor reservation")
	}
}
