package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"rclone-manager/internal/config"
	"rclone-manager/internal/models"
	"rclone-manager/internal/taskdispatch"
)

type cutoverLegacyRunner struct {
	launches int
}

func (r *cutoverLegacyRunner) ExecuteMove(*models.Task) error {
	r.launches++
	return nil
}

func (*cutoverLegacyRunner) IsRunning(uint) bool { return false }
func (*cutoverLegacyRunner) StopTask(uint) error { return nil }

func TestCreateTaskRejectsProactiveQuotaStrategy(t *testing.T) {
	database := proactiveStatusTestDB(t)
	previousDB, previousConfig := db, cfgGlobal
	db, cfgGlobal = database, &config.Config{}
	t.Cleanup(func() { db, cfgGlobal = previousDB, previousConfig })

	gin.SetMode(gin.TestMode)
	request := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(`{"name":"legacy proactive","task_type":"rotation","rotation_strategy":"proactive_quota","source_type":"local","source_dir":"/source","dest_type":"remote","remote_name":"remote","remote_dir":"/dest","transfer_mode":"copy","rotation_remotes":"[\"remote\"]"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	createTask(context)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("create status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var count int64
	if err := database.Model(&models.Task{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rejected proactive task was persisted: %d rows", count)
	}
}

func TestUpdateTaskRejectsProactiveQuotaStrategyWithoutMutatingHistoricalTask(t *testing.T) {
	database := proactiveStatusTestDB(t)
	original := models.Task{ID: 1, Name: "historical proactive", TaskType: "rotation", RotationStrategy: "proactive_quota", SourceType: "local", SourceDir: "/source", DestType: "remote", RemoteName: "remote", RemoteDir: "/dest", TransferMode: models.TransferModeCopy, RotationRemotes: `["remote"]`, Enabled: true}
	if err := database.Create(&original).Error; err != nil {
		t.Fatal(err)
	}
	previousDB, previousConfig, previousRunner := db, cfgGlobal, taskRunner
	db, cfgGlobal = database, &config.Config{}
	taskRunner = taskdispatch.New(database, &cutoverLegacyRunner{}, nil)
	t.Cleanup(func() { db, cfgGlobal, taskRunner = previousDB, previousConfig, previousRunner })

	gin.SetMode(gin.TestMode)
	request := httptest.NewRequest(http.MethodPut, "/tasks/1", strings.NewReader(`{"name":"changed","task_type":"rotation","rotation_strategy":"proactive_quota","source_type":"local","source_dir":"/source","dest_type":"remote","remote_name":"remote","remote_dir":"/dest","transfer_mode":"copy","rotation_remotes":"[\"remote\"]","enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	context.Params = gin.Params{{Key: "id", Value: "1"}}
	updateTaskUnsafe(context)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("update status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var stored models.Task
	if err := database.First(&stored, original.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Name != original.Name || stored.RotationStrategy != original.RotationStrategy {
		t.Fatalf("rejected proactive update mutated historical task: before=%#v after=%#v", original, stored)
	}
}

func TestStartTaskRejectsHistoricalProactiveTaskWithoutLaunchingIt(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.Create(&models.Task{ID: 1, Name: "historical proactive", TaskType: "rotation", RotationStrategy: "proactive_quota", Status: "idle", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	runner := &cutoverLegacyRunner{}
	previousDB, previousRunner, previousDispatcher := db, taskRunner, proactiveDispatcher
	db = database
	taskRunner = taskdispatch.New(database, runner, nil)
	proactiveDispatcher = nil
	t.Cleanup(func() { db, taskRunner, proactiveDispatcher = previousDB, previousRunner, previousDispatcher })

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/tasks/1/start", nil)
	context.Params = gin.Params{{Key: "id", Value: "1"}}
	startTask(context)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("start status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if runner.launches != 0 {
		t.Fatalf("historical proactive task reached generic runner: %d launches", runner.launches)
	}
}

func TestSetupRouterSkipsProactiveRuntimeAndRoutes(t *testing.T) {
	dataDir := t.TempDir()
	previousDB, previousExecutor, previousDispatcher, previousRunner := db, executor, proactiveDispatcher, taskRunner
	previousWake, previousScheduler, previousWatcher, previousQBWatcher := wakeConsumer, sched, watch, qbWatch
	previousHub, previousConfig, previousMountManager, previousManualService := hub, cfgGlobal, mountMgr, manualTransferService

	if err := InitDB(dataDir); err != nil {
		t.Fatal(err)
	}
	proactiveTask := models.Task{ID: 1, Name: "historical proactive", TaskType: "rotation", RotationStrategy: "proactive_quota", Enabled: true, ScheduleEnabled: true, ScheduleInterval: 1, WatchEnabled: true}
	legacyTask := models.Task{ID: 2, Name: "legacy rotation", TaskType: "rotation", RotationStrategy: "legacy_error", Enabled: true, ScheduleEnabled: true, ScheduleInterval: 1}
	if err := db.Create(&proactiveTask).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&legacyTask).Error; err != nil {
		t.Fatal(err)
	}

	setupConfig := &config.Config{DataDir: dataDir, LogDir: t.TempDir(), MountRoot: t.TempDir()}
	router := SetupRouter(setupConfig)
	t.Cleanup(func() {
		ShutdownBackgroundServices()
		db, executor, proactiveDispatcher, taskRunner = previousDB, previousExecutor, previousDispatcher, previousRunner
		wakeConsumer, sched, watch, qbWatch = previousWake, previousScheduler, previousWatcher, previousQBWatcher
		hub, cfgGlobal, mountMgr, manualTransferService = previousHub, previousConfig, previousMountManager, previousManualService
	})

	if proactiveDispatcher != nil || wakeConsumer != nil {
		t.Fatalf("proactive runtime was registered: dispatcher=%v wake=%v", proactiveDispatcher != nil, wakeConsumer != nil)
	}
	if sched.GetNextRun(proactiveTask.ID) != nil {
		t.Fatal("historical proactive task was registered with the scheduler")
	}
	if sched.GetNextRun(legacyTask.ID) == nil {
		t.Fatal("legacy rotation was not registered with the generic scheduler")
	}

	for _, disabledPath := range []string{
		"/api/tasks/:id/proactive-status",
		"/api/tasks/:id/proactive-resolutions",
		"/api/tasks/:id/proactive-manual-merge",
		"/api/tasks/:id/quota-accounts/:accountID/manual-reset",
		"/api/proactive-maintenance/:id/close-unknown",
	} {
		for _, route := range router.Routes() {
			if route.Path == disabledPath {
				t.Fatalf("disabled proactive route remains registered: %s %s", route.Method, route.Path)
			}
		}
	}
}
