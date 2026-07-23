package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"rclone-manager/internal/config"
	"rclone-manager/internal/models"
	"rclone-manager/internal/proactive"
	"rclone-manager/internal/qbittorrent"
	"rclone-manager/internal/quota"
	"rclone-manager/internal/rclone"
	"rclone-manager/internal/scheduler"
	"rclone-manager/internal/taskdispatch"
	"rclone-manager/internal/watcher"
)

type mutationRaceLegacyRunner struct{}

func (mutationRaceLegacyRunner) ExecuteMove(*models.Task) error { return nil }
func (mutationRaceLegacyRunner) IsRunning(uint) bool            { return false }
func (mutationRaceLegacyRunner) StopTask(uint) error            { return nil }

type manualMergeResponse struct {
	code int
	body string
}

func runManualMergeHandler(t *testing.T, taskID string) <-chan manualMergeResponse {
	t.Helper()
	result := make(chan manualMergeResponse, 1)
	go func() {
		gin.SetMode(gin.TestMode)
		request := httptest.NewRequest(http.MethodPost, "/tasks/"+taskID+"/proactive-manual-merge", nil)
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = request
		context.Params = gin.Params{{Key: "id", Value: taskID}}
		startProactiveManualMerge(context)
		result <- manualMergeResponse{code: recorder.Code, body: recorder.Body.String()}
	}()
	return result
}

func TestManualMaintenanceFencesTaskAndSharedScopeMutations(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.AutoMigrate(&models.DestinationScopeMaintenance{}); err != nil {
		t.Fatal(err)
	}
	configPath := t.TempDir() + "/rclone.conf"
	if err := os.WriteFile(configPath, []byte("[remote]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	task := models.Task{ID: 1, RcloneConfig: configPath, RemoteDir: "/shared", TaskType: "rotation", RotationStrategy: "proactive_quota"}
	shared := models.Task{ID: 2, RcloneConfig: configPath, RemoteDir: "/shared", TaskType: "rotation", RotationStrategy: "proactive_quota"}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&shared).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.DestinationScopeMaintenance{DestinationScope: models.DestinationScope(configPath, "/shared"), Epoch: 1, OwnerTaskID: task.ID, State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStatePending, Reason: models.MaintenanceReasonManualMerge, Revision: 1}).Error; err != nil {
		t.Fatal(err)
	}
	dispatcher := &proactive.Dispatcher{DB: database, ConfigResolver: func(string) (string, error) { return configPath, nil }}
	previousDB, previousDispatcher := db, proactiveDispatcher
	db, proactiveDispatcher = database, dispatcher
	defer func() { db, proactiveDispatcher = previousDB, previousDispatcher }()
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name    string
		handler gin.HandlerFunc
		method  string
		id      string
	}{
		{name: "update owner", handler: updateTaskUnsafe, method: http.MethodPut, id: "1"},
		{name: "delete shared task", handler: deleteTask, method: http.MethodDelete, id: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, "/tasks/"+tc.id, nil)
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = request
			context.Params = gin.Params{{Key: "id", Value: tc.id}}
			tc.handler(context)
			if recorder.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestUpdateRouteUsesDefaultConfigFenceWithoutMutation(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.AutoMigrate(&models.DestinationScopeMaintenance{}); err != nil {
		t.Fatal(err)
	}
	explicitConfig := t.TempDir() + "/explicit.conf"
	task := models.Task{ID: 1, Name: "explicit-config", SourceType: "local", SourceDir: "/source", DestType: "remote", RemoteName: "remote", RemoteDir: "/dest", TransferMode: models.TransferModeCopy, RcloneConfig: explicitConfig, TaskType: "normal", Enabled: true, WatchEnabled: false, ScheduleEnabled: false, QBEnabled: false}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	var before models.Task
	if err := database.First(&before, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.DestinationScopeMaintenance{DestinationScope: models.DestinationScope(models.DefaultRcloneConfigPath, "/dest"), Epoch: 1, OwnerTaskID: 99, State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateRunning, Reason: models.MaintenanceReasonQuotaExhaustion, Revision: 1}).Error; err != nil {
		t.Fatal(err)
	}
	dispatcher := &proactive.Dispatcher{DB: database, ConfigResolver: func(raw string) (string, error) { return raw, nil }}
	previousDB := db
	previousConfig := cfgGlobal
	previousDispatcher := proactiveDispatcher
	previousExecutor := executor
	previousTaskRunner := taskRunner
	previousScheduler := sched
	previousWatcher := watch
	previousQBWatcher := qbWatch
	db, cfgGlobal, proactiveDispatcher = database, &config.Config{}, dispatcher
	executor = rclone.NewExecutor(nil, database)
	taskRunner = taskdispatch.New(database, mutationRaceLegacyRunner{}, dispatcher)
	sched = scheduler.NewScheduler(taskRunner)
	watch = watcher.NewWatcher(taskRunner)
	qbWatch = qbittorrent.NewWatcher(executor)
	defer func() {
		db = previousDB
		cfgGlobal = previousConfig
		proactiveDispatcher = previousDispatcher
		executor = previousExecutor
		taskRunner = previousTaskRunner
		sched = previousScheduler
		watch = previousWatcher
		qbWatch = previousQBWatcher
	}()

	request := httptest.NewRequest(http.MethodPut, "/tasks/1", strings.NewReader(`{"name":"explicit-config","source_type":"local","source_dir":"/source","dest_type":"remote","remote_name":"remote","remote_dir":"/dest","transfer_mode":"copy","rclone_config":"","task_type":"normal","enabled":true,"watch_enabled":false,"schedule_enabled":false,"qb_enabled":false}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	context.Params = gin.Params{{Key: "id", Value: "1"}}
	updateTask(context)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("update status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var stored models.Task
	if err := database.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, before) {
		t.Fatalf("blocked update mutated task: before=%#v after=%#v", before, stored)
	}
}

func TestManualStartDeleteRaceLeavesNoOrphanEpoch(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}); err != nil {
		t.Fatal(err)
	}
	configPath := t.TempDir() + "/rclone.conf"
	if err := os.WriteFile(configPath, []byte("[remote]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.Task{ID: 1, TaskType: "rotation", RotationStrategy: "proactive_quota", RcloneConfig: configPath, RemoteDir: "/dest", RotationRemotes: `["remote"]`, RotationQuotaKeys: `{"remote":"race-key"}`, Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.QuotaAccount{QuotaKey: "race-key", RemoteName: "remote", ConfigIdentity: configPath, BudgetBytes: 100, WindowSeconds: 60, Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	dispatcher := &proactive.Dispatcher{DB: database, Quota: &quota.Service{DB: database, ConfigResolver: func(string) (string, error) { return configPath, nil }}, ConfigResolver: func(string) (string, error) { return configPath, nil }}
	previousDB, previousDispatcher, previousRunner := db, proactiveDispatcher, taskRunner
	db, proactiveDispatcher = database, dispatcher
	taskRunner = taskdispatch.New(database, mutationRaceLegacyRunner{}, dispatcher)
	defer func() { db, proactiveDispatcher, taskRunner = previousDB, previousDispatcher, previousRunner }()

	entered, release, exclusiveDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		exclusiveDone <- taskRunner.WithTaskExclusive(context.Background(), 1, func(*models.Task) error {
			if err := database.Delete(&models.Task{}, 1).Error; err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	result := runManualMergeHandler(t, "1")
	select {
	case response := <-result:
		t.Fatalf("manual start escaped delete gate with status %d body=%s", response.code, response.body)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-exclusiveDone; err != nil {
		t.Fatal(err)
	}
	if response := <-result; response.code != http.StatusNotFound {
		t.Fatalf("manual start after exclusive delete status=%d body=%s, want 404", response.code, response.body)
	}
	var epochs int64
	if err := database.Model(&models.DestinationScopeMaintenance{}).Count(&epochs).Error; err != nil {
		t.Fatal(err)
	}
	if epochs != 0 {
		t.Fatalf("delete race left orphaned epochs: %d", epochs)
	}
}

func TestManualStartUpdateRaceUsesPostUpdateScope(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}); err != nil {
		t.Fatal(err)
	}
	configPath := t.TempDir() + "/rclone.conf"
	if err := os.WriteFile(configPath, []byte("[remote]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.Task{ID: 1, TaskType: "rotation", RotationStrategy: "proactive_quota", RcloneConfig: configPath, RemoteDir: "/old", RotationRemotes: `["remote"]`, RotationQuotaKeys: `{"remote":"race-key"}`, Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.QuotaAccount{QuotaKey: "race-key", RemoteName: "remote", ConfigIdentity: configPath, BudgetBytes: 100, WindowSeconds: 60, Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	dispatcher := &proactive.Dispatcher{DB: database, Quota: &quota.Service{DB: database, ConfigResolver: func(string) (string, error) { return configPath, nil }}, ConfigResolver: func(string) (string, error) { return configPath, nil }}
	previousDB, previousDispatcher, previousRunner := db, proactiveDispatcher, taskRunner
	db, proactiveDispatcher = database, dispatcher
	taskRunner = taskdispatch.New(database, mutationRaceLegacyRunner{}, dispatcher)
	defer func() { db, proactiveDispatcher, taskRunner = previousDB, previousDispatcher, previousRunner }()

	entered, release, exclusiveDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		exclusiveDone <- taskRunner.WithTaskExclusive(context.Background(), 1, func(*models.Task) error {
			if err := database.Model(&models.Task{}).Where("id = ?", 1).Update("remote_dir", "/new").Error; err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	result := runManualMergeHandler(t, "1")
	select {
	case response := <-result:
		t.Fatalf("manual start escaped update gate with status %d body=%s", response.code, response.body)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-exclusiveDone; err != nil {
		t.Fatal(err)
	}
	if response := <-result; response.code != http.StatusAccepted {
		t.Fatalf("manual start after update status=%d body=%s, want 202", response.code, response.body)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var active int64
		if err := database.Model(&models.DestinationScopeMaintenance{}).Where("state = ? AND dedupe_state IN ?", models.MaintenanceStateExhausted, []string{"", models.DedupeStatePending, models.DedupeStateClaimed, models.DedupeStateRunning}).Count(&active).Error; err != nil {
			t.Fatal(err)
		}
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("manual completion goroutine did not terminalize before test cleanup")
		}
		time.Sleep(5 * time.Millisecond)
	}
	var epoch models.DestinationScopeMaintenance
	if err := database.First(&epoch).Error; err != nil {
		t.Fatal(err)
	}
	if epoch.DestinationScope != models.DestinationScope(configPath, "/new") {
		t.Fatalf("manual epoch used stale scope: %#v", epoch)
	}
}
