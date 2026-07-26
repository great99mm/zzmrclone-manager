package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/proactive"
	"rclone-manager/internal/quota"
)

type manualQuotaResetRequest struct {
	Confirm      bool            `json:"confirm"`
	Confirmed    bool            `json:"confirmed"`
	Confirmation json.RawMessage `json:"confirmation"`
}

func (r manualQuotaResetRequest) confirmed() bool {
	if r.Confirm || r.Confirmed {
		return true
	}
	if len(r.Confirmation) == 0 {
		return false
	}
	var value bool
	if json.Unmarshal(r.Confirmation, &value) == nil && value {
		return true
	}
	var text string
	return json.Unmarshal(r.Confirmation, &text) == nil && strings.EqualFold(strings.TrimSpace(text), "RESET")
}

func manualQuotaReset(c *gin.Context) {
	taskID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || taskID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task id"})
		return
	}
	accountID, err := strconv.ParseUint(c.Param("accountID"), 10, 32)
	if err != nil || accountID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quota account id"})
		return
	}
	var request manualQuotaResetRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid manual reset request"})
		return
	}
	if !request.confirmed() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "manual reset confirmation is required"})
		return
	}
	var task models.Task
	if err := db.First(&task, uint(taskID)).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load task"})
		return
	}
	if task.TaskType != "rotation" || task.RotationStrategy != "proactive_quota" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "task is not a proactive quota task"})
		return
	}
	var account models.QuotaAccount
	if err := db.First(&account, uint(accountID)).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "quota account not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load quota account"})
		return
	}
	resolved := statusConfigIdentity(task)
	keys, err := proactive.CompleteQuotaKeysFromAccounts(task, []models.QuotaAccount{account}, resolved)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid proactive quota bindings"})
		return
	}
	bound := false
	for _, key := range keys {
		if key == account.QuotaKey {
			bound = true
			break
		}
	}
	if !bound {
		c.JSON(http.StatusNotFound, gin.H{"error": "quota account is not bound to this task"})
		return
	}
	actorIdentityValue, identityOK := c.Get("quota_reset_actor_identity")
	actorTypeValue, typeOK := c.Get("quota_reset_actor_type")
	actorIdentity, identityString := actorIdentityValue.(string)
	actorType, typeString := actorTypeValue.(string)
	if !identityOK || !typeOK || !identityString || !typeString || actorIdentity == "" || actorType == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "administrator reset authorization is required"})
		return
	}
	requestedAt := time.Now().UTC()
	reset, err := quota.ManualResetWithAudit(db, quota.ManualResetRequest{
		TaskID: task.ID, AccountID: account.ID,
		ActorIdentity: actorIdentity, ActorType: actorType,
		RequestedAt: requestedAt, RequestCutoff: requestedAt,
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "quota account not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reset quota account"})
		return
	}
	if proactiveDispatcher != nil {
		if err := proactiveDispatcher.WakeQuotaAccounts([]uint{account.ID}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "quota reset committed but tasks could not be woken"})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"account_id": account.ID, "last_manual_reset_at": reset.EffectiveAt, "request_cutoff": reset.RequestCutoff, "expired_reservations": reset.ExpiredRows})
}
