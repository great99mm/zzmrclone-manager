package manualtransfer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
)

var (
	ErrAllocationConflict         = errors.New("manual allocation request conflicts with the current run")
	ErrAllocationImmutable        = errors.New("allocated manual runs require explicit reanalysis")
	ErrAllocationReanalysisNeeded = errors.New("manual allocation failed; explicit reanalysis is required")
	ErrAllocationUnavailable      = errors.New("manual allocation preview is unavailable")
	ErrAllocationStale            = errors.New("manual allocation is stale; explicit reanalysis is required")
)

type AllocateRequest struct {
	RunID                  uint
	ExpectedRunID          *uint
	ExpectedRevision       int64
	ExpectedConfigRevision int64
	IdempotencyKey         string
	ActorIdentity          string
	ActorType              string
}

type AllocateResult struct {
	Run      ManualTransferRun
	Existing bool
}

type AllocationSummary struct {
	RunID                   uint   `json:"run_id"`
	State                   string `json:"state"`
	Revision                int64  `json:"revision"`
	AllocationVersion       int    `json:"allocation_version"`
	AllocationGeneration    int64  `json:"allocation_generation"`
	AllocationDigest        string `json:"allocation_digest,omitempty"`
	PerAccountCapBytes      int64  `json:"per_account_cap_bytes"`
	PerRunCapBytes          int64  `json:"per_run_cap_bytes"`
	TotalFileCount          int64  `json:"total_file_count"`
	TotalFileBytes          int64  `json:"total_file_bytes"`
	AssignedFileCount       int64  `json:"assigned_file_count"`
	AssignedBytes           int64  `json:"assigned_bytes"`
	AlreadyTransferredCount int64  `json:"already_transferred_count"`
	AlreadyTransferredBytes int64  `json:"already_transferred_bytes"`
	UnassignedFileCount     int64  `json:"unassigned_file_count"`
	UnassignedBytes         int64  `json:"unassigned_bytes"`
	OversizeCount           int64  `json:"oversize_count"`
	OversizeBytes           int64  `json:"oversize_bytes"`
	AggregateCapacityCount  int64  `json:"aggregate_capacity_count"`
	AggregateCapacityBytes  int64  `json:"aggregate_capacity_bytes"`
	AccountCapacityCount    int64  `json:"account_capacity_count"`
	AccountCapacityBytes    int64  `json:"account_capacity_bytes"`
}

type allocationAccountTotals struct {
	Count int64
	Bytes int64
}

type allocationResult struct {
	Digest                  string
	Assigned                int64
	AssignedBytes           int64
	AlreadyTransferredCount int64
	AlreadyTransferredBytes int64
	Unassigned              int64
	UnassignedBytes         int64
	OversizeCount           int64
	OversizeBytes           int64
	AggregateCapacityCount  int64
	AggregateCapacityBytes  int64
	AccountCapacityCount    int64
	AccountCapacityBytes    int64
	AccountTotals           []allocationAccountTotals
}

func (s *Service) CreateAllocate(request AllocateRequest) (AllocateResult, error) {
	return s.CreateAllocateContext(context.Background(), request)
}

func (s *Service) CreateAllocateContext(ctx context.Context, request AllocateRequest) (AllocateResult, error) {
	if s == nil || s.DB == nil {
		return AllocateResult{}, errors.New("manual transfer database is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateAllocateRequest(&request); err != nil {
		return AllocateResult{}, err
	}
	var seed ManualTransferRun
	if err := s.DB.First(&seed, request.RunID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return AllocateResult{}, ErrRunNotFound
		}
		return AllocateResult{}, err
	}
	result := AllocateResult{}
	operation := func(_ *models.Task) error {
		var err error
		result, err = s.createAllocateUnderFence(request)
		return err
	}
	var err error
	if s.TaskFence != nil {
		err = s.TaskFence.WithTaskExclusive(ctx, seed.TaskID, operation)
	} else {
		err = operation(nil)
	}
	if err != nil {
		return AllocateResult{}, err
	}
	if result.Existing {
		return result, nil
	}
	if err := s.EnqueueAllocation(result.Run.ID); err != nil {
		if terminalErr := s.failAllocationByID(result.Run.ID, err.Error()); terminalErr != nil {
			return AllocateResult{}, terminalErr
		}
		return AllocateResult{}, err
	}
	return result, nil
}

func (s *Service) createAllocateUnderFence(request AllocateRequest) (AllocateResult, error) {
	var run ManualTransferRun
	if err := s.DB.First(&run, request.RunID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return AllocateResult{}, ErrRunNotFound
		}
		return AllocateResult{}, err
	}
	if request.ExpectedRunID == nil || *request.ExpectedRunID != run.ID {
		return AllocateResult{}, allocationFenceError(ErrRevisionConflict)
	}
	if request.ExpectedConfigRevision <= 0 || request.ExpectedConfigRevision != normalizedManualAccountRevision(run.ManualConfigRevision) {
		return AllocateResult{}, allocationFenceError(ErrRevisionConflict)
	}
	var task models.Task
	if err := s.DB.First(&task, run.TaskID).Error; err != nil {
		return AllocateResult{}, err
	}
	if err := requireManualTask(&task); err != nil {
		return AllocateResult{}, err
	}
	var latest ManualTransferRun
	if err := s.DB.Where("task_id = ?", run.TaskID).Order("id DESC").First(&latest).Error; err != nil {
		return AllocateResult{}, err
	}
	if latest.ID != run.ID {
		return AllocateResult{}, allocationFenceError(ErrRevisionConflict)
	}
	if err := s.validateRunFence(run, task); err != nil {
		return AllocateResult{}, allocationFenceError(err)
	}
	fingerprint := allocationRequestFingerprint(run.ID, request.ExpectedRevision, request.ExpectedConfigRevision)
	if run.AllocationIdempotencyKey != "" {
		if run.AllocationIdempotencyKey != request.IdempotencyKey || run.AllocationRequestFingerprint != fingerprint {
			return AllocateResult{}, ErrIdempotencyConflict
		}
		return AllocateResult{Run: run, Existing: true}, nil
	}
	if run.State == ManualRunStateAllocated {
		return AllocateResult{}, ErrAllocationImmutable
	}
	if run.State == ManualRunStateAllocationFailed {
		return AllocateResult{}, ErrAllocationReanalysisNeeded
	}
	if run.State != ManualRunStateAnalyzed || run.Revision != request.ExpectedRevision {
		return AllocateResult{}, allocationFenceError(ErrRevisionConflict)
	}
	result := AllocateResult{}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var currentTask models.Task
		if err := tx.First(&currentTask, run.TaskID).Error; err != nil {
			return err
		}
		var currentLatest ManualTransferRun
		if err := tx.Where("task_id = ?", run.TaskID).Order("id DESC").First(&currentLatest).Error; err != nil {
			return err
		}
		if currentLatest.ID != run.ID || request.ExpectedRunID == nil || *request.ExpectedRunID != currentLatest.ID {
			return allocationFenceError(ErrRevisionConflict)
		}
		if request.ExpectedConfigRevision != normalizedManualAccountRevision(currentLatest.ManualConfigRevision) {
			return allocationFenceError(ErrRevisionConflict)
		}
		if err := s.validateRunFenceDB(tx, currentLatest, currentTask); err != nil {
			return allocationFenceError(err)
		}
		updated := tx.Model(&ManualTransferRun{}).Where("id = ? AND state = ? AND revision = ?", run.ID, ManualRunStateAnalyzed, request.ExpectedRevision).Updates(map[string]interface{}{
			"state":                          ManualRunStateAllocating,
			"revision":                       gorm.Expr("revision + 1"),
			"allocation_idempotency_key":     request.IdempotencyKey,
			"allocation_request_fingerprint": fingerprint,
			"last_error":                     "",
		})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return allocationFenceError(ErrRevisionConflict)
		}
		if err := tx.First(&result.Run, run.ID).Error; err != nil {
			return err
		}
		if err := createEventAs(tx, result.Run, ManualRunEventAllocateRequested, ManualRunStateAnalyzed, ManualRunStateAllocating, "operator requested allocation", request.ActorIdentity, request.ActorType); err != nil {
			return err
		}
		return createEventAs(tx, result.Run, ManualRunEventAllocationStarted, ManualRunStateAllocating, ManualRunStateAllocating, "allocation worker queued", request.ActorIdentity, request.ActorType)
	})
	if err != nil {
		return AllocateResult{}, err
	}
	return result, nil
}

func validateAllocateRequest(request *AllocateRequest) error {
	if request.RunID == 0 {
		return errors.New("run id is required")
	}
	if request.ExpectedRunID == nil {
		return errors.New("expected_run_id is required")
	}
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 256 || strings.ContainsAny(request.IdempotencyKey, "\x00\r\n") {
		return errors.New("idempotency key is required")
	}
	if request.ExpectedRevision <= 0 {
		return errors.New("expected_revision is required")
	}
	if request.ExpectedConfigRevision <= 0 {
		return errors.New("expected_config_revision is required")
	}
	if request.ActorIdentity == "" {
		request.ActorIdentity = "unknown-operator"
	}
	if request.ActorType == "" {
		request.ActorType = "admin_session"
	}
	return nil
}

func allocationFenceError(err error) error {
	if !errors.Is(err, ErrRevisionConflict) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrRevisionConflict, ErrAllocationStale)
}

func allocationRequestFingerprint(runID uint, revision, configRevision int64) string {
	return fingerprintBytes(strconv.FormatUint(uint64(runID), 10) + "\x00" + strconv.FormatInt(revision, 10) + "\x00" + strconv.FormatInt(configRevision, 10) + "\x00" + strconv.Itoa(ManualAllocationVersion))
}

func (s *Service) processAllocationJob(runID uint) {
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = s.failAllocationByID(runID, fmt.Sprintf("allocation worker panic: %v", recovered))
		}
	}()
	if err := s.runAllocation(runID); err != nil {
		if terminalErr := s.failAllocationByID(runID, err.Error()); terminalErr != nil {
			logAllocationFailure(runID, terminalErr)
		}
	}
}

func logAllocationFailure(runID uint, err error) {
	if err != nil {
		// Keep allocation failure reporting independent from legacy transfer logs.
		fmt.Printf("manual run %d allocation terminalization failed: %v\n", runID, err)
	}
}

func (s *Service) runAllocation(runID uint) error {
	var seed ManualTransferRun
	if err := s.DB.First(&seed, runID).Error; err != nil {
		return err
	}
	if seed.State != ManualRunStateAllocating {
		return nil
	}
	ctx := s.workerCtx
	if ctx == nil {
		ctx = context.Background()
	}
	operation := func(_ *models.Task) error {
		return s.runAllocationFenced(runID)
	}
	if s.TaskFence != nil {
		return s.TaskFence.WithTaskExclusive(ctx, seed.TaskID, operation)
	}
	return operation(nil)
}

func (s *Service) runAllocationFenced(runID uint) error {
	var run ManualTransferRun
	if err := s.DB.First(&run, runID).Error; err != nil {
		return err
	}
	if run.State != ManualRunStateAllocating {
		return nil
	}
	var task models.Task
	if err := s.DB.First(&task, run.TaskID).Error; err != nil {
		return err
	}
	if err := requireManualTask(&task); err != nil {
		return err
	}
	if err := s.validateRunFence(run, task); err != nil {
		return err
	}
	var accounts []ManualRunAccount
	if err := s.DB.Where("run_id = ?", run.ID).Order("position ASC, id ASC").Limit(ManualMaxAccountInputs + 1).Find(&accounts).Error; err != nil {
		return err
	}
	if len(accounts) == 0 || len(accounts) > ManualMaxAccountInputs {
		return errors.New("manual allocation requires at least one selected account")
	}
	generation := run.AllocationGeneration + 1
	if generation <= 0 {
		generation = 1
	}
	if err := s.DB.Where("run_id = ? AND generation = ? AND activated_at IS NULL", run.ID, generation).Delete(&ManualRunAllocation{}).Error; err != nil {
		return err
	}
	result, err := s.streamAllocation(run, accounts, generation)
	if err != nil {
		return err
	}
	now := s.now()
	return s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&ManualRunAccount{}).Where("run_id = ?", run.ID).Updates(map[string]interface{}{"allocated_count": 0, "allocated_bytes": 0}).Error; err != nil {
			return err
		}
		for index, account := range accounts {
			totals := result.AccountTotals[index]
			if err := tx.Model(&ManualRunAccount{}).Where("run_id = ? AND position = ?", run.ID, account.Position).Updates(map[string]interface{}{"allocated_count": totals.Count, "allocated_bytes": totals.Bytes}).Error; err != nil {
				return err
			}
		}
		var currentTask models.Task
		if err := tx.First(&currentTask, run.TaskID).Error; err != nil {
			return err
		}
		var currentLatest ManualTransferRun
		if err := tx.Where("task_id = ?", run.TaskID).Order("id DESC").First(&currentLatest).Error; err != nil {
			return err
		}
		if currentLatest.ID != run.ID || currentLatest.State != ManualRunStateAllocating || currentLatest.Revision != run.Revision {
			return allocationFenceError(ErrRevisionConflict)
		}
		if err := s.validateRunFenceDB(tx, run, currentTask); err != nil {
			return allocationFenceError(err)
		}
		if err := tx.Model(&ManualRunAllocation{}).Where("run_id = ? AND generation = ? AND activated_at IS NULL", run.ID, generation).Update("activated_at", now).Error; err != nil {
			return err
		}
		nextState := ManualRunStateAllocated
		if result.Assigned == 0 && result.Unassigned == 0 {
			nextState = ManualRunStateSucceeded
		}
		updated := tx.Model(&ManualTransferRun{}).Where("id = ? AND state = ? AND revision = ?", run.ID, ManualRunStateAllocating, run.Revision).Updates(map[string]interface{}{
			"state":                     nextState,
			"revision":                  gorm.Expr("revision + 1"),
			"allocation_version":        ManualAllocationVersion,
			"allocation_generation":     generation,
			"allocation_digest":         result.Digest,
			"allocation_count":          result.Assigned,
			"allocation_bytes":          result.AssignedBytes,
			"assigned_count":            result.Assigned,
			"assigned_bytes":            result.AssignedBytes,
			"already_transferred_count": result.AlreadyTransferredCount,
			"already_transferred_bytes": result.AlreadyTransferredBytes,
			"unassigned_count":          result.Unassigned,
			"unassigned_bytes":          result.UnassignedBytes,
			"oversize_count":            result.OversizeCount,
			"oversize_bytes":            result.OversizeBytes,
			"aggregate_capacity_count":  result.AggregateCapacityCount,
			"aggregate_capacity_bytes":  result.AggregateCapacityBytes,
			"account_capacity_count":    result.AccountCapacityCount,
			"account_capacity_bytes":    result.AccountCapacityBytes,
			"last_error":                "",
		})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return allocationFenceError(ErrRevisionConflict)
		}
		var active ManualTransferRun
		if err := tx.First(&active, run.ID).Error; err != nil {
			return err
		}
		return createEvent(tx, active, ManualRunEventAllocationCompleted, ManualRunStateAllocating, nextState, fmt.Sprintf("allocation completed: %d assigned files, %d already verified files", result.Assigned, result.AlreadyTransferredCount))
	})
}

func (s *Service) streamAllocation(run ManualTransferRun, accounts []ManualRunAccount, generation int64) (allocationResult, error) {
	result := allocationResult{AccountTotals: make([]allocationAccountTotals, len(accounts))}
	alreadyTransferred, err := s.previouslyVerifiedCopySnapshots(run)
	if err != nil {
		return allocationResult{}, err
	}
	used := make([]int64, len(accounts))
	hash := sha256.New()
	lastPath := ""
	lastID := uint(0)
	ctx := s.workerCtx
	if ctx == nil {
		ctx = context.Background()
	}
	batch := make([]ManualRunAllocation, 0, manualRunFileBatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := s.DB.CreateInBatches(batch, manualRunFileBatchSize).Error; err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return allocationResult{}, err
		}
		query := s.DB.Where("run_id = ? AND generation = ? AND activated_at IS NOT NULL", run.ID, run.SnapshotGeneration)
		if lastPath != "" {
			query = query.Where("relative_path > ? OR (relative_path = ? AND id > ?)", lastPath, lastPath, lastID)
		}
		var files []ManualRunFile
		if err := query.Order("relative_path ASC, id ASC").Limit(manualRunFileBatchSize).Find(&files).Error; err != nil {
			return allocationResult{}, err
		}
		if len(files) == 0 {
			break
		}
		for _, file := range files {
			if file.SizeBytes < 0 {
				return allocationResult{}, fmt.Errorf("negative source file size for %q", file.RelativePath)
			}
			row := ManualRunAllocation{RunID: run.ID, Generation: generation, RelativePath: file.RelativePath, SnapshotKey: file.SnapshotKey, SizeBytes: file.SizeBytes}
			if _, exists := alreadyTransferred[file.SnapshotKey]; exists {
				row.UnassignedReason = ManualAllocationReasonAlreadyTransferred
				result.AlreadyTransferredCount++
				result.AlreadyTransferredBytes += file.SizeBytes
			} else if file.SizeBytes > PerAccountBudgetBytes {
				row.UnassignedReason = ManualAllocationReasonOversize
				result.OversizeCount++
				result.OversizeBytes += file.SizeBytes
			} else if result.AssignedBytes > PerRunBudgetBytes-file.SizeBytes {
				row.UnassignedReason = ManualAllocationReasonAggregateCapacity
				result.AggregateCapacityCount++
				result.AggregateCapacityBytes += file.SizeBytes
			} else {
				assigned := false
				for index, account := range accounts {
					if used[index] > PerAccountBudgetBytes-file.SizeBytes {
						continue
					}
					position := account.Position
					row.AccountPosition = &position
					row.AccountID = account.AccountID
					row.AccountIdentity = account.AccountIdentity
					used[index] += file.SizeBytes
					result.AccountTotals[index].Count++
					result.AccountTotals[index].Bytes += file.SizeBytes
					result.Assigned++
					result.AssignedBytes += file.SizeBytes
					assigned = true
					break
				}
				if !assigned {
					row.UnassignedReason = ManualAllocationReasonAccountCapacity
					result.AccountCapacityCount++
					result.AccountCapacityBytes += file.SizeBytes
				}
			}
			if row.UnassignedReason != "" && row.UnassignedReason != ManualAllocationReasonAlreadyTransferred {
				result.Unassigned++
				result.UnassignedBytes += file.SizeBytes
			}
			for _, value := range []string{row.RelativePath, row.SnapshotKey, strconv.FormatInt(row.SizeBytes, 10), accountPositionString(row.AccountPosition), strconv.FormatUint(uint64(row.AccountID), 10), row.UnassignedReason} {
				_, _ = hash.Write([]byte(value))
				_, _ = hash.Write([]byte{0})
			}
			batch = append(batch, row)
			lastPath, lastID = file.RelativePath, file.ID
			if len(batch) == manualRunFileBatchSize {
				if err := flush(); err != nil {
					return allocationResult{}, err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return allocationResult{}, err
	}
	result.Digest = hex.EncodeToString(hash.Sum(nil))
	return result, nil
}

func (s *Service) previouslyVerifiedCopySnapshots(run ManualTransferRun) (map[string]struct{}, error) {
	keys := make(map[string]struct{})
	if run.TransferMode != models.TransferModeCopy || strings.TrimSpace(run.ManualConfigFingerprint) == "" {
		return keys, nil
	}
	type snapshotRecord struct {
		SnapshotKey string
	}
	var records []snapshotRecord
	if err := s.DB.Table("manual_worker_files").
		Select("DISTINCT manual_worker_files.snapshot_key").
		Joins("JOIN manual_transfer_runs ON manual_transfer_runs.id = manual_worker_files.run_id").
		Where("manual_worker_files.state = ? AND manual_worker_files.snapshot_key <> ''", ManualWorkerFileStateVerified).
		Where("manual_transfer_runs.task_id = ? AND manual_transfer_runs.id <> ? AND manual_transfer_runs.transfer_mode = ?", run.TaskID, run.ID, models.TransferModeCopy).
		Where("manual_transfer_runs.source_path = ? AND manual_transfer_runs.source_root_device = ? AND manual_transfer_runs.source_root_inode = ?", run.SourcePath, run.SourceRootDevice, run.SourceRootInode).
		Where("manual_transfer_runs.destination_path = ? AND manual_transfer_runs.config_identity = ? AND manual_transfer_runs.manual_config_fingerprint = ?", run.DestinationPath, run.ConfigIdentity, run.ManualConfigFingerprint).
		Find(&records).Error; err != nil {
		return nil, err
	}
	for _, record := range records {
		keys[record.SnapshotKey] = struct{}{}
	}
	return keys, nil
}

func (s *Service) validateRunFence(run ManualTransferRun, task models.Task) error {
	return s.validateRunFenceDB(s.DB, run, task)
}

func (s *Service) validateRunFenceDB(database *gorm.DB, run ManualTransferRun, task models.Task) error {
	if err := requireManualTask(&task); err != nil {
		return err
	}
	if run.SourcePath != task.SourceDir || !manualDestinationMatches(run.DestinationPath, &task) || run.TransferMode != task.TransferMode {
		return ErrRevisionConflict
	}
	if run.ManualInputRevision > 0 && normalizedManualInputRevision(run.ManualInputRevision) != normalizedManualInputRevision(task.ManualInputRevision) {
		return ErrRevisionConflict
	}
	if run.ManualConfigRevision > 0 && normalizedManualAccountRevision(run.ManualConfigRevision) != normalizedManualAccountRevision(task.ManualAccountRevision) {
		return ErrRevisionConflict
	}
	// Runs written before the durable config fingerprint was introduced cannot
	// be reconstructed safely from their legacy rows. Keep their read/preview
	// compatibility while all newly analyzed runs take the strict path below.
	if run.ManualConfigFingerprint == "" {
		return nil
	}
	var accounts []ManualRunAccount
	if err := database.Where("run_id = ?", run.ID).Order("position ASC, id ASC").Limit(ManualMaxAccountInputs + 1).Find(&accounts).Error; err != nil {
		return err
	}
	if len(accounts) == 0 || len(accounts) > ManualMaxAccountInputs {
		return errors.New("manual allocation requires at least one selected account")
	}
	current, err := s.currentRunAccounts(database, task, accounts)
	if err != nil {
		return err
	}
	if run.ManualConfigFingerprint != "" && manualConfigFingerprint(&task, current) != run.ManualConfigFingerprint {
		return ErrRevisionConflict
	}
	return nil
}

func (s *Service) currentRunAccounts(database *gorm.DB, task models.Task, frozen []ManualRunAccount) ([]frozenAccount, error) {
	current := make([]frozenAccount, 0, len(frozen))
	for _, frozenAccountRow := range frozen {
		var durable models.QuotaAccount
		if err := database.First(&durable, frozenAccountRow.AccountID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrRevisionConflict
			}
			return nil, err
		}
		if !durable.Enabled {
			return nil, ErrRevisionConflict
		}
		identity := strings.TrimSpace(durable.QuotaKey)
		remote := strings.TrimSpace(durable.RemoteName)
		config := strings.TrimSpace(durable.ConfigIdentity)
		if config == "" {
			config, _ = canonicalTaskConfig(&task)
		}
		if identity != frozenAccountRow.AccountIdentity || remote != frozenAccountRow.RemoteName || config != frozenAccountRow.ConfigIdentity {
			return nil, ErrRevisionConflict
		}
		current = append(current, frozenAccount{Position: frozenAccountRow.Position, AccountID: frozenAccountRow.AccountID, AccountIdentity: identity, RemoteName: remote, ConfigIdentity: config})
	}
	if database.Migrator().HasTable(&ManualTaskAccount{}) {
		var configured []ManualTaskAccount
		if err := database.Where("task_id = ? AND enabled = ?", task.ID, true).Order("position ASC, id ASC").Find(&configured).Error; err != nil {
			return nil, err
		}
		if len(configured) > 0 {
			if len(configured) != len(frozen) {
				return nil, ErrRevisionConflict
			}
			for index, account := range configured {
				if account.Position != frozen[index].Position || account.AccountID != frozen[index].AccountID || account.AccountIdentity != frozen[index].AccountIdentity || account.RemoteName != frozen[index].RemoteName || account.ConfigIdentity != frozen[index].ConfigIdentity {
					return nil, ErrRevisionConflict
				}
			}
		}
	}
	return current, nil
}

func accountPositionString(position *int) string {
	if position == nil {
		return ""
	}
	return strconv.Itoa(*position)
}

func (s *Service) failAllocationByID(runID uint, message string) error {
	message = sanitizeMessage(message)
	var run ManualTransferRun
	if err := s.DB.First(&run, runID).Error; err != nil {
		return err
	}
	if run.State != ManualRunStateAllocating {
		return nil
	}
	now := s.now()
	return s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("run_id = ? AND activated_at IS NULL", run.ID).Delete(&ManualRunAllocation{}).Error; err != nil {
			return err
		}
		result := tx.Model(&ManualTransferRun{}).Where("id = ? AND state = ? AND revision = ?", run.ID, ManualRunStateAllocating, run.Revision).Updates(map[string]interface{}{"state": ManualRunStateAllocationFailed, "revision": gorm.Expr("revision + 1"), "failed_at": now, "last_error": message})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevisionConflict
		}
		return createEventAs(tx, run, ManualRunEventAllocationFailed, ManualRunStateAllocating, ManualRunStateAllocationFailed, message, "manual-transfer-system", "system")
	})
}

func (s *Service) GetAllocationSummary(runID uint) (AllocationSummary, error) {
	run, err := s.GetRun(runID)
	if err != nil {
		return AllocationSummary{}, err
	}
	return AllocationSummary{RunID: run.ID, State: run.State, Revision: run.Revision, AllocationVersion: run.AllocationVersion, AllocationGeneration: run.AllocationGeneration, AllocationDigest: run.AllocationDigest, PerAccountCapBytes: PerAccountBudgetBytes, PerRunCapBytes: PerRunBudgetBytes, TotalFileCount: run.SnapshotCount, TotalFileBytes: run.SnapshotBytes, AssignedFileCount: run.AssignedCount, AssignedBytes: run.AssignedBytes, AlreadyTransferredCount: run.AlreadyTransferredCount, AlreadyTransferredBytes: run.AlreadyTransferredBytes, UnassignedFileCount: run.UnassignedCount, UnassignedBytes: run.UnassignedBytes, OversizeCount: run.OversizeCount, OversizeBytes: run.OversizeBytes, AggregateCapacityCount: run.AggregateCapacityCount, AggregateCapacityBytes: run.AggregateCapacityBytes, AccountCapacityCount: run.AccountCapacityCount, AccountCapacityBytes: run.AccountCapacityBytes}, nil
}

type allocationFilters struct {
	Assignment string
	Reason     string
	AccountID  uint
}

func normalizeAllocationFilters(assignment, reason, accountID string) (allocationFilters, error) {
	filters := allocationFilters{Assignment: strings.ToLower(strings.TrimSpace(assignment)), Reason: strings.TrimSpace(reason)}
	if filters.Assignment != "" && filters.Assignment != "assigned" && filters.Assignment != "unassigned" {
		value, err := strconv.ParseUint(filters.Assignment, 10, 32)
		if err != nil || value == 0 {
			return allocationFilters{}, errors.New("assignment must be assigned, unassigned, or a positive account id")
		}
		filters.Assignment = "account:" + strconv.FormatUint(value, 10)
		filters.AccountID = uint(value)
	}
	if raw := strings.TrimSpace(accountID); raw != "" {
		value, err := strconv.ParseUint(raw, 10, 32)
		if err != nil || value == 0 {
			return allocationFilters{}, errors.New("account_id must be a positive integer")
		}
		if filters.AccountID != 0 && filters.AccountID != uint(value) {
			return allocationFilters{}, errors.New("assignment and account_id filters conflict")
		}
		filters.AccountID = uint(value)
		if filters.Assignment == "" {
			filters.Assignment = "account:" + strconv.FormatUint(value, 10)
		}
	}
	switch filters.Reason {
	case "", ManualAllocationReasonOversize, ManualAllocationReasonAggregateCapacity, ManualAllocationReasonAccountCapacity, ManualAllocationReasonAlreadyTransferred:
	default:
		return allocationFilters{}, errors.New("reason is not a known manual allocation reason")
	}
	if filters.Assignment == "assigned" && filters.Reason != "" {
		return allocationFilters{}, errors.New("assigned and reason filters conflict")
	}
	if filters.Assignment == "unassigned" && filters.AccountID != 0 {
		return allocationFilters{}, errors.New("unassigned and account_id filters conflict")
	}
	if filters.AccountID != 0 && filters.Reason != "" {
		return allocationFilters{}, errors.New("account_id and reason filters conflict")
	}
	return filters, nil
}

func (s *Service) ListAllocationFiles(runID uint, cursor string, limit int, assignment, reason string) (FilePage, error) {
	return s.ListAllocationFilesFiltered(runID, cursor, limit, assignment, reason, "")
}

func (s *Service) ListAllocationFilesFiltered(runID uint, cursor string, limit int, assignment, reason, accountID string) (FilePage, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > manualRunFilePageLimit {
		return FilePage{}, errors.New("file response limit exceeds the maximum")
	}
	run, err := s.GetRun(runID)
	if err != nil {
		return FilePage{}, err
	}
	if !isAllocationVisibleState(run.State) || run.AllocationGeneration <= 0 {
		return FilePage{}, ErrAllocationUnavailable
	}
	filters, err := normalizeAllocationFilters(assignment, reason, accountID)
	if err != nil {
		return FilePage{}, err
	}
	parsed := fileCursor{Generation: run.AllocationGeneration, AssignmentFilter: filters.Assignment, ReasonFilter: filters.Reason, AccountIDFilter: filters.AccountID}
	if cursor != "" {
		_, decodeErr := decodeFileCursor(cursor, &parsed)
		if decodeErr != nil || parsed.Generation != run.AllocationGeneration || parsed.RelativePath == "" || parsed.AssignmentFilter != filters.Assignment || parsed.ReasonFilter != filters.Reason || parsed.AccountIDFilter != filters.AccountID {
			return FilePage{}, errors.New("invalid allocation cursor")
		}
	}
	query := s.DB.Where("run_id = ? AND generation = ? AND activated_at IS NOT NULL", runID, run.AllocationGeneration)
	switch filters.Assignment {
	case "assigned":
		query = query.Where("unassigned_reason = ''")
	case "unassigned":
		query = query.Where("unassigned_reason <> '' AND unassigned_reason <> ?", ManualAllocationReasonAlreadyTransferred)
	default:
		if filters.AccountID != 0 {
			query = query.Where("account_id = ?", filters.AccountID)
		}
	}
	if filters.Reason != "" {
		query = query.Where("unassigned_reason = ?", filters.Reason)
	}
	if cursor != "" {
		query = query.Where("relative_path > ? OR (relative_path = ? AND id > ?)", parsed.RelativePath, parsed.RelativePath, parsed.ID)
	}
	var rows []ManualRunAllocation
	if err := query.Order("relative_path ASC, id ASC").Limit(limit + 1).Find(&rows).Error; err != nil {
		return FilePage{}, err
	}
	page := FilePage{Limit: limit, Files: make([]ManualRunFile, 0, len(rows))}
	for _, row := range rows {
		page.Files = append(page.Files, ManualRunFile{ID: row.ID, RunID: row.RunID, Generation: row.Generation, RelativePath: row.RelativePath, SnapshotKey: row.SnapshotKey, SizeBytes: row.SizeBytes, AccountPosition: row.AccountPosition, AccountID: row.AccountID, AccountIdentity: row.AccountIdentity, UnassignedReason: row.UnassignedReason, ActivatedAt: row.ActivatedAt, CreatedAt: row.CreatedAt})
	}
	if len(rows) > limit {
		page.HasMore = true
		page.Files = page.Files[:limit]
		last := rows[limit-1]
		page.NextCursor = encodeFileCursor(fileCursor{Generation: run.AllocationGeneration, RelativePath: last.RelativePath, ID: last.ID, AssignmentFilter: filters.Assignment, ReasonFilter: filters.Reason, AccountIDFilter: filters.AccountID})
	}
	return page, nil
}

func decodeFileCursor(cursor string, parsed *fileCursor) (bool, error) {
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, parsed); err != nil {
		return false, err
	}
	return true, nil
}

func encodeFileCursor(cursor fileCursor) string {
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}
