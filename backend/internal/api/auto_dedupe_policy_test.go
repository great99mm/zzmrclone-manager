package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"rclone-manager/internal/config"
	"rclone-manager/internal/models"
	"rclone-manager/internal/qbittorrent"
	"rclone-manager/internal/rclone"
	"rclone-manager/internal/scheduler"
	"rclone-manager/internal/taskdispatch"
	"rclone-manager/internal/watcher"
)

type autoDedupePolicyLegacyRunner struct{}

func (autoDedupePolicyLegacyRunner) ExecuteMove(*models.Task) error { return nil }
func (autoDedupePolicyLegacyRunner) IsRunning(uint) bool            { return false }
func (autoDedupePolicyLegacyRunner) StopTask(uint) error            { return nil }

func TestCreateTaskForcesAutoDedupeFalse(t *testing.T) {
	database := proactiveStatusTestDB(t)
	previousDB, previousConfig := db, cfgGlobal
	db, cfgGlobal = database, &config.Config{}
	defer func() { db, cfgGlobal = previousDB, previousConfig }()
	gin.SetMode(gin.TestMode)
	request := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(`{"name":"created","source_type":"local","source_dir":"/source","dest_type":"local","remote_dir":"/dest","transfer_mode":"copy","auto_dedupe":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	createTask(context)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var stored models.Task
	if err := database.Order("id DESC").First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.AutoDedupe {
		t.Fatal("create persisted auto_dedupe=true")
	}
}

func TestUpdateTaskForcesAutoDedupeFalse(t *testing.T) {
	database := proactiveStatusTestDB(t)
	task := models.Task{ID: 1, Name: "updated", SourceType: "local", SourceDir: "/source", DestType: "local", RemoteDir: "/dest", TransferMode: models.TransferModeCopy, TaskType: "normal", Enabled: true, AutoDedupe: true, WatchEnabled: false, ScheduleEnabled: false, QBEnabled: false}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	previousDB, previousConfig, previousExecutor, previousTaskRunner, previousScheduler, previousWatcher, previousQBWatcher := db, cfgGlobal, executor, taskRunner, sched, watch, qbWatch
	db, cfgGlobal = database, &config.Config{}
	executor = rclone.NewExecutor(nil, database)
	taskRunner = taskdispatch.New(database, autoDedupePolicyLegacyRunner{}, nil)
	sched = scheduler.NewScheduler(taskRunner)
	watch = watcher.NewWatcher(taskRunner)
	qbWatch = qbittorrent.NewWatcher(executor)
	defer func() {
		db, cfgGlobal, executor, taskRunner, sched, watch, qbWatch = previousDB, previousConfig, previousExecutor, previousTaskRunner, previousScheduler, previousWatcher, previousQBWatcher
	}()
	gin.SetMode(gin.TestMode)
	request := httptest.NewRequest(http.MethodPut, "/tasks/1", strings.NewReader(`{"name":"updated","source_type":"local","source_dir":"/source","dest_type":"local","remote_dir":"/dest","transfer_mode":"copy","task_type":"normal","watch_enabled":false,"schedule_enabled":false,"qb_enabled":false,"auto_dedupe":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	context.Params = gin.Params{{Key: "id", Value: "1"}}
	updateTaskUnsafe(context)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var stored models.Task
	if err := database.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.AutoDedupe {
		t.Fatal("update persisted auto_dedupe=true")
	}
}
