package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"rclone-manager/internal/config"
	"rclone-manager/internal/models"
)

func TestManualQuotaResetRequiresConfirmationAndPreservesInFlight(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Task{}, &models.QuotaAccount{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &models.QuotaReservation{}, &models.QuotaManualResetEvent{}); err != nil {
		t.Fatal(err)
	}
	task := models.Task{ID: 1, Enabled: true, TaskType: "rotation", RotationStrategy: "proactive_quota", RotationRemotes: `["remote"]`, RotationQuotaKeys: `{"remote":"reset-key"}`, RcloneConfig: "/missing/config"}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Hour)
	account := models.QuotaAccount{QuotaKey: "reset-key", RemoteName: "remote", BudgetBytes: 100, WindowSeconds: models.DefaultQuotaWindowSeconds, Enabled: true, CampaignCooldownUntil: &now}
	if err := database.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	for i, state := range []string{models.ReservationStateCommitted, models.ReservationStateHeld, models.ReservationStateActive, models.ReservationStateUnknown} {
		reservation := models.QuotaReservation{QuotaAccountID: account.ID, BatchFileID: uint(i + 1), Bytes: 5, State: state, IdempotencyKey: "reset-api-" + string(rune('a'+i))}
		if state == models.ReservationStateCommitted {
			committedAt := time.Now().Add(-time.Minute)
			reservation.CommittedAt = &committedAt
		}
		if err := database.Create(&reservation).Error; err != nil {
			t.Fatal(err)
		}
	}
	previousDB, previousDispatcher, previousConfig := db, proactiveDispatcher, cfgGlobal
	db, proactiveDispatcher, cfgGlobal = database, nil, &config.Config{}
	t.Cleanup(func() { db, proactiveDispatcher, cfgGlobal = previousDB, previousDispatcher, previousConfig })

	call := func(payload string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest(http.MethodPost, "/api/tasks/1/quota-accounts/1/manual-reset", bytes.NewBufferString(payload))
		context.Params = gin.Params{{Key: "id", Value: "1"}, {Key: "accountID", Value: "1"}}
		context.Set("quota_reset_actor_identity", "admin-test")
		context.Set("quota_reset_actor_type", models.QuotaManualResetActorAdminSession)
		manualQuotaReset(context)
		return recorder
	}
	if response := call(`{}`); response.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed reset status=%d body=%s", response.Code, response.Body.String())
	}
	response := call(`{"confirm":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("confirmed reset status=%d body=%s", response.Code, response.Body.String())
	}
	var stored []models.QuotaReservation
	if err := database.Where("quota_account_id = ?", account.ID).Order("id").Find(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored[0].State != models.ReservationStateExpired || stored[1].State != models.ReservationStateHeld || stored[2].State != models.ReservationStateActive || stored[3].State != models.ReservationStateUnknown {
		t.Fatalf("reset changed reservation states: %#v", stored)
	}
	var audit models.QuotaAccount
	if err := database.First(&audit, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if audit.LastManualResetAt == nil || audit.CampaignCooldownUntil != nil {
		t.Fatalf("reset audit/state missing: %#v", audit)
	}
	var events []models.QuotaManualResetEvent
	if err := database.Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].TaskID != task.ID || events[0].QuotaAccountID != account.ID || events[0].ActorIdentity != "admin-test" || events[0].ActorType != models.QuotaManualResetActorAdminSession || events[0].Outcome != models.QuotaManualResetOutcomeSucceeded || events[0].EffectiveAt == nil {
		t.Fatalf("reset audit events = %#v", events)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
}
