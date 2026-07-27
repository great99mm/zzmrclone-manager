package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"rclone-manager/internal/auth"
	"rclone-manager/internal/config"
	"rclone-manager/internal/manualtransfer"
	"rclone-manager/internal/models"
)

func TestManualAnalyzeAPIRequiresRunCASForReanalysis(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "api-manual.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.AutoMigrate(&models.Task{}, &models.QuotaAccount{}, &manualtransfer.ManualTransferRun{}, &manualtransfer.ManualRunAccount{}, &manualtransfer.ManualRunFile{}, &manualtransfer.ManualRunEvent{}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	task := models.Task{Name: "api", SourceType: "local", SourceDir: root, DestType: "remote", RemoteName: "remote", RemoteDir: "/dest", TransferMode: models.TransferModeCopy, TaskType: models.TaskTypeManual, ManualStrategy: models.ManualStrategyAllocation, WatchEnabled: false, ScheduleEnabled: false, QBEnabled: false, Enabled: true}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	for id, suffix := range map[uint]string{1: "a", 2: "b"} {
		if err := database.Create(&models.QuotaAccount{ID: id, QuotaKey: "account-" + suffix, RemoteName: "remote-" + suffix, ConfigIdentity: filepath.Join(root, "config-"+suffix), Enabled: true}).Error; err != nil {
			t.Fatal(err)
		}
	}
	service := manualtransfer.NewService(database)
	service.Start()
	defer service.Stop()
	previousDB, previousService, previousConfig := db, manualTransferService, cfgGlobal
	db, manualTransferService, cfgGlobal = database, service, &config.Config{}
	defer func() { db, manualTransferService, cfgGlobal = previousDB, previousService, previousConfig }()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/tasks/:id/manual-runs/analyze", requireAdminStrictTokenOrSession, analyzeManualRun)
	router.POST("/api/manual-runs/:id/allocate", requireAdminStrictTokenOrSession, allocateManualRun)
	session := "Bearer " + auth.IssueToken("api-admin", true)
	body := `{"source_path":"` + root + `","destination_path":"/dest","transfer_mode":"copy","config_identity":"caller-config","idempotency_key":"api-first","accounts":[{"account_id":1},{"account_id":2}]}`
	status, response := callManualAnalyzeAPI(t, router, task.ID, body, session)
	if status != http.StatusAccepted {
		t.Fatalf("initial analyze status=%d body=%s", status, response)
	}
	var created struct {
		Run struct {
			ID       uint  `json:"id"`
			Revision int64 `json:"revision"`
		} `json:"run"`
	}
	if err := json.Unmarshal([]byte(response), &created); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var run manualtransfer.ManualTransferRun
	for time.Now().Before(deadline) {
		if err := database.First(&run, created.Run.ID).Error; err != nil {
			t.Fatal(err)
		}
		if run.State == manualtransfer.ManualRunStateAnalyzed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if run.State != manualtransfer.ManualRunStateAnalyzed {
		t.Fatalf("run did not analyze: %#v", run)
	}
	missingAllocateCAS := httptest.NewRecorder()
	missingAllocateRequest := httptest.NewRequest(http.MethodPost, "/api/manual-runs/"+strconv.FormatUint(uint64(run.ID), 10)+"/allocate", strings.NewReader(`{"expected_revision":`+strconv.FormatInt(run.Revision, 10)+`,"idempotency_key":"api-allocate"}`))
	missingAllocateRequest.Header.Set("Authorization", session)
	missingAllocateRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(missingAllocateCAS, missingAllocateRequest)
	if missingAllocateCAS.Code != http.StatusBadRequest {
		t.Fatalf("missing allocate CAS status=%d body=%s", missingAllocateCAS.Code, missingAllocateCAS.Body.String())
	}
	previousRunID := run.ID
	previousRunRevision := run.Revision
	reanalysis := `{"source_path":"` + root + `","destination_path":"/dest","transfer_mode":"copy","idempotency_key":"api-second","expected_run_id":` + strconv.FormatUint(uint64(previousRunID), 10) + `,"expected_revision":` + strconv.FormatInt(previousRunRevision, 10) + `,"accounts":[{"account_id":1},{"account_id":2}]}`
	if status, response = callManualAnalyzeAPI(t, router, task.ID, reanalysis, session); status != http.StatusAccepted {
		t.Fatalf("explicit reanalysis status=%d body=%s", status, response)
	}
	var replacement struct {
		Run struct {
			ID       uint  `json:"id"`
			Revision int64 `json:"revision"`
		} `json:"run"`
	}
	if err := json.Unmarshal([]byte(response), &replacement); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	var replacementRun manualtransfer.ManualTransferRun
	for time.Now().Before(deadline) {
		if err := database.First(&replacementRun, replacement.Run.ID).Error; err != nil {
			t.Fatal(err)
		}
		if replacementRun.State == manualtransfer.ManualRunStateAnalyzed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if replacementRun.State != manualtransfer.ManualRunStateAnalyzed {
		t.Fatalf("replacement did not analyze: %#v", replacementRun)
	}
	if status, response = callManualAnalyzeAPI(t, router, task.ID, reanalysis, session); status != http.StatusAccepted {
		t.Fatalf("replacement replay status=%d body=%s", status, response)
	}
	var replay struct {
		Run struct {
			ID uint `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal([]byte(response), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Run.ID != replacement.Run.ID {
		t.Fatalf("replacement replay returned run %d, want %d", replay.Run.ID, replacement.Run.ID)
	}
	changed := `{"source_path":"` + root + `","destination_path":"/changed","transfer_mode":"copy","idempotency_key":"api-second","expected_run_id":` + strconv.FormatUint(uint64(previousRunID), 10) + `,"expected_revision":` + strconv.FormatInt(previousRunRevision, 10) + `,"accounts":[{"account_id":1},{"account_id":2}]}`
	if status, response = callManualAnalyzeAPI(t, router, task.ID, changed, session); status != http.StatusConflict || !strings.Contains(response, "idempotency") {
		t.Fatalf("changed existing idempotency request status=%d body=%s", status, response)
	}
	stale := `{"source_path":"` + root + `","destination_path":"/dest","transfer_mode":"copy","idempotency_key":"api-third","expected_run_id":` + strconv.FormatUint(uint64(previousRunID), 10) + `,"expected_revision":` + strconv.FormatInt(previousRunRevision, 10) + `,"accounts":[{"account_id":1},{"account_id":2}]}`
	if status, response = callManualAnalyzeAPI(t, router, task.ID, stale, session); status != http.StatusConflict || !strings.Contains(response, "revision conflict") {
		t.Fatalf("stale reanalysis status=%d body=%s", status, response)
	}
}

func TestManualAccountAPIRequiresExplicitRevisionAndIdempotency(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "api-manual-accounts.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.AutoMigrate(&models.Task{}, &models.QuotaAccount{}, &manualtransfer.ManualTaskAccount{}); err != nil {
		t.Fatal(err)
	}
	task := models.Task{Name: "manual-account-api", TaskType: models.TaskTypeManual, ManualStrategy: models.ManualStrategyAllocation, SourceType: "local", SourceDir: t.TempDir(), DestType: "remote", RemoteName: "remote", RemoteDir: "/dest", TransferMode: models.TransferModeCopy, Enabled: true, WatchEnabled: false, ScheduleEnabled: false, QBEnabled: false}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.QuotaAccount{QuotaKey: "api-account", RemoteName: "remote", ConfigIdentity: "/config", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	service := manualtransfer.NewService(database)
	previousDB, previousService, previousConfig := db, manualTransferService, cfgGlobal
	db, manualTransferService, cfgGlobal = database, service, &config.Config{}
	defer func() { db, manualTransferService, cfgGlobal = previousDB, previousService, previousConfig }()
	router := gin.New()
	router.PUT("/api/tasks/:id/manual-accounts", requireAdminStrictTokenOrSession, updateManualTaskAccounts)
	authorization := "Bearer " + auth.IssueToken("api-admin", true)
	call := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, "/api/tasks/"+strconv.FormatUint(uint64(task.ID), 10)+"/manual-accounts", strings.NewReader(body))
		request.Header.Set("Authorization", authorization)
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		return recorder
	}
	if response := call(`{"account_ids":[1],"expected_revision":1}`); response.Code != http.StatusBadRequest {
		t.Fatalf("missing idempotency status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call(`{"account_ids":[1],"idempotency_key":"api-account"}`); response.Code != http.StatusConflict {
		t.Fatalf("missing revision status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call(`{"account_ids":[1],"expected_revision":1,"idempotency_key":"api-account"}`); response.Code != http.StatusOK {
		t.Fatalf("valid account save status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCreateManualTaskBindsSelectedAccounts(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.AutoMigrate(&manualtransfer.ManualTaskAccount{}); err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.QuotaAccount{QuotaKey: "created-manual-account", RemoteName: "drive-account", ConfigIdentity: "/config", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	previousDB, previousService, previousConfig := db, manualTransferService, cfgGlobal
	db, manualTransferService, cfgGlobal = database, manualtransfer.NewService(database), &config.Config{}
	defer func() { db, manualTransferService, cfgGlobal = previousDB, previousService, previousConfig }()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(`{"name":"manual","task_type":"manual","manual_strategy":"allocation_v1","source_type":"local","source_dir":" /source/ ","dest_type":"remote","remote_name":"drive","remote_dir":"/dest","transfer_mode":"copy","manual_account_ids":[1]}`))
	context.Request.Header.Set("Content-Type", "application/json")
	createTask(context)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var task models.Task
	if err := database.Order("id DESC").First(&task).Error; err != nil {
		t.Fatal(err)
	}
	if task.SourceDir != "/source" {
		t.Fatalf("source directory = %q", task.SourceDir)
	}
	var account manualtransfer.ManualTaskAccount
	if err := database.Where("task_id = ?", task.ID).First(&account).Error; err != nil {
		t.Fatal(err)
	}
	if account.AccountID != 1 {
		t.Fatalf("bound account = %d", account.AccountID)
	}
}

func callManualAnalyzeAPI(t *testing.T, router *gin.Engine, taskID uint, body, authorization string) (int, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/tasks/"+strconv.FormatUint(uint64(taskID), 10)+"/manual-runs/analyze", strings.NewReader(body))
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}
