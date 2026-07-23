package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"rclone-manager/internal/logger"
	"rclone-manager/internal/models"
	"rclone-manager/internal/proactive"
	"rclone-manager/internal/taskdispatch"
)

func startProactiveManualMerge(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 || proactiveDispatcher == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid proactive task id"})
		return
	}
	dispatcher := proactiveDispatcher
	if taskRunner == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "task dispatcher is unavailable"})
		return
	}
	var task models.Task
	var epoch models.DestinationScopeMaintenance
	err = taskRunner.WithTaskExclusive(c.Request.Context(), uint(id), func(gatedTask *models.Task) error {
		var claimErr error
		task, epoch, claimErr = dispatcher.ClaimManualMerge(gatedTask.ID)
		return claimErr
	})
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, gorm.ErrRecordNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, proactive.ErrManualMergeConflict) || errors.Is(err, proactive.ErrCoordinatorConflict) || errors.Is(err, taskdispatch.ErrTaskActive) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": redactProactiveError(err.Error(), task)})
		return
	}
	go func() {
		if err := dispatcher.RunClaimedManualMerge(context.Background(), task, epoch); err != nil {
			_ = logger.WriteLog("system.log", "proactive manual merge completion failed; durable epoch/task evidence was recorded")
		}
	}()
	c.JSON(http.StatusAccepted, gin.H{"epoch_id": epoch.ID, "reason": epoch.Reason, "state": epoch.State, "dedupe_state": epoch.DedupeState})
}

func closeProactiveUnknownMaintenance(c *gin.Context) {
	epochID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || epochID == 0 || proactiveDispatcher == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid maintenance epoch id"})
		return
	}
	var request struct {
		Reason           string `json:"reason" binding:"required"`
		ExpectedState    string `json:"expected_state" binding:"required"`
		ExpectedRevision int64  `json:"expected_revision" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.Reason) == "" || request.ExpectedState != models.DedupeStateUnknown || request.ExpectedRevision <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid unknown maintenance close request"})
		return
	}
	if err := proactiveDispatcher.CloseUnknownMaintenance(uint(epochID), request.Reason, request.ExpectedRevision); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, proactive.ErrUnknownMaintenance) {
			status = http.StatusConflict
		} else if errors.Is(err, gorm.ErrRecordNotFound) {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": redactProactiveError(err.Error(), models.Task{})})
		return
	}
	c.JSON(http.StatusOK, gin.H{"epoch_id": epochID, "state": models.MaintenanceStateClosed, "result": models.DedupeStateFailed})
}
