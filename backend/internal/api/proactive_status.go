package api

import (
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/proactive"
	"rclone-manager/internal/quota"
)

const proactiveStatusMaxBatches = 100

var (
	proactiveTokenPattern = regexp.MustCompile(`[0-9a-fA-F]{48}`)
	proactivePathPattern  = regexp.MustCompile(`(^|[\s:])/(?:[^\s]+)`)
)

type proactiveBytes struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

type proactiveAccountStatus struct {
	RemoteName           string     `json:"remote_name"`
	QuotaKey             string     `json:"quota_key"`
	BudgetBytes          *int64     `json:"budget_bytes"`
	WindowSeconds        *int       `json:"window_seconds"`
	UsedBytes            *int64     `json:"used_bytes"`
	ActiveReservedBytes  *int64     `json:"active_reserved_bytes"`
	RemainingBytes       *int64     `json:"remaining_bytes"`
	ProviderBlockedUntil *time.Time `json:"provider_blocked_until"`
	Enabled              *bool      `json:"enabled"`
}

type proactiveBatchStatus struct {
	ID                        uint                      `json:"id"`
	State                     string                    `json:"state"`
	TransferMode              string                    `json:"transfer_mode"`
	Account                   string                    `json:"account"`
	Remote                    string                    `json:"remote"`
	ReservedBytes             int64                     `json:"reserved_bytes"`
	LeaseUntil                *time.Time                `json:"lease_until"`
	Process                   proactiveProcessStatus    `json:"process"`
	Error                     string                    `json:"error"`
	CompletionEvidence        string                    `json:"completion_evidence"`
	CompletionEvidenceVersion int                       `json:"completion_evidence_version"`
	ResolutionRequired        bool                      `json:"resolution_required"`
	ResolutionActions         []string                  `json:"resolution_actions,omitempty"`
	ResolutionItems           []proactiveResolutionItem `json:"resolution_items,omitempty"`
	StartedAt                 *time.Time                `json:"started_at"`
	FinishedAt                *time.Time                `json:"finished_at"`
	CreatedAt                 time.Time                 `json:"created_at"`
	UpdatedAt                 time.Time                 `json:"updated_at"`
	FileCounts                map[string]proactiveBytes `json:"file_counts"`
}

type proactiveResolutionItem struct {
	BatchID           uint      `json:"batch_id"`
	FileID            uint      `json:"file_id"`
	ExpectedState     string    `json:"expected_state"`
	ExpectedUpdatedAt time.Time `json:"expected_updated_at"`
	Actions           []string  `json:"actions"`
}

type proactiveProcessStatus struct {
	Active    bool       `json:"active"`
	StartedAt *time.Time `json:"started_at"`
	ExitCode  *int       `json:"exit_code"`
}

type proactiveQueueStatus struct {
	Pending   proactiveBytes `json:"pending"`
	Planned   proactiveBytes `json:"planned"`
	Executing proactiveBytes `json:"executing"`
	Verified  proactiveBytes `json:"verified"`
	Failed    proactiveBytes `json:"failed"`
}

type proactiveFileAggregate struct {
	BatchID uint
	State   string
	Count   int
	Bytes   int64
}

type proactiveQueueAggregate struct {
	BatchState string
	Count      int
	Bytes      int64
}

type proactiveReservationAggregate struct {
	QuotaAccountID uint
	State          string
	Bytes          int64
	ExpiresAt      *time.Time
}

func getProactiveStatus(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task id"})
		return
	}

	var task models.Task
	if err := db.First(&task, uint(id)).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load task status"})
		return
	}
	if task.TaskType != "rotation" || task.RotationStrategy != "proactive_quota" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "task is not a proactive quota task"})
		return
	}

	remotes := models.ParseRotationRemotes(task.RotationRemotes)
	explicitKeys, parseErr := models.ParseRotationQuotaKeys(task.RotationQuotaKeys)
	if parseErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid proactive quota bindings"})
		return
	}

	keys := make([]string, 0, len(explicitKeys))
	for _, key := range explicitKeys {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}
	var accounts []models.QuotaAccount
	accountQuery := db.Where("remote_name IN ?", remotes)
	if len(keys) > 0 {
		accountQuery = accountQuery.Or("quota_key IN ?", keys)
	}
	if err := accountQuery.Find(&accounts).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load quota accounts"})
		return
	}
	resolvedIdentity := statusConfigIdentity(task)
	quotaKeys, err := proactive.CompleteQuotaKeysFromAccounts(task, accounts, resolvedIdentity)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid proactive quota bindings"})
		return
	}
	accountsByKey := make(map[string]models.QuotaAccount, len(accounts))
	for _, account := range accounts {
		accountsByKey[account.QuotaKey] = account
	}

	var batches []models.RotationQuotaBatch
	summary := strings.EqualFold(c.Query("summary"), "true")
	limit := proactiveStatusMaxBatches
	if requested, parseErr := strconv.Atoi(c.DefaultQuery("limit", strconv.Itoa(limit))); parseErr == nil && requested > 0 && requested < limit {
		limit = requested
	}
	if err := db.Where("task_id = ?", task.ID).Order("id DESC").Limit(limit).Find(&batches).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load proactive batches"})
		return
	}
	var redactionBatches []models.RotationQuotaBatch
	if !summary {
		if err := db.Select("rclone_config_path, source_root, destination_path, manifest_path, owner_token, lease_token, process_start_token, request_key, request_fingerprint").Where("task_id = ?", task.ID).Find(&redactionBatches).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load proactive error context"})
			return
		}
	}

	batchIDs := make([]uint, len(batches))
	for i := range batches {
		batchIDs[i] = batches[i].ID
	}
	fileStats := make(map[uint]map[string]proactiveBytes, len(batchIDs))
	if len(batchIDs) > 0 {
		var aggregates []proactiveFileAggregate
		if err := db.Model(&models.RotationQuotaBatchFile{}).
			Select("batch_id, state, COUNT(*) AS count, COALESCE(SUM(size_bytes), 0) AS bytes").
			Where("batch_id IN ?", batchIDs).Group("batch_id, state").Find(&aggregates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load proactive file totals"})
			return
		}
		for _, aggregate := range aggregates {
			if fileStats[aggregate.BatchID] == nil {
				fileStats[aggregate.BatchID] = make(map[string]proactiveBytes)
			}
			fileStats[aggregate.BatchID][aggregate.State] = proactiveBytes{Count: aggregate.Count, Bytes: aggregate.Bytes}
		}
	}

	accountIDs := make([]uint, 0, len(accountsByKey))
	for _, account := range accountsByKey {
		accountIDs = append(accountIDs, account.ID)
	}
	used := make(map[uint]int64)
	reserved := make(map[uint]int64)
	if len(accountIDs) > 0 {
		var rows []proactiveReservationAggregate
		reservationQuery := db.Model(&models.QuotaReservation{}).Select("quota_account_id, state, bytes, expires_at").Where("quota_account_id IN ?", accountIDs)
		if summary {
			// Match quota.accountUsage without scanning historical releases and
			// expirations: committed usage is current only when unexpired;
			// held/active/unknown reservations remain current by ledger contract.
			reservationQuery = reservationQuery.Where("state IN ? OR (state = ? AND (expires_at IS NULL OR expires_at > ?))", []string{models.ReservationStateHeld, models.ReservationStateActive, models.ReservationStateUnknown}, models.ReservationStateCommitted, time.Now())
		}
		if err := reservationQuery.Find(&rows).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load quota ledger"})
			return
		}
		for _, row := range rows {
			if row.Bytes < 0 {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "quota ledger contains invalid bytes"})
				return
			}
			switch row.State {
			case models.ReservationStateCommitted:
				if row.ExpiresAt == nil || row.ExpiresAt.After(time.Now()) {
					used[row.QuotaAccountID] += row.Bytes
				}
			case models.ReservationStateHeld, models.ReservationStateActive, models.ReservationStateUnknown:
				reserved[row.QuotaAccountID] += row.Bytes
			case models.ReservationStateReleased, models.ReservationStateExpired:
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "quota ledger contains invalid state"})
				return
			}
		}
	}

	bindings := make([]proactiveAccountStatus, 0, len(remotes))
	for _, remote := range remotes {
		key := quotaKeys[remote]
		binding := proactiveAccountStatus{RemoteName: remote, QuotaKey: key}
		if account, ok := accountsByKey[key]; ok {
			budget, window, enabled := account.BudgetBytes, account.WindowSeconds, account.Enabled
			u, r := used[account.ID], reserved[account.ID]
			remaining := budget - u - r
			binding.BudgetBytes, binding.WindowSeconds, binding.Enabled = &budget, &window, &enabled
			binding.UsedBytes, binding.ActiveReservedBytes, binding.RemainingBytes = &u, &r, &remaining
			binding.ProviderBlockedUntil = account.ProviderBlockedUntil
		}
		bindings = append(bindings, binding)
	}

	// Queue categories are mutually exclusive batch-lifecycle buckets. A
	// reserved/held file is pending, a planned/held file is planned, and the
	// same file is never added to both. Canceled and expired batches are
	// intentionally excluded; their files are no longer queued work.
	queue := proactiveQueueStatus{}
	var queueAggregates []proactiveQueueAggregate
	if summary {
		for _, batch := range batches {
			addProactiveQueueForBatch(&queue, batch, fileStats[batch.ID])
		}
	} else if err := db.Model(&models.RotationQuotaBatch{}).
		Select("rotation_quota_batches.state AS batch_state, COUNT(rotation_quota_batch_files.id) AS count, COALESCE(SUM(rotation_quota_batch_files.size_bytes), 0) AS bytes").
		Joins("JOIN rotation_quota_batch_files ON rotation_quota_batch_files.batch_id = rotation_quota_batches.id").
		Where("rotation_quota_batches.task_id = ?", task.ID).
		Group("rotation_quota_batches.state").Find(&queueAggregates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load proactive queue totals"})
		return
	} else {
		for _, aggregate := range queueAggregates {
			value := proactiveBytes{Count: aggregate.Count, Bytes: aggregate.Bytes}
			switch aggregate.BatchState {
			case models.BatchStateReserved:
				queue.Pending = addProactiveBytes(queue.Pending, value)
			case models.BatchStatePlanned:
				queue.Planned = addProactiveBytes(queue.Planned, value)
			case models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown:
				queue.Executing = addProactiveBytes(queue.Executing, value)
			case models.BatchStateSucceeded:
				queue.Verified = addProactiveBytes(queue.Verified, value)
			case models.BatchStateFailed:
				queue.Failed = addProactiveBytes(queue.Failed, value)
			}
		}
	}
	resultBatches := make([]proactiveBatchStatus, 0, len(batches))
	taskResolutionRequired := false
	var unknownFiles []models.RotationQuotaBatchFile
	if len(batchIDs) > 0 {
		if err := db.Where("batch_id IN ? AND state = ? AND move_handoff_state = ? AND move_resolution_state = ''", batchIDs, models.BatchFileStateUnknown, models.MoveHandoffUnknown).Find(&unknownFiles).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load move resolution status"})
			return
		}
	}
	unknownByBatch := make(map[uint][]models.RotationQuotaBatchFile)
	for _, file := range unknownFiles {
		unknownByBatch[file.BatchID] = append(unknownByBatch[file.BatchID], file)
	}
	for _, batch := range batches {
		stats := fileStats[batch.ID]
		if stats == nil {
			stats = map[string]proactiveBytes{}
		}
		process := proactiveProcessStatus{Active: batch.State == models.BatchStateRunning && batch.StartedAt != nil && batch.ProcessID > 0, StartedAt: batch.StartedAt, ExitCode: batch.ExitCode}
		resultBatch := proactiveBatchStatus{ID: batch.ID, State: batch.State, Account: accountKeyForID(accountsByKey, batch.QuotaAccountID), Remote: batch.DestinationRemote, ReservedBytes: batch.ReservedBytes, LeaseUntil: batch.LeaseUntil, Process: process, Error: redactProactiveError(batch.LastError, task, append(redactionBatches, batch)...), StartedAt: batch.StartedAt, FinishedAt: batch.FinishedAt, CreatedAt: batch.CreatedAt, UpdatedAt: batch.UpdatedAt, FileCounts: stats}
		resultBatch.TransferMode = batch.TransferMode
		resultBatch.CompletionEvidence = batch.CompletionEvidence
		resultBatch.CompletionEvidenceVersion = batch.CompletionEvidenceVersion
		if batch.TransferMode == models.TransferModeMove && batch.State == models.BatchStateUnknown && stats[models.BatchFileStateUnknown].Count > 0 {
			resultBatch.ResolutionRequired = true
			resultBatch.ResolutionActions = []string{"accept_moved", "restore_and_release"}
			for _, file := range unknownByBatch[batch.ID] {
				resultBatch.ResolutionItems = append(resultBatch.ResolutionItems, proactiveResolutionItem{BatchID: batch.ID, FileID: file.ID, ExpectedState: file.State, ExpectedUpdatedAt: file.UpdatedAt, Actions: []string{"accept_moved", "restore_and_release"}})
			}
			taskResolutionRequired = true
		}
		resultBatches = append(resultBatches, resultBatch)
	}

	taskError := redactProactiveError(task.LastError, task, redactionBatches...)
	c.JSON(http.StatusOK, gin.H{"task": gin.H{"id": task.ID, "status": task.Status, "enabled": task.Enabled, "transfer_mode": task.TransferMode, "resolution_required": taskResolutionRequired, "rescan_pending": task.RotationRescanPending, "generation": task.RotationRescanGeneration, "stop_requested": task.RotationStopRequested, "wake_at": task.RotationQuotaWakeAt, "current_error": taskError, "last_error": taskError}, "accounts": bindings, "batches": resultBatches, "queue": queue})
}

func addProactiveBytes(left, right proactiveBytes) proactiveBytes {
	left.Count += right.Count
	left.Bytes += right.Bytes
	return left
}

func addProactiveQueueForBatch(queue *proactiveQueueStatus, batch models.RotationQuotaBatch, stats map[string]proactiveBytes) {
	var target *proactiveBytes
	switch batch.State {
	case models.BatchStateReserved:
		target = &queue.Pending
	case models.BatchStatePlanned:
		target = &queue.Planned
	case models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown:
		target = &queue.Executing
	case models.BatchStateSucceeded:
		target = &queue.Verified
	case models.BatchStateFailed:
		target = &queue.Failed
	default:
		return
	}
	for _, value := range stats {
		*target = addProactiveBytes(*target, value)
	}
}

func redactProactiveError(value string, task models.Task, batches ...models.RotationQuotaBatch) string {
	secrets := []string{task.RcloneConfig, task.SourceDir}
	for _, batch := range batches {
		secrets = append(secrets, batch.RcloneConfigPath, batch.SourceRoot, batch.DestinationPath, batch.ManifestPath, batch.OwnerToken, batch.LeaseToken, batch.ProcessStartToken, batch.RequestKey, batch.RequestFingerprint)
	}
	for _, secret := range secrets {
		if strings.TrimSpace(secret) != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	value = proactiveTokenPattern.ReplaceAllString(value, "[redacted-token]")
	value = proactivePathPattern.ReplaceAllString(value, "${1}[redacted-path]")
	if len(value) > 2048 {
		value = value[:2048]
	}
	return value
}

func statusConfigIdentity(task models.Task) string {
	raw := strings.TrimSpace(task.RcloneConfig)
	if raw == "" {
		raw = models.DefaultRcloneConfigPath
	}
	if resolved, err := (&quota.Service{DB: db}).ResolveConfigPath(raw); err == nil {
		return resolved
	}
	if absolute, err := filepath.Abs(filepath.Clean(raw)); err == nil {
		return filepath.Clean(absolute)
	}
	return raw
}

func accountKeyForID(accounts map[string]models.QuotaAccount, id uint) string {
	for key, account := range accounts {
		if account.ID == id {
			return key
		}
	}
	return ""
}
