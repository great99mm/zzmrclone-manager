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

func TestCreateTaskRejectsRotationTaskType(t *testing.T) {
	database := proactiveStatusTestDB(t)
	previousDB, previousConfig := db, cfgGlobal
	db, cfgGlobal = database, &config.Config{}
	t.Cleanup(func() { db, cfgGlobal = previousDB, previousConfig })

	gin.SetMode(gin.TestMode)
	request := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(`{"name":"legacy rotation","task_type":"rotation","rotation_strategy":"legacy_error","source_type":"local","source_dir":"/source","dest_type":"remote","remote_name":"remote","remote_dir":"/dest","transfer_mode":"copy"}`))
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

func TestUpdateTaskRejectsRotationTaskTypeWithoutMutatingHistoricalTask(t *testing.T) {
	database := proactiveStatusTestDB(t)
	original := models.Task{ID: 1, Name: "historical rotation", TaskType: "rotation", RotationStrategy: "legacy_error", SourceType: "local", SourceDir: "/source", DestType: "remote", RemoteName: "remote", RemoteDir: "/dest", TransferMode: models.TransferModeCopy, Enabled: true}
	if err := database.Create(&original).Error; err != nil {
		t.Fatal(err)
	}
	previousDB, previousConfig, previousRunner := db, cfgGlobal, taskRunner
	db, cfgGlobal = database, &config.Config{}
	taskRunner = taskdispatch.New(database, &cutoverLegacyRunner{}, nil)
	t.Cleanup(func() { db, cfgGlobal, taskRunner = previousDB, previousConfig, previousRunner })

	gin.SetMode(gin.TestMode)
	request := httptest.NewRequest(http.MethodPut, "/tasks/1", strings.NewReader(`{"name":"changed","task_type":"rotation","rotation_strategy":"legacy_error","source_type":"local","source_dir":"/source","dest_type":"remote","remote_name":"remote","remote_dir":"/dest","transfer_mode":"copy","enabled":true}`))
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

func TestStartTaskRejectsHistoricalRotationTaskWithoutLaunchingIt(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.Create(&models.Task{ID: 1, Name: "historical rotation", TaskType: "rotation", RotationStrategy: "legacy_error", Status: "idle", Enabled: true}).Error; err != nil {
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
		t.Fatalf("historical rotation task reached generic runner: %d launches", runner.launches)
	}
}

func TestLegacyRotationActionsAreRejectedWithoutExecution(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.Create(&models.Task{ID: 1, Name: "historical rotation", TaskType: "rotation", RotationStrategy: "legacy_error", Status: "idle", Enabled: true, IsQuickTask: true}).Error; err != nil {
		t.Fatal(err)
	}
	previousDB := db
	db = database
	t.Cleanup(func() { db = previousDB })

	for _, testCase := range []struct {
		name    string
		method  string
		handler gin.HandlerFunc
	}{
		{name: "pause", method: http.MethodPost, handler: pauseTask},
		{name: "stop", method: http.MethodPost, handler: stopTask},
		{name: "cancel", method: http.MethodPost, handler: cancelTask},
		{name: "dedupe", method: http.MethodPost, handler: dedupeTask},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(testCase.method, "/tasks/1/"+testCase.name, nil)
			context.Params = gin.Params{{Key: "id", Value: "1"}}
			testCase.handler(context)
			if recorder.Code != http.StatusConflict {
				t.Fatalf("%s status=%d body=%s", testCase.name, recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestSetupRouterSkipsLegacyRotationRuntimeAndRoutes(t *testing.T) {
	dataDir := t.TempDir()
	previousDB, previousExecutor, previousDispatcher, previousRunner := db, executor, proactiveDispatcher, taskRunner
	previousWake, previousScheduler, previousWatcher, previousQBWatcher := wakeConsumer, sched, watch, qbWatch
	previousHub, previousConfig, previousMountManager, previousManualService := hub, cfgGlobal, mountMgr, manualTransferService

	if err := InitDB(dataDir); err != nil {
		t.Fatal(err)
	}
	rotationTask := models.Task{ID: 1, Name: "historical rotation", TaskType: "rotation", RotationStrategy: "legacy_error", Enabled: true, ScheduleEnabled: true, ScheduleInterval: 1, WatchEnabled: true, QBEnabled: true}
	normalTask := models.Task{ID: 2, Name: "normal task", TaskType: "normal", Enabled: true, ScheduleEnabled: true, ScheduleInterval: 1}
	if err := db.Create(&rotationTask).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&normalTask).Error; err != nil {
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
	if sched.GetNextRun(rotationTask.ID) != nil {
		t.Fatal("historical rotation task was registered with the scheduler")
	}
	if sched.GetNextRun(normalTask.ID) == nil {
		t.Fatal("normal task was not registered with the generic scheduler")
	}
	if qbWatch.Status(&rotationTask).Watching {
		t.Fatal("historical rotation task was registered with qBittorrent")
	}
	var storedRotation models.Task
	if err := db.First(&storedRotation, rotationTask.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedRotation.TaskType != "rotation" || storedRotation.RotationStrategy != "legacy_error" {
		t.Fatalf("historical rotation row was mutated: %#v", storedRotation)
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
