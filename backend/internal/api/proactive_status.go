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
	UploadingBytes       *int64     `json:"uploading_bytes"`
	ProviderBlockedUntil *time.Time `json:"provider_blocked_until"`
	Enabled              *bool      `json:"enabled"`
	// WindowStartedAt is the first moment the account's reservation usage
	// hit zero. While non-nil, the next quota reset is WindowStartedAt +
	// WindowSeconds. Reset to nil on refill so the cycle restarts cleanly.
	WindowStartedAt *time.Time `json:"window_started_at"`
	// NextResetAt is the absolute next reset timestamp derived from
	// WindowStartedAt + WindowSeconds. Null while the account is still
	// active (anchor not set) or after the reset has passed.
	NextResetAt *time.Time `json:"next_reset_at"`
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
	FilePaths                 []string                  `json:"file_paths"`
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

type proactiveMaintenanceStatus struct {
	State                string                         `json:"state"`
	Epoch                int64                          `json:"epoch"`
	Reason               string                         `json:"reason"`
	Revision             int64                          `json:"revision"`
	DedupeState          string                         `json:"dedupe_state"`
	Result               string                         `json:"result"`
	Error                string                         `json:"error"`
	ManualMergeAvailable bool                           `json:"manual_merge_available"`
	Blocker              string                         `json:"blocker"`
	LegacyRecovery       *proactiveLegacyRecoveryStatus `json:"legacy_recovery,omitempty"`
}

type proactiveLegacyRecoveryStatus struct {
	EpochID                  uint   `json:"epoch_id"`
	Reason                   string `json:"reason"`
	Revision                 int64  `json:"revision"`
	State                    string `json:"state"`
	DedupeState              string `json:"dedupe_state"`
	ProcessIdentityAvailable bool   `json:"process_identity_available"`
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
	var boundAccounts []models.QuotaAccount
	if err := db.Where("quota_key IN ?", mapStringValues(quotaKeys)).Find(&boundAccounts).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load quota accounts"})
		return
	}
	seenAccountIDs := make(map[uint]struct{}, len(accounts))
	for _, account := range accounts {
		seenAccountIDs[account.ID] = struct{}{}
	}
	for _, account := range boundAccounts {
		if _, exists := seenAccountIDs[account.ID]; !exists {
			accounts = append(accounts, account)
		}
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
	batchFiles := make(map[uint][]string, len(batchIDs))
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
		var filePaths []struct {
			BatchID      uint   `gorm:"column:batch_id"`
			RelativePath string `gorm:"column:relative_path"`
		}
		if err := db.Model(&models.RotationQuotaBatchFile{}).Select("batch_id, relative_path").Where("batch_id IN ?", batchIDs).Order("batch_id, relative_path").Find(&filePaths).Error; err == nil {
			for _, fp := range filePaths {
				if len(batchFiles[fp.BatchID]) < 5 {
					batchFiles[fp.BatchID] = append(batchFiles[fp.BatchID], fp.RelativePath)
				}
			}
		}
	}

	accountIDs := make([]uint, 0, len(accountsByKey))
	for _, account := range accountsByKey {
		accountIDs = append(accountIDs, account.ID)
	}
	used := make(map[uint]int64)
	reserved := make(map[uint]int64)
	uploading := make(map[uint]int64)
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
			case models.ReservationStateActive:
				uploading[row.QuotaAccountID] += row.Bytes
			case models.ReservationStateHeld, models.ReservationStateUnknown:
				reserved[row.QuotaAccountID] += row.Bytes
			case models.ReservationStateReleased, models.ReservationStateExpired:
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "quota ledger contains invalid state"})
				return
			}
		}
	}

	bindings := make([]proactiveAccountStatus, 0, len(remotes))
	now := time.Now()
	allExhausted := len(remotes) > 0
	allInitialized := true
	var earliestReset *time.Time
	for _, remote := range remotes {
		key := quotaKeys[remote]
		binding := proactiveAccountStatus{RemoteName: remote, QuotaKey: key}
		if account, ok := accountsByKey[key]; ok {
			budget, window, enabled := account.BudgetBytes, account.WindowSeconds, account.Enabled
			u, r, up := used[account.ID], reserved[account.ID], uploading[account.ID]
			totalReserved := r + up
			remaining := budget - u - totalReserved
			binding.BudgetBytes, binding.WindowSeconds, binding.Enabled = &budget, &window, &enabled
			binding.UsedBytes, binding.ActiveReservedBytes, binding.RemainingBytes = &u, &totalReserved, &remaining
			binding.UploadingBytes = &up
			binding.ProviderBlockedUntil = account.ProviderBlockedUntil
			binding.WindowStartedAt = account.WindowStartedAt
			if account.WindowStartedAt != nil && window > 0 {
				reset := account.WindowStartedAt.Add(time.Duration(window) * time.Second)
				binding.NextResetAt = &reset
				if reset.After(now) && (earliestReset == nil || reset.Before(*earliestReset)) {
					earliestReset = &reset
				}
			}
			// An account is "exhausted" only when its remaining quota is
			// zero (the user cannot reserve any more bytes today). A fresh
			// account whose first reserve is in-flight but has not yet
			// committed is NOT exhausted — the user just hasn't run any
			// transfers yet, so remaining still equals the full budget.
			exhausted := enabled && budget > 0 && remaining <= 0
			if !exhausted {
				allExhausted = false
			}
		} else {
			allInitialized = false
			allExhausted = false
		}
		bindings = append(bindings, binding)
	}
	_ = allInitialized

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
		resultBatch := proactiveBatchStatus{ID: batch.ID, State: batch.State, Account: accountKeyForID(accountsByKey, batch.QuotaAccountID), Remote: batch.DestinationRemote, ReservedBytes: batch.ReservedBytes, LeaseUntil: batch.LeaseUntil, Process: process, Error: redactProactiveError(batch.LastError, task, append(redactionBatches, batch)...), StartedAt: batch.StartedAt, FinishedAt: batch.FinishedAt, CreatedAt: batch.CreatedAt, UpdatedAt: batch.UpdatedAt, FileCounts: stats, FilePaths: batchFiles[batch.ID]}
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
	maintenance := proactiveMaintenanceStatus{}
	var epoch models.DestinationScopeMaintenance
	var legacyRecovery models.DestinationScopeMaintenance
	if db.Migrator().HasTable(&models.DestinationScopeMaintenance{}) {
		if err := db.Where("destination_scope = ? AND reason = ?", models.DestinationScope(resolvedIdentity, task.RemoteDir), models.MaintenanceReasonManualMerge).Order("epoch DESC").First(&epoch).Error; err == nil {
			maintenance = proactiveMaintenanceStatus{State: epoch.State, Epoch: epoch.Epoch, Reason: epoch.Reason, Revision: epoch.Revision, DedupeState: epoch.DedupeState, Result: epoch.Result, Error: redactProactiveError(epoch.LastError, task)}
		}
		_ = db.Where("destination_scope = ? AND reason = ? AND (state <> ? OR dedupe_state IN ?)", models.DestinationScope(resolvedIdentity, task.RemoteDir), models.MaintenanceReasonQuotaExhaustion, models.MaintenanceStateClosed, []string{models.DedupeStateClaimed, models.DedupeStateRunning, models.DedupeStateUnknown}).Order("epoch DESC").First(&legacyRecovery).Error
	}
	maintenance.ManualMergeAvailable, maintenance.Blocker = proactiveManualMergeAvailability(models.DestinationScope(resolvedIdentity, task.RemoteDir), epoch, quotaKeys, task.Status)
	if legacyRecovery.ID != 0 {
		maintenance.LegacyRecovery = &proactiveLegacyRecoveryStatus{
			EpochID:                  legacyRecovery.ID,
			Reason:                   legacyRecovery.Reason,
			Revision:                 legacyRecovery.Revision,
			State:                    legacyRecovery.State,
			DedupeState:              legacyRecovery.DedupeState,
			ProcessIdentityAvailable: legacyRecovery.ProcessID > 0 && legacyRecovery.ProcessStartToken != "",
		}
		maintenance.ManualMergeAvailable = false
		maintenance.Blocker = "legacy_maintenance_recovery"
	}
	c.JSON(http.StatusOK, gin.H{
		"task":                   gin.H{"id": task.ID, "status": task.Status, "enabled": task.Enabled, "transfer_mode": task.TransferMode, "resolution_required": taskResolutionRequired, "rescan_pending": task.RotationRescanPending, "generation": task.RotationRescanGeneration, "stop_requested": task.RotationStopRequested, "wake_at": task.RotationQuotaWakeAt, "current_error": taskError, "last_error": taskError},
		"accounts":               bindings,
		"batches":                resultBatches,
		"queue":                  queue,
		"maintenance":            maintenance,
		"all_accounts_exhausted": allExhausted,
		"next_quota_reset_at":    earliestReset,
	})
}

func proactiveManualMergeAvailability(scope string, epoch models.DestinationScopeMaintenance, keys map[string]string, taskStatus string) (bool, string) {
	if taskStatus != "idle" {
		return false, "task_running"
	}
	if epoch.ID != 0 && (epoch.State != models.MaintenanceStateClosed || epoch.DedupeState == models.DedupeStateClaimed || epoch.DedupeState == models.DedupeStateRunning || epoch.DedupeState == models.DedupeStateUnknown) {
		return false, "maintenance_epoch"
	}
	if db.Migrator().HasTable(&models.DestinationScopeCoordinator{}) {
		var coordinator models.DestinationScopeCoordinator
		if err := db.Where("destination_scope = ?", scope).First(&coordinator).Error; err == nil && coordinator.ScannerLeaseUntil != nil && coordinator.ScannerLeaseUntil.After(time.Now()) {
			return false, "scanner_active"
		}
	}
	var active int64
	if err := db.Model(&models.RotationQuotaBatch{}).Where("destination_scope = ? AND state IN ?", scope, []string{models.BatchStatePlanned, models.BatchStateReserved, models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Count(&active).Error; err != nil {
		return false, "ledger_unavailable"
	}
	if active > 0 {
		return false, "active_batch"
	}
	var accounts []models.QuotaAccount
	if err := db.Where("quota_key IN ?", mapStringValues(keys)).Find(&accounts).Error; err != nil {
		return false, "ledger_unavailable"
	}
	blocked, _, err := quota.AccountWideBlocker(db, accountIDsForStatus(accounts), scope, time.Now())
	if err != nil {
		return false, "ledger_unavailable"
	}
	if blocked {
		return false, "account_active_elsewhere"
	}
	return true, ""
}

func mapStringValues(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

func accountIDsForStatus(accounts []models.QuotaAccount) []uint {
	result := make([]uint, len(accounts))
	for i, account := range accounts {
		result[i] = account.ID
	}
	return result
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
	secrets := []string{task.RcloneConfig, task.SourceDir, task.RemoteDir}
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
