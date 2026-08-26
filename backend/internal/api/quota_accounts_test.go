package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"rclone-manager/internal/config"
	"rclone-manager/internal/models"
)

func TestQuotaAccountManagement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.QuotaAccount{}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(configPath, []byte("[drive-a]\ntype = drive\n[drive-b]\ntype = drive\n"), 0600); err != nil {
		t.Fatal(err)
	}
	previousDB, previousConfig := db, cfgGlobal
	db, cfgGlobal = database, &config.Config{RcloneConfig: configPath}
	t.Cleanup(func() { db, cfgGlobal = previousDB, previousConfig })

	call := func(handler gin.HandlerFunc, method, path, body string, params gin.Params) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		context.Params = params
		handler(context)
		return recorder
	}

	created := call(createQuotaAccount, http.MethodPost, "/api/quota-accounts", `{"remote_name":"drive-a","budget_bytes":1000,"window_seconds":3600}`, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var account quotaAccountResponse
	if err := json.Unmarshal(created.Body.Bytes(), &account); err != nil {
		t.Fatal(err)
	}
	if account.ID == 0 || account.RemoteName != "drive-a" || account.BudgetBytes != 1000 || account.WindowSeconds != 3600 || !account.Enabled {
		t.Fatalf("created account = %#v", account)
	}
	var stored models.QuotaAccount
	if err := database.First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.QuotaKey != models.DefaultRotationQuotaKey(configPath, "drive-a") || stored.ConfigIdentity != configPath {
		t.Fatalf("stored identity = %#v", stored)
	}

	updated := call(updateQuotaAccount, http.MethodPut, "/api/quota-accounts/1", `{"remote_name":"drive-b","budget_bytes":2000,"enabled":false}`, gin.Params{{Key: "id", Value: "1"}})
	if updated.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updated.Code, updated.Body.String())
	}
	if err := database.First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RemoteName != "drive-b" || stored.QuotaKey != models.DefaultRotationQuotaKey(configPath, "drive-b") || stored.BudgetBytes != 2000 || stored.WindowSeconds != 3600 || stored.Enabled {
		t.Fatalf("updated account = %#v", stored)
	}
	invalidUpdate := call(updateQuotaAccount, http.MethodPut, "/api/quota-accounts/1", `{"remote_name":"missing"}`, gin.Params{{Key: "id", Value: "1"}})
	if invalidUpdate.Code != http.StatusBadRequest {
		t.Fatalf("invalid update status=%d body=%s", invalidUpdate.Code, invalidUpdate.Body.String())
	}

	invalid := call(createQuotaAccount, http.MethodPost, "/api/quota-accounts", `{"remote_name":"missing"}`, nil)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid remote status=%d body=%s", invalid.Code, invalid.Body.String())
	}

	listed := call(listQuotaAccounts, http.MethodGet, "/api/quota-accounts", "", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}
	var page struct {
		Accounts []quotaAccountResponse `json:"accounts"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Accounts) != 1 || page.Accounts[0].RemoteName != "drive-b" || page.Accounts[0].Enabled {
		t.Fatalf("listed accounts = %#v", page.Accounts)
	}

	router := gin.New()
	router.GET("/api/quota-accounts", requireAdminStrictTokenOrSession, listQuotaAccounts)
	unauthorized := httptest.NewRecorder()
	router.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/quota-accounts", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
}
