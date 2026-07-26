package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/proactive"
)

type proactiveMoveResolutionRequest struct {
	BatchID           uint   `json:"batch_id" binding:"required"`
	FileID            uint   `json:"file_id" binding:"required"`
	Action            string `json:"action" binding:"required"`
	ExpectedState     string `json:"expected_state" binding:"required"`
	ExpectedUpdatedAt string `json:"expected_updated_at" binding:"required"`
}

func resolveProactiveMove(c *gin.Context) {
	taskID, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || taskID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task id"})
		return
	}
	var request proactiveMoveResolutionRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid move resolution request"})
		return
	}
	if request.Action != "accept_moved" && request.Action != "restore_and_release" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid move resolution action"})
		return
	}
	expected, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(request.ExpectedUpdatedAt))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid expected_updated_at"})
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
	var inspector proactive.ProcessInspector
	if proactiveDispatcher != nil {
		inspector = proactiveDispatcher.Inspector
	}
	result, err := proactive.ResolveUnknownMoveFile(db, proactive.MoveResolutionRequest{TaskID: uint(taskID), BatchID: request.BatchID, FileID: request.FileID, Action: request.Action, ExpectedState: request.ExpectedState, ExpectedUpdatedAt: expected, Inspector: inspector})
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, proactive.ErrMoveResolutionConflict):
			status = http.StatusConflict
		case errors.Is(err, proactive.ErrMoveResolutionEvidence):
			status = http.StatusUnprocessableEntity
		case errors.Is(err, gorm.ErrRecordNotFound):
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": redactProactiveError(err.Error(), models.Task{})})
		return
	}
	if proactiveDispatcher != nil {
		if err := proactiveDispatcher.WakeQuotaAccounts([]uint{result.QuotaAccountID}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "move resolution committed but pending quota tasks could not be woken"})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"batch_id": result.BatchID, "file_id": result.FileID, "state": result.State, "action": request.Action})
}
