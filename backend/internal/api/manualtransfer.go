package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"rclone-manager/internal/auth"
	"rclone-manager/internal/manualtransfer"
	"rclone-manager/internal/models"
)

type manualAnalyzeHTTPRequest struct {
	SourcePath       string                        `json:"source_path"`
	Source           string                        `json:"source"`
	DestinationPath  string                        `json:"destination_path"`
	Destination      string                        `json:"destination"`
	TransferMode     string                        `json:"transfer_mode"`
	ConfigIdentity   string                        `json:"config_identity"`
	ConfigurationID  string                        `json:"configuration_identity"`
	Accounts         []manualtransfer.AccountInput `json:"accounts"`
	AccountIDs       []uint                        `json:"account_ids"`
	IdempotencyKey   string                        `json:"idempotency_key"`
	RunID            *uint                         `json:"run_id"`
	ExpectedRunID    *uint                         `json:"expected_run_id"`
	ExpectedRevision *int64                        `json:"expected_revision"`
}

type manualTaskAccountsHTTPRequest struct {
	AccountIDs       []uint `json:"account_ids"`
	ExpectedRevision int64  `json:"expected_revision"`
	IdempotencyKey   string `json:"idempotency_key"`
}

type manualAccountOption struct {
	AccountID  uint   `json:"account_id"`
	RemoteName string `json:"remote_name"`
}

// UnmarshalJSON keeps the finalized envelope contract while accepting the
// pre-envelope ordered-id payload during the Phase 2 rollout.
func (r *manualTaskAccountsHTTPRequest) UnmarshalJSON(data []byte) error {
	if len(strings.TrimSpace(string(data))) > 0 && strings.TrimSpace(string(data))[0] == '[' {
		return json.Unmarshal(data, &r.AccountIDs)
	}
	type request manualTaskAccountsHTTPRequest
	var decoded request
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = manualTaskAccountsHTTPRequest(decoded)
	return nil
}

type manualAllocateHTTPRequest struct {
	ExpectedRunID          *uint  `json:"expected_run_id"`
	ExpectedRevision       int64  `json:"expected_revision"`
	ExpectedConfigRevision int64  `json:"expected_config_revision"`
	IdempotencyKey         string `json:"idempotency_key"`
}

type manualStartHTTPRequest struct {
	ExpectedRunID          *uint  `json:"expected_run_id"`
	ExpectedRevision       int64  `json:"expected_revision"`
	ExpectedConfigRevision int64  `json:"expected_config_revision"`
	IdempotencyKey         string `json:"idempotency_key"`
}

type manualRunResponse struct {
	Run                manualtransfer.ManualTransferRun  `json:"run"`
	Accounts           []manualtransfer.ManualRunAccount `json:"accounts"`
	AccountsNextCursor string                            `json:"accounts_next_cursor,omitempty"`
	AccountsHasMore    bool                              `json:"accounts_has_more"`
	AccountsPageLimit  int                               `json:"accounts_page_limit"`
	Status             manualtransfer.RunStatus          `json:"status"`
}

func analyzeManualRun(c *gin.Context) {
	if manualTransferService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "manual analyze is unavailable"})
		return
	}
	taskID, err := parseManualID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task id"})
		return
	}
	var body manualAnalyzeHTTPRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, manualtransfer.ManualMaxAnalyzeRequestBytes)
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(body.SourcePath) == "" {
		body.SourcePath = body.Source
	}
	if strings.TrimSpace(body.DestinationPath) == "" {
		body.DestinationPath = body.Destination
	}
	if strings.TrimSpace(body.ConfigIdentity) == "" {
		body.ConfigIdentity = body.ConfigurationID
	}
	if len(body.Accounts) == 0 && len(body.AccountIDs) > 0 {
		body.Accounts = make([]manualtransfer.AccountInput, 0, len(body.AccountIDs))
		for _, accountID := range body.AccountIDs {
			body.Accounts = append(body.Accounts, manualtransfer.AccountInput{AccountID: accountID})
		}
	}
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = body.IdempotencyKey
	}
	expectedRunID := body.ExpectedRunID
	if expectedRunID == nil {
		expectedRunID = body.RunID
	}
	actorIdentity, actorType := manualActor(c)
	result, err := manualTransferService.CreateAnalyzeContext(c.Request.Context(), manualtransfer.AnalyzeRequest{
		TaskID: taskID, SourcePath: body.SourcePath, DestinationPath: body.DestinationPath,
		TransferMode: body.TransferMode, ConfigIdentity: body.ConfigIdentity, Accounts: body.Accounts,
		IdempotencyKey: idempotencyKey, ExpectedRunID: expectedRunID, ExpectedRevision: body.ExpectedRevision,
		ActorIdentity: actorIdentity, ActorType: actorType,
	})
	if err != nil {
		switch {
		case errors.Is(err, manualtransfer.ErrIdempotencyConflict), errors.Is(err, manualtransfer.ErrRevisionConflict), errors.Is(err, manualtransfer.ErrActiveAnalysis), errors.Is(err, manualtransfer.ErrNotManualTask), strings.Contains(err.Error(), "task is active"):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		case strings.Contains(err.Error(), "queue is full"):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "manual analyze queue is full"})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": sanitizeManualAPIError(err.Error())})
		}
		return
	}
	response, err := manualRunResponseFor(result.Run, manualTransferService)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load manual run accounts"})
		return
	}
	c.JSON(http.StatusAccepted, response)
}

func listManualRuns(c *gin.Context) {
	if manualTransferService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "manual analyze is unavailable"})
		return
	}
	taskID, err := parseManualID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task id"})
		return
	}
	if _, err := manualTransferService.GetTask(taskID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		if errors.Is(err, manualtransfer.ErrNotManualTask) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load task"})
		return
	}
	runs, err := manualTransferService.ListRuns(taskID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list manual runs"})
		return
	}
	responses := make([]manualRunResponse, 0, len(runs))
	for _, run := range runs {
		response, responseErr := manualRunResponseFor(run, manualTransferService)
		if responseErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load manual run accounts"})
			return
		}
		responses = append(responses, response)
	}
	c.JSON(http.StatusOK, gin.H{"runs": responses})
}

func listManualTaskAccounts(c *gin.Context) {
	if manualTransferService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "manual account configuration is unavailable"})
		return
	}
	taskID, err := parseManualID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task id"})
		return
	}
	page, err := manualTransferService.ListTaskAccounts(taskID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		if errors.Is(err, manualtransfer.ErrNotManualTask) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list manual task accounts"})
		return
	}
	c.JSON(http.StatusOK, page)
}

func listManualAvailableAccounts(c *gin.Context) {
	var accounts []models.QuotaAccount
	if err := db.Select("id", "remote_name").
		Where("enabled = ? AND quota_key <> '' AND remote_name <> ''", true).
		Order("remote_name ASC, id ASC").
		Find(&accounts).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load manual accounts"})
		return
	}
	options := make([]manualAccountOption, 0, len(accounts))
	for _, account := range accounts {
		options = append(options, manualAccountOption{AccountID: account.ID, RemoteName: account.RemoteName})
	}
	c.JSON(http.StatusOK, gin.H{"accounts": options})
}

func updateManualTaskAccounts(c *gin.Context) {
	if manualTransferService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "manual account configuration is unavailable"})
		return
	}
	taskID, err := parseManualID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task id"})
		return
	}
	var body manualTaskAccountsHTTPRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" {
		key = body.IdempotencyKey
	}
	actorIdentity, actorType := manualActor(c)
	result, err := manualTransferService.UpdateTaskAccounts(c.Request.Context(), manualtransfer.UpdateTaskAccountsRequest{TaskID: taskID, AccountIDs: body.AccountIDs, ExpectedRevision: body.ExpectedRevision, IdempotencyKey: key, ActorIdentity: actorIdentity, ActorType: actorType})
	if err != nil {
		switch {
		case errors.Is(err, manualtransfer.ErrIdempotencyConflict), errors.Is(err, manualtransfer.ErrRevisionConflict), errors.Is(err, manualtransfer.ErrNotManualTask), strings.Contains(err.Error(), "task is active"):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": sanitizeManualAPIError(err.Error())})
		}
		return
	}
	c.JSON(http.StatusOK, result.Page)
}

func getManualRun(c *gin.Context) {
	run, ok := loadManualRun(c)
	if !ok {
		return
	}
	response, err := manualRunResponseFor(run, manualTransferService)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load manual run accounts"})
		return
	}
	c.JSON(http.StatusOK, response)
}

func getManualRunStatus(c *gin.Context) {
	run, ok := loadManualRun(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, manualtransfer.StatusForRun(run))
}

func listManualRunFiles(c *gin.Context) {
	if manualTransferService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "manual analyze is unavailable"})
		return
	}
	runID, err := parseManualID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid manual run id"})
		return
	}
	limit := 100
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive integer"})
			return
		}
	}
	var page manualtransfer.FilePage
	page, err = manualTransferService.ListFilesFiltered(runID, c.Query("cursor"), limit, c.Query("assignment"), c.Query("reason"), c.Query("account_id"))
	if err != nil {
		if errors.Is(err, manualtransfer.ErrRunNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "manual transfer run not found"})
			return
		}
		if errors.Is(err, manualtransfer.ErrNotManualTask) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		if errors.Is(err, manualtransfer.ErrSnapshotUnavailable) {
			c.JSON(http.StatusConflict, gin.H{"error": "manual snapshot is not activated"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": sanitizeManualAPIError(err.Error())})
		return
	}
	c.JSON(http.StatusOK, page)
}

func allocateManualRun(c *gin.Context) {
	if manualTransferService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "manual allocation is unavailable"})
		return
	}
	runID, err := parseManualID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid manual run id"})
		return
	}
	var body manualAllocateHTTPRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" {
		key = body.IdempotencyKey
	}
	actorIdentity, actorType := manualActor(c)
	result, err := manualTransferService.CreateAllocateContext(c.Request.Context(), manualtransfer.AllocateRequest{RunID: runID, ExpectedRunID: body.ExpectedRunID, ExpectedRevision: body.ExpectedRevision, ExpectedConfigRevision: body.ExpectedConfigRevision, IdempotencyKey: key, ActorIdentity: actorIdentity, ActorType: actorType})
	if err != nil {
		switch {
		case errors.Is(err, manualtransfer.ErrIdempotencyConflict), errors.Is(err, manualtransfer.ErrRevisionConflict), errors.Is(err, manualtransfer.ErrNotManualTask), errors.Is(err, manualtransfer.ErrAllocationImmutable), errors.Is(err, manualtransfer.ErrAllocationReanalysisNeeded):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		case errors.Is(err, manualtransfer.ErrRunNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "manual transfer run not found"})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": sanitizeManualAPIError(err.Error())})
		}
		return
	}
	response, err := manualRunResponseFor(result.Run, manualTransferService)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load manual run accounts"})
		return
	}
	c.JSON(http.StatusAccepted, response)
}

func startManualRun(c *gin.Context) {
	if manualTransferService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "manual worker execution is unavailable"})
		return
	}
	runID, err := parseManualID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid manual run id"})
		return
	}
	var body manualStartHTTPRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" {
		key = body.IdempotencyKey
	}
	actorIdentity, actorType := manualActor(c)
	result, err := manualTransferService.StartRun(c.Request.Context(), manualtransfer.StartRequest{RunID: runID, ExpectedRunID: body.ExpectedRunID, ExpectedRevision: body.ExpectedRevision, ExpectedConfigRevision: body.ExpectedConfigRevision, IdempotencyKey: key, ActorIdentity: actorIdentity, ActorType: actorType})
	if err != nil {
		switch {
		case errors.Is(err, manualtransfer.ErrIdempotencyConflict), errors.Is(err, manualtransfer.ErrRevisionConflict), errors.Is(err, manualtransfer.ErrWorkerConflict), errors.Is(err, manualtransfer.ErrManualMoveUnsupported):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		case errors.Is(err, manualtransfer.ErrRunNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "manual transfer run not found"})
		case errors.Is(err, manualtransfer.ErrWorkerUnavailable):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": sanitizeManualAPIError(err.Error())})
		}
		return
	}
	response, err := manualRunResponseFor(result.Run, manualTransferService)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load manual run accounts"})
		return
	}
	responseData := gin.H{"run": response.Run, "accounts": response.Accounts, "accounts_next_cursor": response.AccountsNextCursor, "accounts_has_more": response.AccountsHasMore, "accounts_page_limit": response.AccountsPageLimit, "status": response.Status, "worker_ids": result.WorkerIDs, "existing": result.Existing}
	c.JSON(http.StatusAccepted, responseData)
}

func listManualRunWorkers(c *gin.Context) {
	if manualTransferService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "manual worker execution is unavailable"})
		return
	}
	runID, err := parseManualID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid manual run id"})
		return
	}
	workers, err := manualTransferService.GetRunWorkers(runID)
	if err != nil {
		writeManualWorkerError(c, err)
		return
	}
	run, err := manualTransferService.GetRun(runID)
	if err != nil {
		writeManualWorkerError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"run_id": runID, "run_state": run.State, "workers": workers})
}

func getManualWorker(c *gin.Context) {
	if manualTransferService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "manual worker execution is unavailable"})
		return
	}
	workerID, err := parseManualID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid manual worker id"})
		return
	}
	detail, err := manualTransferService.GetWorker(workerID)
	if err != nil {
		writeManualWorkerError(c, err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

func cancelManualWorker(c *gin.Context) {
	if manualTransferService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "manual worker execution is unavailable"})
		return
	}
	workerID, err := parseManualID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid manual worker id"})
		return
	}
	actorIdentity, actorType := manualActor(c)
	detail, err := manualTransferService.CancelWorker(c.Request.Context(), workerID, actorIdentity, actorType)
	if err != nil {
		writeManualWorkerError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, detail)
}

func retryManualWorker(c *gin.Context) {
	if manualTransferService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "manual worker execution is unavailable"})
		return
	}
	workerID, err := parseManualID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid manual worker id"})
		return
	}
	actorIdentity, actorType := manualActor(c)
	detail, err := manualTransferService.RetryWorker(c.Request.Context(), workerID, actorIdentity, actorType)
	if err != nil {
		writeManualWorkerError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, detail)
}

func getManualWorkerLogs(c *gin.Context) {
	if manualTransferService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "manual worker execution is unavailable"})
		return
	}
	workerID, err := parseManualID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid manual worker id"})
		return
	}
	offset := int64(0)
	if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
		offset, err = strconv.ParseInt(raw, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "offset must be a non-negative integer"})
			return
		}
	}
	limit := int64(64 << 10)
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		limit, err = strconv.ParseInt(raw, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive integer"})
			return
		}
	}
	page, err := manualTransferService.GetWorkerLogs(workerID, offset, limit)
	if err != nil {
		writeManualWorkerError(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

func writeManualWorkerError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, manualtransfer.ErrWorkerNotFound), errors.Is(err, manualtransfer.ErrRunNotFound), errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "manual worker not found"})
	case errors.Is(err, manualtransfer.ErrNotManualTask):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, manualtransfer.ErrWorkerConflict), errors.Is(err, manualtransfer.ErrManualMoveUnsupported), errors.Is(err, manualtransfer.ErrWorkerNoIncomplete):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": sanitizeManualAPIError(err.Error())})
	}
}

func getManualRunAllocationSummary(c *gin.Context) {
	if manualTransferService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "manual allocation is unavailable"})
		return
	}
	runID, err := parseManualID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid manual run id"})
		return
	}
	summary, err := manualTransferService.GetAllocationSummary(runID)
	if err != nil {
		if errors.Is(err, manualtransfer.ErrRunNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "manual transfer run not found"})
			return
		}
		if errors.Is(err, manualtransfer.ErrNotManualTask) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusConflict, gin.H{"error": sanitizeManualAPIError(err.Error())})
		return
	}
	c.JSON(http.StatusOK, summary)
}

func listManualRunAccounts(c *gin.Context) {
	if manualTransferService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "manual analyze is unavailable"})
		return
	}
	runID, err := parseManualID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid manual run id"})
		return
	}
	limit, err := parseManualPageLimit(c.Query("limit"), manualtransfer.ManualAccountPageLimit)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	page, err := manualTransferService.ListRunAccounts(runID, c.Query("cursor"), limit)
	if err != nil {
		if errors.Is(err, manualtransfer.ErrRunNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "manual transfer run not found"})
			return
		}
		if errors.Is(err, manualtransfer.ErrNotManualTask) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": sanitizeManualAPIError(err.Error())})
		return
	}
	c.JSON(http.StatusOK, page)
}

func loadManualRun(c *gin.Context) (manualtransfer.ManualTransferRun, bool) {
	if manualTransferService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "manual analyze is unavailable"})
		return manualtransfer.ManualTransferRun{}, false
	}
	runID, err := parseManualID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid manual run id"})
		return manualtransfer.ManualTransferRun{}, false
	}
	run, err := manualTransferService.GetRun(runID)
	if err != nil {
		if errors.Is(err, manualtransfer.ErrRunNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "manual transfer run not found"})
		} else if errors.Is(err, manualtransfer.ErrNotManualTask) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load manual transfer run"})
		}
		return manualtransfer.ManualTransferRun{}, false
	}
	return run, true
}

func manualRunResponseFor(run manualtransfer.ManualTransferRun, service *manualtransfer.Service) (manualRunResponse, error) {
	if err := manualtransfer.ValidatePublicRun(run); err != nil {
		return manualRunResponse{}, err
	}
	page, err := service.ListRunAccounts(run.ID, "", manualtransfer.ManualAccountPageLimit)
	if err != nil {
		return manualRunResponse{}, err
	}
	return manualRunResponse{Run: run, Accounts: page.Accounts, AccountsNextCursor: page.NextCursor, AccountsHasMore: page.HasMore, AccountsPageLimit: page.Limit, Status: manualtransfer.StatusForRun(run)}, nil
}

func parseManualPageLimit(raw string, maximum int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return maximum, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > maximum {
		return 0, fmt.Errorf("limit must be between 1 and %d", maximum)
	}
	return limit, nil
}

func manualActor(c *gin.Context) (string, string) {
	if value, ok := c.Get("manual_transfer_actor_identity"); ok {
		identity, _ := value.(string)
		actorType, _ := c.Get("manual_transfer_actor_type")
		kind, _ := actorType.(string)
		return identity, kind
	}
	if cfgGlobal != nil && cfgGlobal.APIToken != "" && c.Query("token") == cfgGlobal.APIToken {
		return "configured-api-token", "privileged_token"
	}
	authorization := strings.TrimSpace(c.GetHeader("Authorization"))
	if strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
		claims, ok := auth.ClaimsForToken(strings.TrimSpace(authorization[len("Bearer "):]))
		if ok {
			return claims.Username, "admin_session"
		}
	}
	return "administrator", "admin_session"
}

func parseManualID(raw string) (uint, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 32)
	if err != nil || value == 0 {
		return 0, errors.New("invalid id")
	}
	return uint(value), nil
}

func sanitizeManualAPIError(message string) string {
	return manualtransfer.SanitizeMessage(message)
}
