package manualtransfer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/proactive"
)

var ErrSettlementConflict = errors.New("manual run settlement state conflict")

const settlementParallelism = 8

type SettlementRequest struct {
	RunID            uint
	ExpectedRevision int64
	IdempotencyKey   string
	ActorIdentity    string
	ActorType        string
}

type SettlementResult struct {
	Run      ManualTransferRun
	Existing bool
}

type settlementObservation struct {
	fileID        uint
	workerID      uint
	sizeBytes     int64
	state         string
	releaseReason string
	err           error
}

type settlementTarget struct {
	file   ManualWorkerFile
	worker ManualRunWorker
}

func normalizedSettlementState(state string) string {
	if strings.TrimSpace(state) == "" {
		return ManualSettlementStateActive
	}
	return state
}

func validateSettlementRequest(request *SettlementRequest) error {
	if request == nil || request.RunID == 0 {
		return ErrRunNotFound
	}
	if request.ExpectedRevision <= 0 {
		return ErrRevisionConflict
	}
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 256 || strings.ContainsAny(request.IdempotencyKey, "\x00\r\n") {
		return errors.New("idempotency key is required")
	}
	if request.ActorIdentity == "" {
		request.ActorIdentity = "unknown-operator"
	}
	if request.ActorType == "" {
		request.ActorType = "admin_session"
	}
	return nil
}

func (s *Service) StopRun(ctx context.Context, request SettlementRequest) (SettlementResult, error) {
	if s == nil || s.DB == nil {
		return SettlementResult{}, errors.New("manual transfer database is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateSettlementRequest(&request); err != nil {
		return SettlementResult{}, err
	}
	seed, err := s.GetRun(request.RunID)
	if err != nil {
		return SettlementResult{}, err
	}
	result := SettlementResult{}
	mark := func(_ *models.Task) error {
		return retryManualSQLite(func() error {
			return s.DB.Transaction(func(tx *gorm.DB) error {
				var run ManualTransferRun
				if err := tx.First(&run, request.RunID).Error; err != nil {
					return err
				}
				state := normalizedSettlementState(run.SettlementState)
				if run.State == ManualRunStateSucceeded && state == ManualSettlementStateFinished {
					result = SettlementResult{Run: run, Existing: true}
					return nil
				}
				if run.SettlementStopKey == request.IdempotencyKey && state != ManualSettlementStateActive {
					result = SettlementResult{Run: run, Existing: true}
					return nil
				}
				if request.ExpectedRevision != run.Revision || (state != ManualSettlementStateActive && state != ManualSettlementStateStopping) {
					return ErrSettlementConflict
				}
				updated := tx.Model(&ManualTransferRun{}).Where("id = ? AND revision = ?", run.ID, run.Revision).Updates(map[string]interface{}{
					"settlement_state":    ManualSettlementStateStopping,
					"settlement_stop_key": request.IdempotencyKey,
					"settlement_error":    "",
					"revision":            gorm.Expr("revision + 1"),
				})
				if updated.Error != nil {
					return updated.Error
				}
				if updated.RowsAffected != 1 {
					return ErrRevisionConflict
				}
				if err := tx.First(&result.Run, run.ID).Error; err != nil {
					return err
				}
				return createSettlementEvent(tx, run.ID, ManualRunEventStopRequested, state, ManualSettlementStateStopping, request.ActorIdentity, request.ActorType, "operator requested all workers stop")
			})
		})
	}
	if s.TaskFence != nil {
		err = s.TaskFence.WithTaskExclusive(ctx, seed.TaskID, mark)
	} else {
		err = mark(nil)
	}
	if err != nil {
		return SettlementResult{}, err
	}
	if normalizedSettlementState(result.Run.SettlementState) != ManualSettlementStateStopping {
		return result, nil
	}

	workers, err := s.GetRunWorkers(request.RunID)
	if err != nil {
		return SettlementResult{}, err
	}
	for _, worker := range workers {
		if _, cancelErr := s.CancelWorker(ctx, worker.ID, request.ActorIdentity, request.ActorType); cancelErr != nil {
			s.recordSettlementError(request.RunID, cancelErr)
			return SettlementResult{}, cancelErr
		}
	}
	if err := s.waitForRunWorkersStopped(ctx, request.RunID); err != nil {
		s.recordSettlementError(request.RunID, err)
		return SettlementResult{}, err
	}
	if err := retryManualSQLite(func() error {
		return s.DB.Transaction(func(tx *gorm.DB) error {
			var run ManualTransferRun
			if err := tx.First(&run, request.RunID).Error; err != nil {
				return err
			}
			if normalizedSettlementState(run.SettlementState) != ManualSettlementStateStopping {
				return ErrSettlementConflict
			}
			if err := ensureRunWorkersStoppedTx(tx, run.ID); err != nil {
				return err
			}
			now := s.now()
			updated := tx.Model(&ManualTransferRun{}).Where("id = ? AND revision = ? AND settlement_state = ?", run.ID, run.Revision, ManualSettlementStateStopping).Updates(map[string]interface{}{
				"settlement_state":      ManualSettlementStateStopped,
				"settlement_stopped_at": now,
				"settlement_error":      "",
				"revision":              gorm.Expr("revision + 1"),
			})
			if updated.Error != nil || updated.RowsAffected != 1 {
				if updated.Error != nil {
					return updated.Error
				}
				return ErrRevisionConflict
			}
			if err := createSettlementEvent(tx, run.ID, ManualRunEventStopped, ManualSettlementStateStopping, ManualSettlementStateStopped, request.ActorIdentity, request.ActorType, "all manual run workers stopped"); err != nil {
				return err
			}
			return tx.First(&result.Run, run.ID).Error
		})
	}); err != nil {
		return SettlementResult{}, err
	}
	return result, nil
}

func (s *Service) ReconcileRun(ctx context.Context, request SettlementRequest) (SettlementResult, error) {
	if s == nil || s.DB == nil {
		return SettlementResult{}, errors.New("manual transfer database is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateSettlementRequest(&request); err != nil {
		return SettlementResult{}, err
	}
	seed, err := s.GetRun(request.RunID)
	if err != nil {
		return SettlementResult{}, err
	}
	result := SettlementResult{}
	mark := func(_ *models.Task) error {
		return retryManualSQLite(func() error {
			return s.DB.Transaction(func(tx *gorm.DB) error {
				var run ManualTransferRun
				if err := tx.First(&run, request.RunID).Error; err != nil {
					return err
				}
				state := normalizedSettlementState(run.SettlementState)
				if run.State == ManualRunStateSucceeded && state == ManualSettlementStateFinished {
					result = SettlementResult{Run: run, Existing: true}
					return nil
				}
				if run.SettlementReconcileKey == request.IdempotencyKey && (state == ManualSettlementStateReconciling || state == ManualSettlementStateReconciled || state == ManualSettlementStateFinished) {
					result = SettlementResult{Run: run, Existing: true}
					return nil
				}
				if request.ExpectedRevision != run.Revision || state != ManualSettlementStateStopped {
					return ErrSettlementConflict
				}
				if err := ensureRunWorkersStoppedTx(tx, run.ID); err != nil {
					return err
				}
				updated := tx.Model(&ManualTransferRun{}).Where("id = ? AND revision = ? AND settlement_state = ?", run.ID, run.Revision, ManualSettlementStateStopped).Updates(map[string]interface{}{
					"settlement_state":         ManualSettlementStateReconciling,
					"settlement_reconcile_key": request.IdempotencyKey,
					"settlement_checked_count": 0,
					"settlement_error":         "",
					"revision":                 gorm.Expr("revision + 1"),
				})
				if updated.Error != nil || updated.RowsAffected != 1 {
					if updated.Error != nil {
						return updated.Error
					}
					return ErrRevisionConflict
				}
				if err := tx.First(&result.Run, run.ID).Error; err != nil {
					return err
				}
				return createSettlementEvent(tx, run.ID, ManualRunEventReconcileRequested, ManualSettlementStateStopped, ManualSettlementStateReconciling, request.ActorIdentity, request.ActorType, "operator requested remote comparison and release")
			})
		})
	}
	if s.TaskFence != nil {
		err = s.TaskFence.WithTaskExclusive(ctx, seed.TaskID, mark)
	} else {
		err = mark(nil)
	}
	if err != nil || result.Existing {
		return result, err
	}
	if err := s.enqueueSettlement(request.RunID); err != nil {
		s.failSettlementReconciliation(request.RunID, err)
		return SettlementResult{}, err
	}
	return result, nil
}

func (s *Service) FinishRun(ctx context.Context, request SettlementRequest) (SettlementResult, error) {
	if s == nil || s.DB == nil {
		return SettlementResult{}, errors.New("manual transfer database is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateSettlementRequest(&request); err != nil {
		return SettlementResult{}, err
	}
	seed, err := s.GetRun(request.RunID)
	if err != nil {
		return SettlementResult{}, err
	}
	result := SettlementResult{}
	finish := func(_ *models.Task) error {
		return retryManualSQLite(func() error {
			return s.DB.Transaction(func(tx *gorm.DB) error {
				var run ManualTransferRun
				if err := tx.First(&run, request.RunID).Error; err != nil {
					return err
				}
				state := normalizedSettlementState(run.SettlementState)
				if run.State == ManualRunStateSucceeded && state == ManualSettlementStateFinished {
					result = SettlementResult{Run: run, Existing: true}
					return nil
				}
				if run.SettlementFinishKey == request.IdempotencyKey && state == ManualSettlementStateFinished {
					result = SettlementResult{Run: run, Existing: true}
					return nil
				}
				if request.ExpectedRevision != run.Revision || state != ManualSettlementStateReconciled {
					return ErrSettlementConflict
				}
				if err := ensureRunWorkersStoppedTx(tx, run.ID); err != nil {
					return err
				}
				var unresolved int64
				if err := tx.Model(&ManualWorkerFile{}).Where("run_id = ? AND state NOT IN ?", run.ID, []string{ManualWorkerFileStateVerified, ManualWorkerFileStateReleased}).Count(&unresolved).Error; err != nil {
					return err
				}
				if unresolved != 0 {
					return ErrSettlementConflict
				}
				now := s.now()
				updated := tx.Model(&ManualTransferRun{}).Where("id = ? AND revision = ? AND settlement_state = ?", run.ID, run.Revision, ManualSettlementStateReconciled).Updates(map[string]interface{}{
					"settlement_state":       ManualSettlementStateFinished,
					"settlement_finish_key":  request.IdempotencyKey,
					"settlement_finished_at": now,
					"settlement_error":       "",
					"revision":               gorm.Expr("revision + 1"),
				})
				if updated.Error != nil || updated.RowsAffected != 1 {
					if updated.Error != nil {
						return updated.Error
					}
					return ErrRevisionConflict
				}
				if err := createSettlementEvent(tx, run.ID, ManualRunEventFinished, ManualSettlementStateReconciled, ManualSettlementStateFinished, request.ActorIdentity, request.ActorType, "operator finished reconciled manual run"); err != nil {
					return err
				}
				return tx.First(&result.Run, run.ID).Error
			})
		})
	}
	if s.TaskFence != nil {
		err = s.TaskFence.WithTaskExclusive(ctx, seed.TaskID, finish)
	} else {
		err = finish(nil)
	}
	return result, err
}

func (s *Service) processSettlementJob(runID uint) {
	if err := s.reconcileRunFiles(s.workerCtx, runID); err != nil {
		s.failSettlementReconciliation(runID, err)
	}
}

func (s *Service) reconcileRunFiles(ctx context.Context, runID uint) error {
	if s.Runner == nil {
		return ErrWorkerUnavailable
	}
	var run ManualTransferRun
	if err := s.DB.First(&run, runID).Error; err != nil {
		return err
	}
	if normalizedSettlementState(run.SettlementState) != ManualSettlementStateReconciling {
		return nil
	}
	var workers []ManualRunWorker
	if err := s.DB.Where("run_id = ?", runID).Find(&workers).Error; err != nil {
		return err
	}
	byID := make(map[uint]ManualRunWorker, len(workers))
	for _, worker := range workers {
		byID[worker.ID] = worker
	}
	var files []ManualWorkerFile
	if err := s.DB.Where("run_id = ?", runID).Order("id ASC").Find(&files).Error; err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("manual run has no assigned worker files")
	}
	targets := make(chan settlementTarget)
	results := make(chan settlementObservation, len(files))
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	workerCount := settlementParallelism
	if len(files) < workerCount {
		workerCount = len(files)
	}
	for index := 0; index < workerCount; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for target := range targets {
				results <- s.observeSettlementTarget(workerCtx, run, target)
			}
		}()
	}
	go func() {
		defer close(targets)
		for _, file := range files {
			worker, ok := byID[file.WorkerID]
			if !ok {
				results <- settlementObservation{fileID: file.ID, workerID: file.WorkerID, err: errors.New("worker assignment is missing")}
				continue
			}
			select {
			case targets <- settlementTarget{file: file, worker: worker}:
			case <-workerCtx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	observations := make([]settlementObservation, 0, len(files))
	var firstErr error
	checked := int64(0)
	for observation := range results {
		checked++
		if observation.err != nil && firstErr == nil {
			firstErr = observation.err
			cancel()
		}
		observations = append(observations, observation)
		if checked%25 == 0 || checked == int64(len(files)) {
			_ = s.DB.Model(&ManualTransferRun{}).Where("id = ? AND settlement_state = ?", runID, ManualSettlementStateReconciling).Update("settlement_checked_count", checked).Error
		}
	}
	if firstErr != nil {
		return firstErr
	}
	if len(observations) != len(files) {
		return errors.New("remote comparison was interrupted")
	}
	return s.commitSettlementObservations(run, observations)
}

func (s *Service) observeSettlementTarget(ctx context.Context, run ManualTransferRun, target settlementTarget) settlementObservation {
	result := settlementObservation{fileID: target.file.ID, workerID: target.worker.ID, sizeBytes: target.file.SizeBytes}
	object, err := s.Runner.StatRemote(ctx, target.worker.ConfigIdentity, target.worker.RemoteName, run.DestinationPath, target.file.RelativePath)
	if err != nil {
		if errors.Is(err, proactive.ErrRemoteObjectNotFound) {
			result.state = ManualWorkerFileStateReleased
			result.releaseReason = ManualWorkerFileReleaseRemoteMissing
			return result
		}
		result.err = fmt.Errorf("remote comparison failed for %q: %w", target.file.RelativePath, err)
		return result
	}
	if object.IsDir || (object.Path != "" && object.Path != target.file.RelativePath && filepath.Base(object.Path) != filepath.Base(target.file.RelativePath)) {
		result.err = fmt.Errorf("remote stat did not confirm assigned file %q", target.file.RelativePath)
		return result
	}
	if object.Size != target.file.SizeBytes {
		result.state = ManualWorkerFileStateReleased
		result.releaseReason = ManualWorkerFileReleaseSizeMismatch
		return result
	}
	result.state = ManualWorkerFileStateVerified
	return result
}

func (s *Service) commitSettlementObservations(run ManualTransferRun, observations []settlementObservation) error {
	return retryManualSQLite(func() error {
		return s.DB.Transaction(func(tx *gorm.DB) error {
			var current ManualTransferRun
			if err := tx.First(&current, run.ID).Error; err != nil {
				return err
			}
			if normalizedSettlementState(current.SettlementState) != ManualSettlementStateReconciling {
				return ErrSettlementConflict
			}
			now := s.now()
			var verifiedCount, verifiedBytes, releasedCount, releasedBytes int64
			for _, observation := range observations {
				updates := map[string]interface{}{"state": observation.state, "release_reason": observation.releaseReason, "last_error": ""}
				if observation.state == ManualWorkerFileStateVerified {
					updates["verified_at"] = now
					verifiedCount++
					verifiedBytes += observation.sizeBytes
				} else {
					updates["verified_at"] = nil
					releasedCount++
					releasedBytes += observation.sizeBytes
				}
				updated := tx.Model(&ManualWorkerFile{}).Where("id = ? AND run_id = ?", observation.fileID, run.ID).Updates(updates)
				if updated.Error != nil || updated.RowsAffected != 1 {
					if updated.Error != nil {
						return updated.Error
					}
					return errors.New("worker file changed during reconciliation")
				}
			}
			var aggregates []struct {
				WorkerID uint
				Count    int64
				Bytes    int64
			}
			if err := tx.Model(&ManualWorkerFile{}).Select("worker_id, count(*) AS count, coalesce(sum(size_bytes), 0) AS bytes").Where("run_id = ? AND state = ?", run.ID, ManualWorkerFileStateVerified).Group("worker_id").Scan(&aggregates).Error; err != nil {
				return err
			}
			if err := tx.Model(&ManualRunWorker{}).Where("run_id = ?", run.ID).Updates(map[string]interface{}{"completed_count": 0, "completed_bytes": 0, "progress_percent": 0, "speed_bytes_per_second": 0}).Error; err != nil {
				return err
			}
			for _, aggregate := range aggregates {
				var worker ManualRunWorker
				if err := tx.First(&worker, aggregate.WorkerID).Error; err != nil {
					return err
				}
				percent := float64(0)
				if worker.AssignedBytes > 0 {
					percent = float64(aggregate.Bytes) * 100 / float64(worker.AssignedBytes)
				}
				if err := tx.Model(&ManualRunWorker{}).Where("id = ?", worker.ID).Updates(map[string]interface{}{"completed_count": aggregate.Count, "completed_bytes": aggregate.Bytes, "progress_percent": percent, "speed_bytes_per_second": 0}).Error; err != nil {
					return err
				}
			}
			updated := tx.Model(&ManualTransferRun{}).Where("id = ? AND revision = ? AND settlement_state = ?", current.ID, current.Revision, ManualSettlementStateReconciling).Updates(map[string]interface{}{
				"settlement_state":          ManualSettlementStateReconciled,
				"settlement_checked_count":  int64(len(observations)),
				"settlement_verified_count": verifiedCount,
				"settlement_verified_bytes": verifiedBytes,
				"settlement_released_count": releasedCount,
				"settlement_released_bytes": releasedBytes,
				"settlement_reconciled_at":  now,
				"settlement_error":          "",
				"revision":                  gorm.Expr("revision + 1"),
			})
			if updated.Error != nil || updated.RowsAffected != 1 {
				if updated.Error != nil {
					return updated.Error
				}
				return ErrRevisionConflict
			}
			details := fmt.Sprintf("remote comparison verified %d files and released %d files", verifiedCount, releasedCount)
			return createSettlementEvent(tx, run.ID, ManualRunEventReconciled, ManualSettlementStateReconciling, ManualSettlementStateReconciled, "manual-transfer-settlement", "system", details)
		})
	})
}

func (s *Service) failSettlementReconciliation(runID uint, cause error) {
	message := "remote comparison failed"
	if cause != nil {
		message = SanitizeMessage(cause.Error())
	}
	_ = retryManualSQLite(func() error {
		return s.DB.Transaction(func(tx *gorm.DB) error {
			var run ManualTransferRun
			if err := tx.First(&run, runID).Error; err != nil {
				return err
			}
			if normalizedSettlementState(run.SettlementState) != ManualSettlementStateReconciling {
				return nil
			}
			if err := tx.Model(&ManualTransferRun{}).Where("id = ? AND revision = ?", run.ID, run.Revision).Updates(map[string]interface{}{"settlement_state": ManualSettlementStateStopped, "settlement_error": message, "revision": gorm.Expr("revision + 1")}).Error; err != nil {
				return err
			}
			return createSettlementEvent(tx, run.ID, ManualRunEventReconcileFailed, ManualSettlementStateReconciling, ManualSettlementStateStopped, "manual-transfer-settlement", "system", message)
		})
	})
}

func (s *Service) RecoverSettlements() error {
	if s == nil || s.DB == nil {
		return errors.New("manual transfer database is required")
	}
	return retryManualSQLite(func() error {
		return s.DB.Transaction(func(tx *gorm.DB) error {
			var runs []ManualTransferRun
			if err := tx.Where("settlement_state IN ?", []string{ManualSettlementStateReconciling, ManualSettlementStateStopping}).Find(&runs).Error; err != nil {
				return err
			}
			for _, run := range runs {
				state := normalizedSettlementState(run.SettlementState)
				next := state
				message := "stop was interrupted by server restart; retry stop"
				updates := map[string]interface{}{}
				if state == ManualSettlementStateReconciling {
					next = ManualSettlementStateStopped
					message = "remote comparison was interrupted by server restart; retry comparison"
				} else if stopErr := ensureRunWorkersStoppedTx(tx, run.ID); stopErr == nil {
					next = ManualSettlementStateStopped
					message = ""
					updates["settlement_stopped_at"] = s.now()
				} else if !errors.Is(stopErr, ErrSettlementConflict) {
					return stopErr
				}
				updates["settlement_state"] = next
				updates["settlement_error"] = message
				updates["revision"] = gorm.Expr("revision + 1")
				updated := tx.Model(&ManualTransferRun{}).Where("id = ? AND revision = ?", run.ID, run.Revision).Updates(updates)
				if updated.Error != nil {
					return updated.Error
				}
				if updated.RowsAffected != 1 {
					return ErrRevisionConflict
				}
				if state == ManualSettlementStateStopping && next == ManualSettlementStateStopped {
					if err := createSettlementEvent(tx, run.ID, ManualRunEventStopped, state, next, "manual-transfer-settlement", "system", "all manual run workers were stopped during server restart recovery"); err != nil {
						return err
					}
				}
			}
			var succeededRunIDs []uint
			if err := tx.Model(&ManualTransferRun{}).Where("state = ? AND settlement_state <> ?", ManualRunStateSucceeded, ManualSettlementStateFinished).Pluck("id", &succeededRunIDs).Error; err != nil {
				return err
			}
			for _, runID := range succeededRunIDs {
				if err := s.deriveRunStateTx(tx, runID); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func (s *Service) waitForRunWorkersStopped(ctx context.Context, runID uint) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	for {
		stopped, err := s.runWorkersStopped(runID)
		if err != nil {
			return err
		}
		if stopped {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return errors.New("workers did not reach a stopped state before timeout")
		case <-ticker.C:
		}
	}
}

func (s *Service) runWorkersStopped(runID uint) (bool, error) {
	s.workerMu.Lock()
	for workerID := range s.workerProcesses {
		var count int64
		if err := s.DB.Model(&ManualRunWorker{}).Where("id = ? AND run_id = ?", workerID, runID).Count(&count).Error; err != nil {
			s.workerMu.Unlock()
			return false, err
		}
		if count != 0 {
			s.workerMu.Unlock()
			return false, nil
		}
	}
	s.workerMu.Unlock()
	var active int64
	if err := s.DB.Model(&ManualRunWorker{}).Where("run_id = ? AND (state NOT IN ? OR lease_token <> '' OR process_id > 0 OR process_start_token <> '')", runID, []string{ManualWorkerStateSucceeded, ManualWorkerStateFailed, ManualWorkerStateCancelled}).Count(&active).Error; err != nil {
		return false, err
	}
	return active == 0, nil
}

func ensureRunWorkersStoppedTx(tx *gorm.DB, runID uint) error {
	var total, active int64
	if err := tx.Model(&ManualRunWorker{}).Where("run_id = ?", runID).Count(&total).Error; err != nil {
		return err
	}
	if total == 0 {
		return ErrSettlementConflict
	}
	if err := tx.Model(&ManualRunWorker{}).Where("run_id = ? AND (state NOT IN ? OR lease_token <> '' OR process_id > 0 OR process_start_token <> '')", runID, []string{ManualWorkerStateSucceeded, ManualWorkerStateFailed, ManualWorkerStateCancelled}).Count(&active).Error; err != nil {
		return err
	}
	if active != 0 {
		return ErrSettlementConflict
	}
	return nil
}

func (s *Service) recordSettlementError(runID uint, cause error) {
	if cause == nil {
		return
	}
	_ = s.DB.Model(&ManualTransferRun{}).Where("id = ?", runID).Update("settlement_error", SanitizeMessage(cause.Error())).Error
}

func createSettlementEvent(tx *gorm.DB, runID uint, eventType, from, to, actorIdentity, actorType, details string) error {
	return tx.Create(&ManualRunEvent{RunID: runID, EventType: eventType, FromState: from, ToState: to, ActorIdentity: actorIdentity, ActorType: actorType, Details: SanitizeMessage(details)}).Error
}
