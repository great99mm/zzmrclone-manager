package api

import (
	"encoding/json"
	"fmt"
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
	"rclone-manager/internal/models"
	"rclone-manager/internal/proactive"
)

func proactiveStatusTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "proactive-status.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.AutoMigrate(&models.Task{}, &models.SystemSetting{}, &models.QuotaAccount{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &models.QuotaReservation{}, &models.NetworkTelemetrySample{}); err != nil {
		t.Fatal(err)
	}
	return database
}

func callProactiveStatus(t *testing.T, database *gorm.DB, taskID uint) (int, map[string]interface{}) {
	return callProactiveStatusURL(t, database, taskID, "")
}

func callProactiveStatusURL(t *testing.T, database *gorm.DB, taskID uint, query string) (int, map[string]interface{}) {
	t.Helper()
	previous := db
	db = database
	defer func() { db = previous }()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/tasks/1/proactive-status?"+query, nil)
	context.Params = gin.Params{{Key: "id", Value: "1"}}
	if taskID != 1 {
		context.Params[0].Value = "2"
	}
	getProactiveStatus(context)
	var body map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (%s)", err, recorder.Body.String())
	}
	return recorder.Code, body
}

func TestProactiveStatusReturnsAccountsBatchesAndReservations(t *testing.T) {
	database := proactiveStatusTestDB(t)
	task := models.Task{ID: 1, TaskType: "rotation", RotationStrategy: "proactive_quota", TransferMode: models.TransferModeCopy, RotationRemotes: `['drive']`, RotationQuotaKeys: `{"drive":"drive-key"}`, Status: "running", Enabled: true}
	// ParseRotationRemotes accepts JSON arrays; use its canonical encoding in the fixture.
	task.RotationRemotes = `["drive"]`
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	account := models.QuotaAccount{QuotaKey: "drive-key", RemoteName: "drive", BudgetBytes: 1000, WindowSeconds: 3600, Enabled: true}
	if err := database.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Minute)
	batch := models.RotationQuotaBatch{ID: 1, TaskID: task.ID, QuotaAccountID: account.ID, DestinationRemote: "drive", TransferMode: models.TransferModeCopy, CompletionEvidence: models.CompletionEvidenceRemote, CompletionEvidenceVersion: 1, State: models.BatchStateRunning, ReservedBytes: 300, StartedAt: &started}
	if err := database.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	file := models.RotationQuotaBatchFile{BatchID: batch.ID, RelativePath: "file.bin", SnapshotKey: "snapshot", SizeBytes: 300, State: models.BatchFileStateActive}
	if err := database.Create(&file).Error; err != nil {
		t.Fatal(err)
	}
	committed := time.Now().Add(-time.Minute)
	if err := database.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchID: batch.ID, BatchFileID: file.ID, Bytes: 200, State: models.ReservationStateCommitted, ExpiresAt: ptrTime(time.Now().Add(time.Hour)), ReservedAt: &committed, IdempotencyKey: "committed"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchID: batch.ID, BatchFileID: file.ID + 1, Bytes: 300, State: models.ReservationStateActive, IdempotencyKey: "active"}).Error; err != nil {
		t.Fatal(err)
	}

	code, body := callProactiveStatus(t, database, task.ID)
	if code != 200 {
		t.Fatalf("status = %d, body = %#v", code, body)
	}
	accounts := body["accounts"].([]interface{})[0].(map[string]interface{})
	wantRemaining := float64(models.DefaultRotationQuotaLimitBytes - 200 - 300)
	if accounts["used_bytes"] != float64(200) || accounts["active_reserved_bytes"] != float64(300) || accounts["remaining_bytes"] != wantRemaining {
		t.Fatalf("unexpected account totals: %#v", accounts)
	}
	batches := body["batches"].([]interface{})
	if len(batches) != 1 || batches[0].(map[string]interface{})["remote"] != "drive" {
		t.Fatalf("unexpected batches: %#v", batches)
	}
	if batches[0].(map[string]interface{})["process"].(map[string]interface{})["active"] != false {
		t.Fatal("batch without a persisted process identity was reported active")
	}
	if batches[0].(map[string]interface{})["transfer_mode"] != models.TransferModeCopy || body["task"].(map[string]interface{})["transfer_mode"] != models.TransferModeCopy {
		t.Fatalf("transfer mode missing from status: %#v", body)
	}
	if batches[0].(map[string]interface{})["completion_evidence"] != models.CompletionEvidenceRemote {
		t.Fatalf("completion evidence missing from status: %#v", batches[0])
	}
}

func TestProactiveStatusExposesLegacyRecoveryAndCloseWakesScope(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	task := models.Task{ID: 1, TaskType: "rotation", RotationStrategy: "proactive_quota", RcloneConfig: configPath, RemoteDir: "/dest", RotationRemotes: `["remote"]`, Enabled: true}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	legacyToken := "0123456789abcdef0123456789abcdef0123456789abcdef"
	legacyError := fmt.Sprintf("pid=77 config=%s token=%s legacy-secret", configPath, legacyToken)
	epoch := models.DestinationScopeMaintenance{DestinationScope: models.DestinationScope(configPath, "/dest"), Epoch: 4, OwnerTaskID: task.ID, FirstRemote: "remote", RemoteDir: "/dest", ResolvedConfigPath: configPath, ResolvedConfigIdentity: configPath, State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateUnknown, Reason: models.MaintenanceReasonQuotaExhaustion, Revision: 9, ProcessID: 77, ProcessStartToken: legacyToken, LastError: legacyError}
	if err := database.Create(&epoch).Error; err != nil {
		t.Fatal(err)
	}
	dispatcher := &proactive.Dispatcher{DB: database, Inspector: routeDeadInspector{}, ConfigResolver: func(string) (string, error) { return configPath, nil }, Now: func() time.Time { return time.Unix(100, 0) }}
	previousDB, previousDispatcher := db, proactiveDispatcher
	db, proactiveDispatcher = database, dispatcher
	defer func() { db, proactiveDispatcher = previousDB, previousDispatcher }()

	code, body := callProactiveStatus(t, database, task.ID)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%#v", code, body)
	}
	maintenance := body["maintenance"].(map[string]interface{})
	if maintenance["blocker"] != "legacy_maintenance_recovery" || maintenance["manual_merge_available"] != false {
		t.Fatalf("legacy blocker projection=%#v", maintenance)
	}
	recovery := maintenance["legacy_recovery"].(map[string]interface{})
	if recovery["epoch_id"] != float64(epoch.ID) || recovery["reason"] != models.MaintenanceReasonQuotaExhaustion || recovery["revision"] != float64(9) || recovery["process_identity_available"] != true {
		t.Fatalf("legacy recovery projection=%#v", recovery)
	}
	if _, exposed := recovery["process_id"]; exposed {
		t.Fatalf("legacy recovery exposed process id: %#v", recovery)
	}
	encoded := string(mustJSON(t, body))
	if strings.Contains(encoded, `"process_id":`) {
		t.Fatalf("legacy recovery leaked process id field: %s", encoded)
	}
	for _, secret := range []string{legacyToken, configPath, legacyError, "legacy-secret"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("legacy recovery leaked %q: %s", secret, encoded)
		}
	}
	closeRecorder := httptest.NewRecorder()
	closeContext, _ := gin.CreateTestContext(closeRecorder)
	closeContext.Request = httptest.NewRequest(http.MethodPost, "/api/proactive-maintenance/1/close-unknown", strings.NewReader(`{"reason":"quota_exhaustion","expected_state":"unknown","expected_revision":9}`))
	closeContext.Request.Header.Set("Content-Type", "application/json")
	closeContext.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(epoch.ID), 10)}}
	closeProactiveUnknownMaintenance(closeContext)
	if closeRecorder.Code != http.StatusOK {
		t.Fatalf("legacy close endpoint status=%d body=%s", closeRecorder.Code, closeRecorder.Body.String())
	}
	var storedEpoch models.DestinationScopeMaintenance
	if err := database.First(&storedEpoch, epoch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedEpoch.State != models.MaintenanceStateClosed || storedEpoch.DedupeState != models.DedupeStateFailed {
		t.Fatalf("legacy close=%#v", storedEpoch)
	}
	var storedTask models.Task
	if err := database.First(&storedTask, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !storedTask.RotationRescanPending || storedTask.RotationQuotaWakeAt == nil {
		t.Fatalf("legacy close did not wake scope: %#v", storedTask)
	}
}

func mustJSON(t *testing.T, value interface{}) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestProactiveStatusResolvesDefaultAliasFromPersistedAccount(t *testing.T) {
	database := proactiveStatusTestDB(t)
	configIdentity := models.DefaultRcloneConfigPath
	defaultKey := models.DefaultRotationQuotaKey(configIdentity, "drive")
	if err := database.Create(&models.Task{ID: 1, TaskType: "rotation", RotationStrategy: "proactive_quota", RotationRemotes: `["drive"]`, RotationQuotaKeys: `{}`}).Error; err != nil {
		t.Fatal(err)
	}
	account := models.QuotaAccount{QuotaKey: defaultKey, RemoteName: "drive", ConfigIdentity: configIdentity, BudgetBytes: 100, WindowSeconds: 60, Enabled: true}
	if err := database.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	otherKey := models.DefaultRotationQuotaKey("/other/config", "drive")
	other := models.QuotaAccount{QuotaKey: otherKey, RemoteName: "drive", ConfigIdentity: "/other/config", BudgetBytes: 900, WindowSeconds: 60, Enabled: true}
	if err := database.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.QuotaReservation{QuotaAccountID: other.ID, Bytes: 700, State: models.ReservationStateCommitted, IdempotencyKey: "other-config"}).Error; err != nil {
		t.Fatal(err)
	}
	batch := models.RotationQuotaBatch{ID: 1, TaskID: 1, QuotaAccountID: account.ID, DestinationRemote: "drive", State: models.BatchStateReserved}
	if err := database.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}

	code, body := callProactiveStatus(t, database, 1)
	if code != 200 {
		t.Fatalf("status = %d, body = %#v", code, body)
	}
	binding := body["accounts"].([]interface{})[0].(map[string]interface{})
	if binding["quota_key"] != defaultKey {
		t.Fatalf("quota key = %v, want %s", binding["quota_key"], defaultKey)
	}
	if binding["used_bytes"] != float64(0) {
		t.Fatalf("status selected another config's ledger: %#v", binding)
	}
	if body["batches"].([]interface{})[0].(map[string]interface{})["account"] != defaultKey {
		t.Fatalf("batch account assignment = %#v", body["batches"])
	}
}

func TestProactiveStatusExposesMoveResolutionWithoutSensitivePaths(t *testing.T) {
	database := proactiveStatusTestDB(t)
	root := filepath.Join(t.TempDir(), "source")
	owner := strings.Repeat("a", 48)
	if err := database.Create(&models.Task{ID: 1, TaskType: "rotation", RotationStrategy: "proactive_quota", SourceDir: root, RcloneConfig: filepath.Join(t.TempDir(), "rclone.conf"), TransferMode: models.TransferModeMove, RotationRemotes: `["drive"]`, RotationQuotaKeys: `{"drive":"move-key"}`, LastError: root + " token-" + owner}).Error; err != nil {
		t.Fatal(err)
	}
	account := models.QuotaAccount{QuotaKey: "move-key", RemoteName: "drive", BudgetBytes: 100, WindowSeconds: 60, Enabled: true}
	if err := database.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	batch := models.RotationQuotaBatch{ID: 1, TaskID: 1, QuotaAccountID: account.ID, DestinationRemote: "drive", TransferMode: models.TransferModeMove, State: models.BatchStateUnknown, LastError: root + " owner=" + owner, MoveHandoffContractVersion: models.MoveHandoffVersion}
	if err := database.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.RotationQuotaBatchFile{BatchID: batch.ID, RelativePath: "secret.bin", SizeBytes: 1, State: models.BatchFileStateUnknown, MoveHandoffState: models.MoveHandoffUnknown}).Error; err != nil {
		t.Fatal(err)
	}
	code, body := callProactiveStatus(t, database, 1)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %#v", code, body)
	}
	batches := body["batches"].([]interface{})
	status := batches[0].(map[string]interface{})
	if status["resolution_required"] != true {
		t.Fatalf("resolution requirement missing: %#v", status)
	}
	actions := status["resolution_actions"].([]interface{})
	if len(actions) != 2 || actions[0] != "accept_moved" || actions[1] != "restore_and_release" {
		t.Fatalf("resolution actions = %#v", actions)
	}
	items := status["resolution_items"].([]interface{})
	item := items[0].(map[string]interface{})
	if item["batch_id"] != float64(1) || item["expected_state"] != models.BatchFileStateUnknown || item["file_id"] == nil || item["expected_updated_at"] == nil {
		t.Fatalf("resolution CAS item incomplete: %#v", item)
	}
	encoded, _ := json.Marshal(body)
	if strings.Contains(string(encoded), root) || strings.Contains(string(encoded), owner) {
		t.Fatalf("status leaked sensitive move context: %s", encoded)
	}
}

func TestProactiveStatusRedactsTaskErrorFromAllBatchContext(t *testing.T) {
	database := proactiveStatusTestDB(t)
	task := models.Task{ID: 1, TaskType: "rotation", RotationStrategy: "proactive_quota", SourceDir: "/source", RcloneConfig: "/config", RotationRemotes: `["drive"]`, RotationQuotaKeys: "{}"}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	batch := models.RotationQuotaBatch{ID: 1, TaskID: 1, OwnerToken: "owner-token", LeaseToken: "lease-token", ProcessStartToken: "process-token", RcloneConfigPath: "/config", SourceRoot: "/stage", ManifestPath: "/stage/batch-owner-token-lease-token.manifest", DestinationPath: "/dest", RequestKey: "request-token", RequestFingerprint: "fingerprint-token", DestinationRemote: "drive", State: models.BatchStateFailed}
	if err := database.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	errorText := "/stage/batch-owner-token-lease-token.manifest owner-token lease-token process-token request-token /source"
	if err := database.Model(&task).Update("last_error", errorText).Error; err != nil {
		t.Fatal(err)
	}
	code, body := callProactiveStatus(t, database, 1)
	if code != 200 {
		t.Fatalf("status = %d, body = %#v", code, body)
	}
	currentError := body["task"].(map[string]interface{})["current_error"].(string)
	for _, secret := range []string{"/stage", "owner-token", "lease-token", "process-token", "request-token", "/source"} {
		if strings.Contains(currentError, secret) {
			t.Fatalf("task error leaked %q: %s", secret, currentError)
		}
	}
}

func TestProactiveStatusAcceptsBearerSessionAndExactAPIToken(t *testing.T) {
	previous := cfgGlobal
	cfgGlobal = &config.Config{APIToken: "server-token"}
	defer func() { cfgGlobal = previous }()
	for name, request := range map[string]*http.Request{
		"bearer":      httptest.NewRequest(http.MethodGet, "/api/tasks/1/proactive-status", nil),
		"query token": httptest.NewRequest(http.MethodGet, "/api/tasks/1/proactive-status?token=server-token", nil),
	} {
		if name == "bearer" {
			request.Header.Set("Authorization", "Bearer "+auth.IssueToken("admin", true))
		}
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = request
		requireTokenOrSession(context)
		if context.IsAborted() {
			t.Fatalf("%s access rejected: %d", name, recorder.Code)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/tasks/1/proactive-status", nil)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	requireTokenOrSession(context)
	if !context.IsAborted() || recorder.Code != http.StatusForbidden {
		t.Fatalf("unauthorized access was not rejected: %d", recorder.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/tasks/1/proactive-status", nil)
	request.Header.Set("Authorization", "Bearer invalid-session")
	recorder = httptest.NewRecorder()
	context, _ = gin.CreateTestContext(recorder)
	context.Request = request
	requireTokenOrSession(context)
	if !context.IsAborted() || recorder.Code != http.StatusForbidden {
		t.Fatalf("invalid bearer access was not rejected: %d", recorder.Code)
	}
}

func TestProactiveStatusQueueUsesBatchLifecycleOnce(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.Create(&models.Task{ID: 1, TaskType: "rotation", RotationStrategy: "proactive_quota", RotationRemotes: `["drive"]`, RotationQuotaKeys: "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	states := []struct {
		id    uint
		state string
		size  int64
	}{
		{1, models.BatchStateReserved, 10},
		{2, models.BatchStatePlanned, 20},
		{3, models.BatchStateRunning, 30},
		{4, models.BatchStateSucceeded, 40},
		{5, models.BatchStateFailed, 50},
		{6, models.BatchStateCanceled, 60},
		{7, models.BatchStateExpired, 70},
	}
	for _, item := range states {
		batch := models.RotationQuotaBatch{ID: item.id, TaskID: 1, DestinationRemote: "drive-" + string(rune('a'+item.id)), RequestKey: "request-" + string(rune('a'+item.id)), OwnerToken: "owner-" + string(rune('a'+item.id)), State: item.state}
		if err := database.Create(&batch).Error; err != nil {
			t.Fatal(err)
		}
		file := models.RotationQuotaBatchFile{BatchID: item.id, RelativePath: "file-" + string(rune('a'+item.id)), SnapshotKey: "snapshot-" + string(rune('a'+item.id)), SizeBytes: item.size, State: models.BatchFileStateHeld}
		if err := database.Create(&file).Error; err != nil {
			t.Fatal(err)
		}
	}
	code, body := callProactiveStatus(t, database, 1)
	if code != 200 {
		t.Fatalf("status = %d, body = %#v", code, body)
	}
	queue := body["queue"].(map[string]interface{})
	want := map[string]float64{"pending": 10, "planned": 20, "executing": 30, "verified": 40, "failed": 50}
	for category, bytes := range want {
		actual := queue[category].(map[string]interface{})["bytes"]
		if actual != bytes {
			t.Fatalf("queue.%s.bytes = %v, want %v", category, actual, bytes)
		}
	}
}

func TestProactiveManualAvailabilityMatchesAccountWideBlocker(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.AutoMigrate(&models.QuotaAccount{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &models.QuotaReservation{}, &models.DestinationScopeCoordinator{}, &models.DestinationScopeMaintenance{}); err != nil {
		t.Fatal(err)
	}
	account := models.QuotaAccount{QuotaKey: "shared-status", RemoteName: "remote", BudgetBytes: 100, WindowSeconds: 60, Enabled: true}
	if err := database.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	other := models.RotationQuotaBatch{TaskID: 99, QuotaAccountID: account.ID, DestinationScope: models.DestinationScope("/config", "/other"), State: models.BatchStateUnknown, RequestKey: "status-other", OwnerToken: "status-owner"}
	if err := database.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	previous := db
	db = database
	defer func() { db = previous }()
	available, blocker := proactiveManualMergeAvailability(models.DestinationScope("/config", "/dest"), models.DestinationScopeMaintenance{}, map[string]string{"remote": "shared-status"}, "idle", time.Now())
	if available || blocker != "account_active_elsewhere" {
		t.Fatalf("status blocker parity = available=%v blocker=%q", available, blocker)
	}
}

func TestProactiveManualAvailabilityRejectsNonIdleTask(t *testing.T) {
	database := proactiveStatusTestDB(t)
	previous := db
	db = database
	defer func() { db = previous }()
	for _, status := range []string{"running", "paused", "error", "canceled"} {
		available, blocker := proactiveManualMergeAvailability("scope", models.DestinationScopeMaintenance{}, map[string]string{}, status, time.Now())
		if available || blocker != "task_running" {
			t.Fatalf("status=%q: available=%v blocker=%q", status, available, blocker)
		}
	}
	available, blocker := proactiveManualMergeAvailability("scope", models.DestinationScopeMaintenance{}, map[string]string{}, "idle", time.Now())
	if !available || blocker != "" {
		t.Fatalf("idle: available=%v blocker=%q", available, blocker)
	}
}

func TestProactiveStatusProjectsBlockerAndRedactsMaintenanceError(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "manual-rclone.conf")
	if err := os.WriteFile(configPath, []byte("[remote]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	task := models.Task{ID: 1, Enabled: true, TaskType: "rotation", RotationStrategy: "proactive_quota", RcloneConfig: configPath, RemoteDir: "/dest", RotationRemotes: `["remote"]`, RotationQuotaKeys: `{"remote":"status-shared"}`}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	account := models.QuotaAccount{QuotaKey: "status-shared", RemoteName: "remote", BudgetBytes: 100, WindowSeconds: 60, Enabled: true}
	if err := database.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.RotationQuotaBatch{TaskID: 99, QuotaAccountID: account.ID, DestinationScope: models.DestinationScope(configPath, "/other"), State: models.BatchStateUnknown, RequestKey: "status-blocker", OwnerToken: "status-owner"}).Error; err != nil {
		t.Fatal(err)
	}
	code, body := callProactiveStatus(t, database, 1)
	if code != http.StatusOK || body["maintenance"].(map[string]interface{})["blocker"] != "account_active_elsewhere" {
		t.Fatalf("status account blocker parity: code=%d body=%#v", code, body)
	}
	secretToken := "0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := database.Create(&models.DestinationScopeMaintenance{DestinationScope: models.DestinationScope(configPath, "/dest"), Epoch: 1, OwnerTaskID: 1, FirstRemote: "remote", RemoteDir: "/dest", ResolvedConfigPath: configPath, ResolvedConfigIdentity: configPath, State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStateUnknown, Reason: models.MaintenanceReasonManualMerge, LeaseToken: secretToken, ProcessID: 77, ProcessStartToken: secretToken, LastError: configPath + " " + secretToken}).Error; err != nil {
		t.Fatal(err)
	}
	code, body = callProactiveStatus(t, database, 1)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%#v", code, body)
	}
	maintenance := body["maintenance"].(map[string]interface{})
	if maintenance["blocker"] != "maintenance_epoch" || maintenance["manual_merge_available"] != false {
		t.Fatalf("maintenance projection=%#v", maintenance)
	}
	if _, exposed := maintenance["capacity_wake"]; exposed {
		t.Fatal("automatic capacity maintenance leaked into public status")
	}
	errorText := maintenance["error"].(string)
	for _, secret := range []string{configPath, secretToken} {
		if strings.Contains(errorText, secret) {
			t.Fatalf("maintenance error leaked %q: %s", secret, errorText)
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{configPath, secretToken} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("status JSON leaked maintenance identity %q: %s", secret, encoded)
		}
	}
}

func TestProactiveStatusSummaryBoundsHistoricalBatchWork(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.Create(&models.Task{ID: 1, TaskType: "rotation", RotationStrategy: "proactive_quota", RotationRemotes: `["drive"]`, RotationQuotaKeys: "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	for id := uint(1); id <= 200; id++ {
		batch := models.RotationQuotaBatch{ID: id, TaskID: 1, DestinationRemote: "drive-" + string(rune(id)), RequestKey: "request-" + string(rune(id)), OwnerToken: "owner-" + string(rune(id)), State: models.BatchStateSucceeded}
		if err := database.Create(&batch).Error; err != nil {
			t.Fatal(err)
		}
		file := models.RotationQuotaBatchFile{BatchID: id, RelativePath: "file-" + string(rune(id)), SnapshotKey: "snapshot-" + string(rune(id)), SizeBytes: int64(id), State: models.BatchFileStateCommitted}
		if err := database.Create(&file).Error; err != nil {
			t.Fatal(err)
		}
	}
	code, body := callProactiveStatusURL(t, database, 1, "summary=true&limit=20")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %#v", code, body)
	}
	if got := len(body["batches"].([]interface{})); got != 20 {
		t.Fatalf("summary returned %d batches, want 20", got)
	}
	queue := body["queue"].(map[string]interface{})
	if got := queue["verified"].(map[string]interface{})["count"]; got != float64(20) {
		t.Fatalf("summary verified count = %v, want 20", got)
	}
}

func TestProactiveStatusSummaryFiltersHistoricalReservations(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.Create(&models.Task{ID: 1, TaskType: "rotation", RotationStrategy: "proactive_quota", RotationRemotes: `["drive"]`, RotationQuotaKeys: `{"drive":"drive-key"}`}).Error; err != nil {
		t.Fatal(err)
	}
	account := models.QuotaAccount{ID: 1, QuotaKey: "drive-key", RemoteName: "drive", BudgetBytes: 1000, WindowSeconds: 3600, Enabled: true}
	if err := database.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	for id := uint(1); id <= 500; id++ {
		state := models.ReservationStateReleased
		if id%2 == 0 {
			state = models.ReservationStateExpired
		}
		reservation := models.QuotaReservation{QuotaAccountID: account.ID, BatchFileID: id, Bytes: 10000, State: state, IdempotencyKey: "historical-" + string(rune(id))}
		if err := database.Create(&reservation).Error; err != nil {
			t.Fatal(err)
		}
	}
	future := time.Now().Add(time.Hour)
	if err := database.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchFileID: 501, Bytes: 11, State: models.ReservationStateCommitted, ExpiresAt: &future, IdempotencyKey: "current-committed"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchFileID: 502, Bytes: 7, State: models.ReservationStateActive, IdempotencyKey: "current-active"}).Error; err != nil {
		t.Fatal(err)
	}

	code, body := callProactiveStatusURL(t, database, 1, "summary=true&limit=20")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %#v", code, body)
	}
	binding := body["accounts"].([]interface{})[0].(map[string]interface{})
	wantRemaining := float64(models.DefaultRotationQuotaLimitBytes - 11 - 7)
	if binding["used_bytes"] != float64(11) || binding["active_reserved_bytes"] != float64(7) || binding["remaining_bytes"] != wantRemaining {
		t.Fatalf("historical reservations affected summary totals: %#v", binding)
	}
}

func TestProactiveStatusIsEmptyWithoutLedgerRows(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.Create(&models.Task{ID: 1, TaskType: "rotation", RotationStrategy: "proactive_quota", RotationRemotes: `["drive"]`, RotationQuotaKeys: `{}`}).Error; err != nil {
		t.Fatal(err)
	}
	code, body := callProactiveStatus(t, database, 1)
	if code != 200 || len(body["batches"].([]interface{})) != 0 || len(body["accounts"].([]interface{})) != 1 {
		t.Fatalf("unexpected empty status: %d %#v", code, body)
	}
}

func TestProactiveStatusRejectsNonProactiveTask(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.Create(&models.Task{ID: 1, TaskType: "normal"}).Error; err != nil {
		t.Fatal(err)
	}
	code, body := callProactiveStatus(t, database, 1)
	if code != 400 || body["error"] != "task is not a proactive quota task" {
		t.Fatalf("unexpected rejection: %d %#v", code, body)
	}
}

func TestProactiveStatusDoesNotRequireCredentials(t *testing.T) {
	database := proactiveStatusTestDB(t)
	if err := database.Create(&models.Task{ID: 1, TaskType: "rotation", RotationStrategy: "proactive_quota", RcloneConfig: "", RotationRemotes: `["drive"]`, RotationQuotaKeys: "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	code, body := callProactiveStatus(t, database, 1)
	if code != 200 || body["task"] == nil {
		t.Fatalf("missing-credentials status failed: %d %#v", code, body)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }

func TestProactiveStatusExposesWindowAnchorAndExhaustionFlag(t *testing.T) {
	database := proactiveStatusTestDB(t)
	configPath := filepath.Join(t.TempDir(), "anchor-rclone.conf")
	if err := os.WriteFile(configPath, []byte("[remote]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	task := models.Task{ID: 1, Enabled: true, TaskType: "rotation", RotationStrategy: "proactive_quota", RcloneConfig: configPath, RemoteDir: "/dest", RotationRemotes: `["remote"]`, RotationQuotaKeys: `{"remote":"anchor-key"}`}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	exhausted := time.Now().Add(-30 * time.Minute)
	account := models.QuotaAccount{QuotaKey: "anchor-key", RemoteName: "remote", ConfigIdentity: configPath, BudgetBytes: 100, WindowSeconds: 86400, Enabled: true, WindowStartedAt: &exhausted}
	if err := database.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	// Drain the budget to zero so "all_accounts_exhausted" reflects the
	// user's intent (no more quota available today).
	expires := time.Now().Add(time.Hour)
	committedAt := time.Now().Add(-time.Minute)
	if err := database.Create(&models.RotationQuotaBatch{TaskID: 1, QuotaAccountID: account.ID, DestinationScope: "scope", State: models.BatchStateSucceeded, RequestKey: "drain", OwnerToken: "o", ReservedBytes: models.DefaultRotationQuotaLimitBytes, StartedAt: &committedAt, FinishedAt: &committedAt}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.QuotaReservation{QuotaAccountID: account.ID, Bytes: models.DefaultRotationQuotaLimitBytes, State: models.ReservationStateCommitted, ExpiresAt: &expires, ReservedAt: &committedAt, IdempotencyKey: "drain"}).Error; err != nil {
		t.Fatal(err)
	}
	code, body := callProactiveStatus(t, database, 1)
	if code != http.StatusOK {
		t.Fatalf("status failed: %d %#v", code, body)
	}
	if body["all_accounts_exhausted"] != true {
		t.Fatalf("expected all_accounts_exhausted=true after full use, got %v", body["all_accounts_exhausted"])
	}
	if body["next_quota_reset_at"] == nil {
		t.Fatalf("expected next_quota_reset_at, got nil")
	}
	accounts := body["accounts"].([]interface{})
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	first := accounts[0].(map[string]interface{})
	if first["window_started_at"] == nil {
		t.Fatalf("expected window_started_at, got nil")
	}
	if first["next_reset_at"] == nil {
		t.Fatalf("expected next_reset_at, got nil")
	}
}

func TestProactiveStatusLeavesNextRecoveryUnavailableWithoutLedgerEvidence(t *testing.T) {
	database := proactiveStatusTestDB(t)
	anchor := time.Now().Add(-time.Hour)
	task := models.Task{ID: 1, Enabled: true, TaskType: "rotation", RotationStrategy: "proactive_quota", RotationRemotes: `["remote"]`, RotationQuotaKeys: `{"remote":"boundary-key"}`}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	account := models.QuotaAccount{QuotaKey: "boundary-key", RemoteName: "remote", BudgetBytes: 100, WindowSeconds: 60, Enabled: true, WindowStartedAt: &anchor}
	if err := database.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	code, body := callProactiveStatus(t, database, 1)
	if code != http.StatusOK {
		t.Fatalf("status failed: %d %#v", code, body)
	}
	accounts := body["accounts"].([]interface{})
	accountStatus := accounts[0].(map[string]interface{})
	if accountStatus["next_reset_at"] != nil || body["next_quota_reset_at"] != nil {
		t.Fatalf("status invented a calendar reset: account=%v top=%v", accountStatus["next_reset_at"], body["next_quota_reset_at"])
	}
}

func TestProactiveStatusTreatsProviderBlockAsExhaustedUntilReset(t *testing.T) {
	database := proactiveStatusTestDB(t)
	task := models.Task{ID: 1, Enabled: true, TaskType: "rotation", RotationStrategy: "proactive_quota", RotationRemotes: `["remote"]`, RotationQuotaKeys: `{"remote":"blocked-key"}`}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	blockedUntil := time.Now().Add(24 * time.Hour)
	anchor := time.Now().Add(-time.Hour)
	account := models.QuotaAccount{QuotaKey: "blocked-key", RemoteName: "remote", BudgetBytes: 100, WindowSeconds: 86400, Enabled: true, ProviderBlockedUntil: &blockedUntil, WindowStartedAt: &anchor}
	if err := database.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	if err := database.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchFileID: 1, Bytes: 25, State: models.ReservationStateCommitted, ExpiresAt: &expires, IdempotencyKey: "blocked-usage"}).Error; err != nil {
		t.Fatal(err)
	}

	code, body := callProactiveStatus(t, database, 1)
	if code != http.StatusOK {
		t.Fatalf("status failed: %d %#v", code, body)
	}
	if body["all_accounts_exhausted"] != true || body["next_quota_reset_at"] == nil {
		t.Fatalf("blocked account did not project an exhausted reset state: %#v", body)
	}
	accountStatus := body["accounts"].([]interface{})[0].(map[string]interface{})
	if accountStatus["account_id"] != float64(account.ID) || accountStatus["availability_state"] != models.QuotaAvailabilityStateProviderBlocked || accountStatus["remaining_bytes"] != float64(0) || accountStatus["next_reset_at"] == nil {
		t.Fatalf("blocked account did not project empty quota and provider reset: %#v", accountStatus)
	}
}

func TestProactiveStatusClearsExpiredLegacyProviderRecovery(t *testing.T) {
	database := proactiveStatusTestDB(t)
	task := models.Task{ID: 1, Enabled: true, TaskType: "rotation", RotationStrategy: "proactive_quota", RotationRemotes: `["remote"]`, RotationQuotaKeys: `{"remote":"recovery-key"}`}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	anchor := time.Now().Add(-time.Hour)
	account := models.QuotaAccount{QuotaKey: "recovery-key", RemoteName: "remote", BudgetBytes: 100, WindowSeconds: 86400, Enabled: true, ProviderBlockedUntil: &past, RecoveryState: models.QuotaRecoveryStateExhausted, RecoveryGeneration: 3, FirstExhaustedAt: &past, WindowStartedAt: &anchor}
	if err := database.Create(&account).Error; err != nil {
		t.Fatal(err)
	}

	code, body := callProactiveStatus(t, database, 1)
	if code != http.StatusOK {
		t.Fatalf("status failed: %d %#v", code, body)
	}
	if body["all_accounts_exhausted"] != false || body["next_quota_reset_at"] != nil {
		t.Fatalf("expired legacy provider recovery remained blocking: %#v", body)
	}
	accountStatus := body["accounts"].([]interface{})[0].(map[string]interface{})
	if accountStatus["recovery_state"] != models.QuotaRecoveryStateAvailable || accountStatus["availability_state"] != models.QuotaAvailabilityStateAvailable || accountStatus["remaining_bytes"] != float64(models.DefaultRotationQuotaLimitBytes) || accountStatus["next_reset_at"] != nil || accountStatus["campaign_cooldown_until"] != nil || accountStatus["provider_blocked_until"] != nil {
		t.Fatalf("expired legacy provider recovery projection contradicted gates: %#v", accountStatus)
	}
}

func TestProactiveStatusReportsNetworkAndLedgerRollingComparison(t *testing.T) {
	database := proactiveStatusTestDB(t)
	task := models.Task{ID: 1, Enabled: true, TaskType: "rotation", RotationStrategy: "proactive_quota", RotationRemotes: `["remote"]`, RotationQuotaKeys: `{"remote":"telemetry-key"}`}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	account := models.QuotaAccount{QuotaKey: "telemetry-key", RemoteName: "remote", BudgetBytes: 1000, WindowSeconds: models.DefaultQuotaWindowSeconds, Enabled: true, QuotaPolicyVersion: models.RollingQuotaPolicyVersion}
	if err := database.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	committedAt := time.Now().Add(-time.Minute)
	if err := database.Create(&models.QuotaReservation{QuotaAccountID: account.ID, Bytes: 25, State: models.ReservationStateCommitted, CommittedAt: &committedAt, IdempotencyKey: "telemetry-ledger"}).Error; err != nil {
		t.Fatal(err)
	}
	sampledAt := time.Now().Add(-time.Second)
	if err := database.Create(&models.NetworkTelemetrySample{SampledAt: sampledAt.Add(-24 * time.Hour), TxBytes: 100, Available: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.NetworkTelemetrySample{SampledAt: sampledAt, TxBytes: 250, Available: true}).Error; err != nil {
		t.Fatal(err)
	}
	code, body := callProactiveStatus(t, database, 1)
	if code != http.StatusOK {
		t.Fatalf("status failed: %d %#v", code, body)
	}
	network, ok := body["network_telemetry"].(map[string]interface{})
	if !ok || network["rolling_24h_tx_bytes"] != float64(150) || network["ledger_committed_bytes"] != float64(25) || network["difference_bytes"] != float64(125) {
		t.Fatalf("network telemetry = %#v", body["network_telemetry"])
	}
	accountStatus := body["accounts"].([]interface{})[0].(map[string]interface{})
	if accountStatus["account_id"] != float64(account.ID) || accountStatus["rolling_usage_bytes"] != float64(25) || accountStatus["unresolved_bytes"] != float64(0) || accountStatus["forecast_3h_bytes"] == nil {
		t.Fatalf("rolling account status = %#v", accountStatus)
	}
}
