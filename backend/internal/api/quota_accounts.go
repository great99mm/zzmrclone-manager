package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
)

type quotaAccountRequest struct {
	RemoteName    string `json:"remote_name"`
	BudgetBytes   *int64 `json:"budget_bytes"`
	WindowSeconds *int   `json:"window_seconds"`
	Enabled       *bool  `json:"enabled"`
}

type quotaAccountResponse struct {
	ID            uint   `json:"id"`
	RemoteName    string `json:"remote_name"`
	BudgetBytes   int64  `json:"budget_bytes"`
	WindowSeconds int    `json:"window_seconds"`
	Enabled       bool   `json:"enabled"`
}

func listQuotaAccounts(c *gin.Context) {
	var accounts []models.QuotaAccount
	if err := db.Order("remote_name ASC, id ASC").Find(&accounts).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load quota accounts"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"accounts": quotaAccountResponses(accounts)})
}

func createQuotaAccount(c *gin.Context) {
	var request quotaAccountRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quota account request"})
		return
	}

	remote, err := configuredQuotaAccountRemote(request.RemoteName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	budget, window, enabled, err := quotaAccountSettings(request, models.DefaultRotationQuotaLimitBytes, models.DefaultQuotaWindowSeconds, true)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	configIdentity := quotaAccountConfigIdentity()
	account := models.QuotaAccount{
		QuotaKey:                    models.DefaultRotationQuotaKey(configIdentity, remote),
		RemoteName:                  remote,
		ConfigIdentity:              configIdentity,
		BudgetBytes:                 budget,
		WindowSeconds:               window,
		FixedWindowMigrationVersion: models.FixedWindowMigrationVersion,
		QuotaPolicyVersion:          models.RollingQuotaPolicyVersion,
		RecoveryState:               models.QuotaRecoveryStateAvailable,
		Enabled:                     enabled,
	}
	if err := db.Create(&account).Error; err != nil {
		if isQuotaAccountUniqueError(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "a trusted account is already configured for this remote"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create quota account"})
		return
	}
	c.JSON(http.StatusCreated, quotaAccountResponseFor(account))
}

func updateQuotaAccount(c *gin.Context) {
	id, ok := quotaAccountID(c)
	if !ok {
		return
	}
	var request quotaAccountRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quota account request"})
		return
	}

	var account models.QuotaAccount
	if err := db.First(&account, id).Error; err != nil {
		quotaAccountNotFound(c, err)
		return
	}
	budget, window, enabled, err := quotaAccountSettings(request, account.BudgetBytes, account.WindowSeconds, account.Enabled)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := db.Model(&account).Updates(map[string]interface{}{
		"budget_bytes":   budget,
		"window_seconds": window,
		"enabled":        enabled,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update quota account"})
		return
	}
	account.BudgetBytes = budget
	account.WindowSeconds = window
	account.Enabled = enabled
	c.JSON(http.StatusOK, quotaAccountResponseFor(account))
}

func quotaAccountID(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quota account id"})
		return 0, false
	}
	return uint(id), true
}

func quotaAccountNotFound(c *gin.Context, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "quota account not found"})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load quota account"})
}

func quotaAccountSettings(request quotaAccountRequest, currentBudget int64, currentWindow int, currentEnabled bool) (int64, int, bool, error) {
	if request.BudgetBytes != nil {
		currentBudget = *request.BudgetBytes
	}
	if request.WindowSeconds != nil {
		currentWindow = *request.WindowSeconds
	}
	if request.Enabled != nil {
		currentEnabled = *request.Enabled
	}
	if currentBudget <= 0 {
		return 0, 0, false, errors.New("quota budget must be greater than zero")
	}
	if currentWindow <= 0 {
		return 0, 0, false, errors.New("quota window must be greater than zero")
	}
	return currentBudget, currentWindow, currentEnabled, nil
}

func configuredQuotaAccountRemote(value string) (string, error) {
	remote := strings.TrimSpace(value)
	if remote == "" {
		return "", errors.New("remote name is required")
	}
	content, err := os.ReadFile(quotaAccountConfigIdentity())
	if err != nil {
		return "", errors.New("rclone configuration is unavailable")
	}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") && strings.TrimSpace(line[1:len(line)-1]) == remote {
			return remote, nil
		}
	}
	return "", errors.New("remote is not configured in rclone.conf")
}

func quotaAccountConfigIdentity() string {
	if cfgGlobal != nil && strings.TrimSpace(cfgGlobal.RcloneConfig) != "" {
		return filepath.Clean(cfgGlobal.RcloneConfig)
	}
	return models.DefaultRcloneConfigPath
}

func quotaAccountResponses(accounts []models.QuotaAccount) []quotaAccountResponse {
	responses := make([]quotaAccountResponse, 0, len(accounts))
	for _, account := range accounts {
		responses = append(responses, quotaAccountResponseFor(account))
	}
	return responses
}

func quotaAccountResponseFor(account models.QuotaAccount) quotaAccountResponse {
	return quotaAccountResponse{
		ID:            account.ID,
		RemoteName:    account.RemoteName,
		BudgetBytes:   account.BudgetBytes,
		WindowSeconds: account.WindowSeconds,
		Enabled:       account.Enabled,
	}
}

func isQuotaAccountUniqueError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") || strings.Contains(message, "duplicate key")
}
