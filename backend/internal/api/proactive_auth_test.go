package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"rclone-manager/internal/auth"
	"rclone-manager/internal/config"
	"rclone-manager/internal/models"
	"rclone-manager/internal/proactive"
	"rclone-manager/internal/quota"
	"rclone-manager/internal/taskdispatch"
)

func runStrictAuth(t *testing.T, cfg *config.Config, query, authorization string) int {
	t.Helper()
	previous := cfgGlobal
	cfgGlobal = cfg
	defer func() { cfgGlobal = previous }()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/resolve", requireStrictTokenOrSession, func(c *gin.Context) { c.Status(http.StatusNoContent) })
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/resolve?"+query, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	router.ServeHTTP(recorder, request)
	return recorder.Code
}

func TestStrictMoveResolutionAuthRequiresSessionOrExactToken(t *testing.T) {
	if code := runStrictAuth(t, &config.Config{}, "", ""); code != http.StatusForbidden {
		t.Fatalf("anonymous optional-token request = %d", code)
	}
	if code := runStrictAuth(t, &config.Config{}, "", "Bearer arbitrary"); code != http.StatusForbidden {
		t.Fatalf("bad bearer optional-token request = %d", code)
	}
	session := auth.IssueToken("phase3", true)
	if code := runStrictAuth(t, &config.Config{}, "", "Bearer "+session); code != http.StatusNoContent {
		t.Fatalf("valid session request = %d", code)
	}
	if code := runStrictAuth(t, &config.Config{APIToken: "configured"}, "token=configured", ""); code != http.StatusNoContent {
		t.Fatalf("exact configured token request = %d", code)
	}
	if code := runStrictAuth(t, &config.Config{APIToken: "configured"}, "token=wrong", "Bearer arbitrary"); code != http.StatusForbidden {
		t.Fatalf("bad configured credentials request = %d", code)
	}
}

type routeManualExecutor struct{ db *gorm.DB }

func (r routeManualExecutor) RunBatch(context.Context, uint) error { return nil }
func (routeManualExecutor) ExecuteMove(*models.Task) error         { return nil }
func (routeManualExecutor) IsRunning(uint) bool                    { return false }
func (routeManualExecutor) StopTask(uint) error                    { return nil }
func (r routeManualExecutor) RunDedupe(_ context.Context, epoch models.DestinationScopeMaintenance) error {
	return r.db.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND state = ? AND dedupe_state IN ? AND lease_token = ?", epoch.ID, models.MaintenanceStateExhausted, []string{models.DedupeStateClaimed, models.DedupeStateRunning}, epoch.LeaseToken).Updates(map[string]interface{}{"state": models.MaintenanceStateClosed, "dedupe_state": models.DedupeStateSucceeded, "result": models.DedupeStateSucceeded, "finished_at": time.Now()}).Error
}

type routeDeadInspector struct{}

func (routeDeadInspector) Inspect(int, string) (proactive.ProcessStatus, error) {
	return proactive.ProcessStatus{Confirmed: true, Alive: false}, nil
}

func TestActualManualMaintenanceRoutesStartAndCloseWithCAS(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}); err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "rclone.conf")
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if err := database.Create(&models.QuotaAccount{QuotaKey: "route-key", RemoteName: "remote", ConfigIdentity: file.Name(), BudgetBytes: 100, WindowSeconds: 60, Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.Task{ID: 1, Enabled: true, TaskType: "rotation", RotationStrategy: "proactive_quota", RcloneConfig: file.Name(), RotationRemotes: `["remote"]`, RotationQuotaKeys: `{"remote":"route-key"}`, RemoteDir: "/dest", SourceType: "local", SourceDir: "/source", TransferMode: models.TransferModeCopy}).Error; err != nil {
		t.Fatal(err)
	}
	service := &quota.Service{DB: database, ConfigResolver: func(string) (string, error) { return file.Name(), nil }}
	dispatcher := &proactive.Dispatcher{DB: database, Quota: service, Executor: routeManualExecutor{db: database}, Inspector: routeDeadInspector{}, ConfigResolver: service.ResolveConfigPath, Now: func() time.Time { return time.Unix(100, 0) }}
	previousDB, previousDispatcher, previousConfig, previousTaskRunner := db, proactiveDispatcher, cfgGlobal, taskRunner
	db, proactiveDispatcher, cfgGlobal = database, dispatcher, &config.Config{}
	taskRunner = taskdispatch.New(database, routeManualExecutor{db: database}, dispatcher)
	defer func() {
		db, proactiveDispatcher, cfgGlobal, taskRunner = previousDB, previousDispatcher, previousConfig, previousTaskRunner
	}()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/tasks/:id/proactive-manual-merge", requireStrictTokenOrSession, startProactiveManualMerge)
	router.POST("/api/proactive-maintenance/:id/close-unknown", requireStrictTokenOrSession, closeProactiveUnknownMaintenance)
	session := "Bearer " + auth.IssueToken("phase6-route", true)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/tasks/1/proactive-manual-merge", nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("anonymous manual start status=%d", recorder.Code)
	}
	taskRouteRecorder := httptest.NewRecorder()
	router.ServeHTTP(taskRouteRecorder, httptest.NewRequest(http.MethodPost, "/api/tasks/1/proactive-maintenance/close-unknown", nil))
	if taskRouteRecorder.Code != http.StatusNotFound {
		t.Fatalf("misleading task-scoped close route status=%d", taskRouteRecorder.Code)
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/tasks/1/proactive-manual-merge", nil)
	request.Header.Set("Authorization", session)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("manual start status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var started models.DestinationScopeMaintenance
	if err := database.Order("id DESC").First(&started).Error; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		var current models.DestinationScopeMaintenance
		if err := database.First(&current, started.ID).Error; err != nil {
			t.Fatal(err)
		}
		if current.State == models.MaintenanceStateClosed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("manual completion did not terminalize: %#v", current)
		}
		time.Sleep(5 * time.Millisecond)
	}
	lease := time.Unix(200, 0)
	unknown := models.DestinationScopeMaintenance{DestinationScope: started.DestinationScope, Epoch: started.Epoch + 1, OwnerTaskID: 1, FirstRemote: "remote", RemoteDir: "/dest", ResolvedConfigPath: file.Name(), ResolvedConfigIdentity: file.Name(), State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateUnknown, Reason: models.MaintenanceReasonManualMerge, LeaseToken: "unknown-owner", LeaseUntil: &lease, ProcessID: 55, ProcessStartToken: "55:1", Revision: 9, LastError: "/secret/config token=identity"}
	if err := database.Create(&unknown).Error; err != nil {
		t.Fatal(err)
	}
	body := `{"reason":"manual_merge","expected_state":"unknown","expected_revision":9}`
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/proactive-maintenance/"+itoa(unknown.ID)+"/close-unknown", strings.NewReader(body))
	request.Header.Set("Authorization", session)
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unknown close status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/proactive-maintenance/"+itoa(unknown.ID)+"/close-unknown", strings.NewReader(body))
	request.Header.Set("Authorization", session)
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("stale unknown close status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if routeErr := database.Model(&models.DestinationScopeMaintenance{}).Where("id = ?", unknown.ID).Pluck("last_error", new(string)).Error; routeErr != nil {
		t.Fatal(routeErr)
	}
}

func itoa(value uint) string { return strconv.FormatUint(uint64(value), 10) }
