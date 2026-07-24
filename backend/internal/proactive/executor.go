package proactive

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/logger"
	"rclone-manager/internal/models"
	"rclone-manager/internal/quota"
)

var ErrLeaseConflict = errors.New("batch lease or destination scope is already owned")
var ErrAccountBlocked = errors.New("proactive quota account is blocked or disabled")
var ErrRetryableExecutor = errors.New("retryable proactive executor failure")

type Executor struct {
	DB                        *gorm.DB
	ManifestDir               string
	Runner                    CommandRunner
	Now                       func() time.Time
	Manifest                  ManifestWriter
	LeaseDuration             time.Duration
	LeaseRenewInterval        time.Duration
	BeforeStageClone          func()
	PersistProcessFunc        func(uint, string, ProcessHandle) error
	MoveEnabled               func() bool
	ConfigResolver            quota.ConfigResolver
	PersistDedupeIdentityFunc func(models.DestinationScopeMaintenance, ProcessHandle) error
}

func (e *Executor) RunBatch(ctx context.Context, batchID uint) error {
	if e == nil || e.DB == nil || e.Runner == nil {
		return errors.New("proactive executor dependencies are required")
	}
	var mode string
	if err := e.DB.Model(&models.RotationQuotaBatch{}).Where("id = ?", batchID).Pluck("transfer_mode", &mode).Error; err != nil {
		return err
	}
	if mode == models.TransferModeMove {
		if e.MoveEnabled == nil || !e.MoveEnabled() {
			return errors.New("proactive move execution is disabled")
		}
		return e.runMoveBatch(ctx, batchID)
	}
	batch, files, token, err := e.claim(batchID)
	if err != nil {
		return err
	}
	e.writeTaskLog(batch.TaskID, fmt.Sprintf("批次 #%d 开始（%d 个文件，%.0f bytes）", batchID, len(files), float64(batch.ReservedBytes)))
	_ = e.DB.Model(&models.Task{}).Where("id = ?", batch.TaskID).Update("status", "running")
	if !models.IsValidOwnerToken(batch.OwnerToken) {
		return e.markUnknown(batchID, token, errors.New("invalid persisted owner token"))
	}
	heartbeat := e.startLeaseHeartbeat(ctx, batchID, token)
	defer heartbeat.stop()
	root, err := quota.OpenSourceRoot(batch.SourceRoot)
	if err != nil {
		return e.markUnknown(batchID, token, err)
	}
	defer root.Close()
	if root.Device != batch.SourceRootDevice || root.Inode != batch.SourceRootInode {
		return e.markUnknown(batchID, token, errors.New("source root identity changed"))
	}
	for _, file := range files {
		ok, err := root.Validate(quota.LocalSnapshot{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode, RootDevice: root.Device, RootInode: root.Inode})
		if err != nil || !ok {
			if err == nil {
				err = errors.New("source file changed")
			}
			return e.markUnknown(batchID, token, err)
		}
	}
	if err := validateRuntimeConfig(batch.RcloneConfigPath); err != nil {
		return e.markUnknown(batchID, token, err)
	}
	manifestPath, manifestHash, _, err := e.Manifest.Write(e.ManifestDir, batch, files)
	if err != nil {
		return e.markUnknown(batchID, token, err)
	}
	if err := e.persistManifest(batchID, token, manifestPath, manifestHash); err != nil {
		return err
	}
	stage, err := quota.PrepareStage(e.ManifestDir, batch.ID, batch.OwnerToken, token)
	if err != nil {
		return e.markUnknown(batchID, token, err)
	}
	stage.SetBeforeClone(e.BeforeStageClone)
	for _, file := range files {
		if err := heartbeat.err(); err != nil {
			result := e.markUnknown(batchID, token, err)
			e.cleanupStage(stage, batchID, token)
			return result
		}
		snapshot := quota.LocalSnapshot{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode, RootDevice: root.Device, RootInode: root.Inode}
		validated, openErr := root.OpenValidated(snapshot)
		if openErr != nil {
			result := e.markUnknown(batchID, token, openErr)
			e.cleanupStage(stage, batchID, token)
			return result
		}
		linkErr := stage.Snapshot(snapshot, validated)
		_ = validated.Close()
		if linkErr != nil {
			result := e.markUnknown(batchID, token, linkErr)
			e.cleanupStage(stage, batchID, token)
			return result
		}
	}
	if err := heartbeat.err(); err != nil {
		result := e.markUnknown(batchID, token, err)
		e.cleanupStage(stage, batchID, token)
		return result
	}
	heartbeat.stop()
	if err := heartbeat.err(); err != nil {
		result := e.markUnknown(batchID, token, err)
		e.cleanupStage(stage, batchID, token)
		return result
	}
	if err := e.startIntent(batchID, token); err != nil {
		e.cleanupStage(stage, batchID, token)
		return err
	}
	for _, file := range files {
		if err := stage.Validate(quota.LocalSnapshot{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, Device: file.Device, Inode: file.Inode}); err != nil {
			result := e.markUnknown(batchID, token, err)
			e.cleanupStage(stage, batchID, token)
			return result
		}
	}
	e.writeTaskLog(batch.TaskID, fmt.Sprintf("批次 #%d 开始复制到 %s:%s", batchID, batch.DestinationRemote, batch.DestinationPath))
	process, err := e.Runner.StartCopy(ctx, CopySpec{ConfigPath: batch.RcloneConfigPath, ManifestPath: manifestPath, SourceRoot: stage.File(), DestinationRemote: batch.DestinationRemote, DestinationPath: batch.DestinationPath})
	if err != nil {
		var started *StartedProcessIdentityError
		if errors.As(err, &started) {
			if !validProcessResult(started.Result) {
				result := e.markUnknown(batchID, token, fmt.Errorf("started process could not be confirmed stopped: %w", err))
				_ = stage.Close()
				e.recordStageRetention(batchID, token, "process stop could not be confirmed")
				return result
			}
			return e.markStartedUnknown(ctx, batch, files, token, started, stage)
		}
		failure := e.failBeforeProcess(batchID, token, err)
		e.cleanupStage(stage, batchID, token)
		return failure
	}
	persistProcess := e.persistProcess
	if e.PersistProcessFunc != nil {
		persistProcess = e.PersistProcessFunc
	}
	if err := persistProcess(batchID, token, process); err != nil {
		stopErr := process.Stop()
		result, waitErr := process.Wait()
		if !validProcessResult(result) {
			_ = stage.Close()
			failure := e.markUnknown(batchID, token, fmt.Errorf("process metadata write failed and process stop unconfirmed: stop=%v wait=%v", stopErr, waitErr))
			e.recordStageRetention(batchID, token, "process stop could not be confirmed")
			return failure
		}
		return e.finishProcess(ctx, batch, files, token, stage, result, waitErr, err)
	}
	result, waitErr := process.Wait()
	return e.finishProcess(ctx, batch, files, token, stage, result, waitErr, nil)
}
func (e *Executor) RunDedupe(ctx context.Context, epoch models.DestinationScopeMaintenance) error {
	if epoch.Reason != models.MaintenanceReasonManualMerge || epoch.ID == 0 || epoch.LeaseToken == "" {
		return ErrManualMergeConflict
	}
	manual := true
	runner, ok := e.Runner.(DedupeRunner)
	claimed := e.DB.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND reason = ? AND state = ? AND dedupe_state = ? AND lease_token = ?", epoch.ID, models.MaintenanceReasonManualMerge, models.MaintenanceStateExhausted, models.DedupeStateClaimed, epoch.LeaseToken).Updates(map[string]interface{}{"dedupe_state": models.DedupeStateRunning, "started_at": e.now(), "revision": gorm.Expr("revision + 1")})
	if claimed.Error != nil {
		return claimed.Error
	}
	if claimed.RowsAffected != 1 {
		return ErrLeaseConflict
	}
	if !ok {
		err := errors.New("dedupe runner is unavailable")
		if manual {
			if closeErr := completeManualMaintenance(e.DB, epoch, models.DedupeStateFailed, models.DedupeStateFailed, 1, err.Error(), e.now(), e.ConfigResolver); closeErr != nil {
				return errors.Join(err, closeErr)
			}
		}
		return err
	}
	process, err := runner.StartDedupe(ctx, DedupeSpec{ConfigPath: epoch.ResolvedConfigPath, Remote: epoch.FirstRemote, DestinationPath: epoch.RemoteDir})
	if err != nil {
		if manual {
			if closeErr := completeManualMaintenance(e.DB, epoch, models.DedupeStateFailed, models.DedupeStateFailed, 1, err.Error(), e.now(), e.ConfigResolver); closeErr != nil {
				return errors.Join(err, closeErr)
			}
			return err
		}
		_ = e.DB.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND state = ? AND dedupe_state = ? AND lease_token = ?", epoch.ID, models.MaintenanceStateExhausted, models.DedupeStateRunning, epoch.LeaseToken).Updates(map[string]interface{}{"dedupe_state": models.DedupeStateFailed, "result": models.DedupeStateFailed, "last_error": redactMaintenanceError(err.Error(), epoch), "finished_at": e.now()})
		return err
	}
	var identityErr error
	identityRows := int64(1)
	if e.PersistDedupeIdentityFunc != nil {
		identityErr = e.PersistDedupeIdentityFunc(epoch, process)
	} else {
		identity := e.DB.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND state = ? AND dedupe_state = ? AND lease_token = ?", epoch.ID, models.MaintenanceStateExhausted, models.DedupeStateRunning, epoch.LeaseToken).Updates(map[string]interface{}{"process_id": process.PID(), "process_start_token": process.StartToken()})
		identityErr, identityRows = identity.Error, identity.RowsAffected
	}
	if identityErr != nil || identityRows != 1 {
		stopErr := process.Stop()
		stoppedResult, waitErr := process.Wait()
		message := "dedupe process identity persistence failed"
		if stopErr != nil {
			message += ": stop=" + stopErr.Error()
		}
		if waitErr != nil {
			message += ": wait=" + waitErr.Error()
		}
		failure := fmt.Errorf("dedupe process identity persistence failed: %v", identityErr)
		if identityErr == nil && identityRows != 1 {
			failure = errors.New("dedupe process identity persistence failed: lease ownership was lost")
		}
		if manual && stopErr == nil && validProcessResult(stoppedResult) {
			if closeErr := completeManualMaintenance(e.DB, epoch, models.DedupeStateFailed, models.DedupeStateFailed, 1, message, e.now(), e.ConfigResolver); closeErr != nil {
				return errors.Join(failure, closeErr)
			}
			return failure
		}
		if updateErr := e.DB.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND state = ? AND dedupe_state = ? AND lease_token = ?", epoch.ID, models.MaintenanceStateExhausted, models.DedupeStateRunning, epoch.LeaseToken).Updates(map[string]interface{}{"dedupe_state": models.DedupeStateUnknown, "result": models.DedupeStateUnknown, "last_error": redactMaintenanceError(message, epoch), "revision": gorm.Expr("revision + 1")}).Error; updateErr != nil {
			return errors.Join(failure, updateErr)
		}
		return failure
	}
	heartbeatDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = e.DB.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND state = ? AND dedupe_state = ? AND lease_token = ?", epoch.ID, models.MaintenanceStateExhausted, models.DedupeStateRunning, epoch.LeaseToken).Update("lease_until", e.now().Add(2*time.Minute)).Error
			case <-heartbeatDone:
				return
			}
		}
	}()
	output, waitErr := process.Wait()
	close(heartbeatDone)
	state := models.MaintenanceStateSucceeded
	if waitErr != nil || output.ExitCode != 0 {
		state = models.MaintenanceStateFailed
	}
	dedupeState := models.DedupeStateSucceeded
	if state == models.MaintenanceStateFailed {
		dedupeState = models.DedupeStateFailed
	}
	if manual {
		return completeManualMaintenance(e.DB, epoch, dedupeState, dedupeState, output.ExitCode, output.Stderr, e.now(), e.ConfigResolver)
	}
	if err := e.DB.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND state = ? AND dedupe_state = ? AND lease_token = ?", epoch.ID, models.MaintenanceStateExhausted, models.DedupeStateRunning, epoch.LeaseToken).Updates(map[string]interface{}{"dedupe_state": dedupeState, "result": dedupeState, "exit_code": output.ExitCode, "finished_at": e.now(), "last_error": redactMaintenanceError(output.Stderr, epoch), "revision": gorm.Expr("revision + 1")}).Error; err != nil {
		return err
	}
	return waitErr
}

// RecoverBatch is intentionally internal to the proactive lane. It never
// starts a process: an already-started batch is first made conservatively
// unknown, then remote-reconciled using the persisted batch lease.
func (e *Executor) RecoverBatch(ctx context.Context, batchID uint) error {
	var batch models.RotationQuotaBatch
	if err := e.DB.First(&batch, batchID).Error; err != nil {
		return err
	}
	if batch.TransferMode == models.TransferModeMove {
		return e.recoverMoveBatch(ctx, batchID)
	}
	var files []models.RotationQuotaBatchFile
	if err := e.DB.Where("batch_id = ?", batchID).Order("relative_path").Find(&files).Error; err != nil {
		return err
	}
	if batch.StartedAt == nil {
		return errors.New("recovery refuses a never-started batch")
	}
	if models.IsActiveBatchState(batch.State) {
		result := e.DB.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state IN ?", batchID, batch.LeaseToken, []string{models.BatchStateReserved, models.BatchStatePlanned, models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Update("state", models.BatchStateUnknown)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLeaseConflict
		}
	}
	result := e.DB.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state = ?", batchID, batch.LeaseToken, models.BatchStateUnknown).Update("state", models.BatchStateReconciling)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLeaseConflict
	}
	return e.reconcile(ctx, batch, batch.LeaseToken, files)
}

func (e *Executor) claim(batchID uint) (models.RotationQuotaBatch, []models.RotationQuotaBatchFile, string, error) {
	token := randomToken()
	var batch models.RotationQuotaBatch
	var files []models.RotationQuotaBatchFile
	err := e.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&batch, batchID).Error; err != nil {
			return err
		}
		if batch.StartedAt != nil {
			return fmt.Errorf("batch %d has already started and cannot be copied again", batchID)
		}
		if batch.TransferMode != models.TransferModeCopy || (batch.State != models.BatchStateReserved && batch.State != models.BatchStatePlanned) {
			return fmt.Errorf("batch %d is not a copy batch ready to run", batchID)
		}
		var running int64
		if err := tx.Model(&models.RotationQuotaBatch{}).Where("id <> ? AND destination_scope = ? AND state IN ?", batchID, batch.DestinationScope, []string{models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Count(&running).Error; err != nil {
			return err
		}
		if running > 0 {
			return ErrLeaseConflict
		}
		var leased int64
		if err := tx.Model(&models.RotationQuotaBatch{}).Where("id <> ? AND destination_scope = ? AND state IN ? AND lease_token <> '' AND lease_until > ?", batchID, batch.DestinationScope, []string{models.BatchStateReserved, models.BatchStatePlanned}, e.now()).Count(&leased).Error; err != nil {
			return err
		}
		if leased > 0 {
			return ErrLeaseConflict
		}
		result := tx.Model(&models.RotationQuotaBatch{}).Where("id = ? AND state IN ? AND (lease_until IS NULL OR lease_until <= ? OR lease_token = '')", batchID, []string{models.BatchStateReserved, models.BatchStatePlanned}, e.now()).Updates(map[string]interface{}{"lease_token": token, "lease_until": e.now().Add(e.leaseDuration())})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLeaseConflict
		}
		return tx.Where("batch_id = ?", batchID).Order("relative_path").Find(&files).Error
	})
	return batch, files, token, err
}

func (e *Executor) persistManifest(batchID uint, token, path, hash string) error {
	result := e.DB.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state IN ? AND (manifest_path = '' OR (manifest_path = ? AND manifest_hash = ?))", batchID, token, []string{models.BatchStateReserved, models.BatchStatePlanned}, path, hash).Updates(map[string]interface{}{"manifest_path": path, "manifest_hash": hash, "state": models.BatchStatePlanned})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLeaseConflict
	}
	return nil
}

func (e *Executor) startIntent(batchID uint, token string) error {
	return e.DB.Transaction(func(tx *gorm.DB) error {
		var batch models.RotationQuotaBatch
		if err := tx.First(&batch, batchID).Error; err != nil {
			return err
		}
		if batch.LeaseToken != token || batch.State != models.BatchStatePlanned {
			return ErrLeaseConflict
		}
		var account models.QuotaAccount
		if err := tx.First(&account, batch.QuotaAccountID).Error; err != nil {
			return err
		}
		now := e.now()
		if !account.Enabled || (account.ProviderBlockedUntil != nil && account.ProviderBlockedUntil.After(now)) {
			return ErrAccountBlocked
		}
		var reservations []models.QuotaReservation
		if err := tx.Where("batch_id = ?", batchID).Find(&reservations).Error; err != nil {
			return err
		}
		if len(reservations) == 0 {
			return errors.New("batch has no reservations")
		}
		for _, reservation := range reservations {
			if reservation.State != models.ReservationStateHeld || reservation.ExpiresAt == nil || !reservation.ExpiresAt.After(now) {
				return errors.New("batch reservation is not held and unexpired")
			}
		}
		if result := tx.Model(&models.QuotaReservation{}).Where("batch_id = ? AND state = ?", batchID, models.ReservationStateHeld).Updates(map[string]interface{}{"state": models.ReservationStateActive, "started_at": now}); result.Error != nil {
			return result.Error
		} else if result.RowsAffected != int64(len(reservations)) {
			return errors.New("reservation start intent lost a row")
		}
		if result := tx.Model(&models.RotationQuotaBatchFile{}).Where("batch_id = ? AND state = ?", batchID, models.BatchFileStateHeld).Update("state", models.BatchFileStateActive); result.Error != nil {
			return result.Error
		} else if result.RowsAffected != int64(len(reservations)) {
			return errors.New("file start intent lost a row")
		}
		result := tx.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state = ?", batchID, token, models.BatchStatePlanned).Updates(map[string]interface{}{"state": models.BatchStateRunning, "started_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLeaseConflict
		}
		return nil
	})
}

func (e *Executor) persistProcess(batchID uint, token string, process ProcessHandle) error {
	if process == nil || process.PID() <= 0 || process.StartToken() == "" {
		return errors.New("process identity is required")
	}
	result := e.DB.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state = ?", batchID, token, models.BatchStateRunning).Updates(map[string]interface{}{"process_id": process.PID(), "process_start_token": process.StartToken()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLeaseConflict
	}
	return nil
}

func (e *Executor) toReconciling(batchID uint, token string, result ProcessResult, waitErr error) error {
	message := result.Stderr
	if message == "" {
		message = result.Stdout
	}
	if waitErr != nil {
		message = message + ": " + waitErr.Error()
	}
	update := e.DB.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state = ?", batchID, token, models.BatchStateRunning).Updates(map[string]interface{}{"state": models.BatchStateReconciling, "exit_code": result.ExitCode, "last_error": message})
	if update.Error != nil {
		return update.Error
	}
	if update.RowsAffected != 1 {
		return ErrLeaseConflict
	}
	return nil
}

func (e *Executor) finishProcess(ctx context.Context, batch models.RotationQuotaBatch, files []models.RotationQuotaBatchFile, token string, stage *quota.StageHandle, result ProcessResult, waitErr error, processErr error) error {
	if err := e.toReconciling(batch.ID, token, result, waitErr); err != nil {
		_ = stage.Close()
		e.recordStageRetention(batch.ID, token, "process stopped but reconciliation transition failed")
		e.writeTaskLog(batch.TaskID, fmt.Sprintf("批次 #%d 对账失败: %v", batch.ID, err))
		return err
	}
	if err := e.freezeOnMarker(batch.ID, token, result); err != nil {
		_ = stage.Close()
		e.recordStageRetention(batch.ID, token, "process stopped but marker handling failed")
		e.writeTaskLog(batch.TaskID, fmt.Sprintf("批次 #%d marker 处理失败: %v", batch.ID, err))
		return err
	}
	if err := e.reconcile(ctx, batch, token, files); err != nil {
		_ = stage.Close()
		e.recordStageRetention(batch.ID, token, "process stopped but remote reconciliation failed")
		e.writeTaskLog(batch.TaskID, fmt.Sprintf("批次 #%d 远端核验失败: %v", batch.ID, err))
		return err
	}
	// Wait returned, so the process is confirmed stopped. Reconciliation has
	// completed even when the batch remains unknown (for example, a marker).
	e.cleanupStage(stage, batch.ID, token)
	if processErr == nil {
		e.writeTaskLog(batch.TaskID, fmt.Sprintf("批次 #%d 完成", batch.ID))
	} else {
		e.writeTaskLog(batch.TaskID, fmt.Sprintf("批次 #%d 失败: %v", batch.ID, processErr))
	}
	// Set task back to idle when no active batches remain.
	var active int64
	if e.DB.Model(&models.RotationQuotaBatch{}).Where("task_id = ? AND state IN ?", batch.TaskID, []string{models.BatchStateReserved, models.BatchStatePlanned, models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Count(&active); active == 0 {
		_ = e.DB.Model(&models.Task{}).Where("id = ?", batch.TaskID).Update("status", "idle")
	}
	return processErr
}

func validProcessResult(result ProcessResult) bool { return result.PID > 0 }

func (e *Executor) cleanupStage(stage *quota.StageHandle, batchID uint, token string) {
	if stage == nil {
		return
	}
	if err := stage.Cleanup(); err != nil {
		_ = e.DB.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ?", batchID, token).Update("last_error", fmt.Sprintf("stage cleanup failed; retain for Phase3B janitor: %v", err))
	}
}

func (e *Executor) recordStageRetention(batchID uint, token, reason string) {
	owner := "unknown-owner"
	var batch models.RotationQuotaBatch
	if err := e.DB.Select("owner_token").First(&batch, batchID).Error; err == nil && models.IsValidOwnerToken(batch.OwnerToken) {
		owner = batch.OwnerToken
	}
	path := filepath.Join(e.ManifestDir, ".rclone-manager-stage", fmt.Sprintf("%d-%s-%s", batchID, owner, token))
	_ = e.DB.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ?", batchID, token).Update("last_error", fmt.Sprintf("stage retained at %s for Phase3B janitor: %s", path, reason))
}

type leaseHeartbeat struct {
	stopCh chan struct{}
	doneCh chan struct{}
	errCh  chan error
	once   sync.Once
}

func (e *Executor) startLeaseHeartbeat(ctx context.Context, batchID uint, token string) *leaseHeartbeat {
	interval := e.LeaseRenewInterval
	if interval <= 0 {
		interval = e.leaseDuration() / 3
	}
	if interval <= 0 {
		interval = time.Minute
	}
	h := &leaseHeartbeat{stopCh: make(chan struct{}), doneCh: make(chan struct{}), errCh: make(chan error, 1)}
	go func() {
		defer close(h.doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := e.renewLease(batchID, token); err != nil {
					select {
					case h.errCh <- err:
					default:
					}
					return
				}
			case <-h.stopCh:
				return
			case <-ctx.Done():
				select {
				case h.errCh <- ctx.Err():
				default:
				}
				return
			}
		}
	}()
	return h
}
func (h *leaseHeartbeat) stop() {
	if h == nil {
		return
	}
	h.once.Do(func() { close(h.stopCh); <-h.doneCh })
}
func (h *leaseHeartbeat) err() error {
	if h == nil {
		return nil
	}
	select {
	case err := <-h.errCh:
		return err
	default:
		return nil
	}
}

func (e *Executor) leaseDuration() time.Duration {
	if e.LeaseDuration > 0 {
		return e.LeaseDuration
	}
	return time.Hour
}
func (e *Executor) renewLease(batchID uint, token string) error {
	result := e.DB.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state IN ?", batchID, token, []string{models.BatchStateReserved, models.BatchStatePlanned}).Update("lease_until", e.now().Add(e.leaseDuration()))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLeaseConflict
	}
	return nil
}

func (e *Executor) markStartedUnknown(ctx context.Context, batch models.RotationQuotaBatch, files []models.RotationQuotaBatchFile, token string, cause *StartedProcessIdentityError, stage *quota.StageHandle) error {
	err := e.DB.Transaction(func(tx *gorm.DB) error {
		var current models.RotationQuotaBatch
		if err := tx.First(&current, batch.ID).Error; err != nil {
			return err
		}
		if current.LeaseToken != token || current.State != models.BatchStateRunning {
			return ErrLeaseConflict
		}
		var account models.QuotaAccount
		if err := tx.First(&account, current.QuotaAccountID).Error; err != nil {
			return err
		}
		for _, file := range files {
			result := tx.Model(&models.RotationQuotaBatchFile{}).Where("id = ? AND batch_id = ? AND state = ?", file.ID, current.ID, models.BatchFileStateActive).Update("state", models.BatchFileStateUnknown)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrLeaseConflict
			}
			result = tx.Model(&models.QuotaReservation{}).Where("batch_file_id = ? AND batch_id = ? AND state = ?", file.ID, current.ID, models.ReservationStateActive).Update("state", models.ReservationStateUnknown)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrLeaseConflict
			}
		}
		if result := tx.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state = ?", current.ID, token, models.BatchStateRunning).Update("last_error", cause.Error()); result.Error != nil {
			return result.Error
		} else if result.RowsAffected != 1 {
			return ErrLeaseConflict
		}
		return nil
	})
	if err != nil {
		_ = stage.Close()
		return err
	}
	return e.finishProcess(ctx, batch, files, token, stage, cause.Result, cause.WaitErr, nil)
}

func (e *Executor) freezeOnMarker(batchID uint, token string, result ProcessResult) error {
	marker := DetectUploadLimit(result.Stdout + "\n" + result.Stderr)
	if !marker.Detected {
		return nil
	}
	now := e.now()
	return e.DB.Transaction(func(tx *gorm.DB) error {
		var batch models.RotationQuotaBatch
		if err := tx.First(&batch, batchID).Error; err != nil {
			return err
		}
		if batch.LeaseToken != token || batch.State != models.BatchStateReconciling {
			return ErrLeaseConflict
		}
		var account models.QuotaAccount
		if err := tx.First(&account, batch.QuotaAccountID).Error; err != nil {
			return err
		}
		until := now.Add(24 * time.Hour)
		if account.ProviderBlockedUntil == nil || account.ProviderBlockedUntil.Before(until) {
			account.ProviderBlockedUntil = &until
		}
		update := tx.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state = ?", batchID, token, models.BatchStateReconciling).Updates(map[string]interface{}{"limit_marker": marker.Text, "marker_detected_at": now})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrLeaseConflict
		}
		return tx.Save(&account).Error
	})
}

func (e *Executor) reconcile(ctx context.Context, batch models.RotationQuotaBatch, token string, files []models.RotationQuotaBatchFile) error {
	allVerified := true
	type observation struct {
		file     models.RotationQuotaBatchFile
		object   RemoteObject
		verified bool
	}
	observations := make([]observation, 0, len(files))
	for _, file := range files {
		object, err := e.Runner.StatRemote(ctx, batch.RcloneConfigPath, batch.DestinationRemote, batch.DestinationPath, file.RelativePath)
		verified := err == nil && !object.IsDir && object.Path == file.RelativePath && object.Size == file.SizeBytes
		if !verified {
			allVerified = false
		}
		observations = append(observations, observation{file: file, object: object, verified: verified})
	}
	return e.DB.Transaction(func(tx *gorm.DB) error {
		var current models.RotationQuotaBatch
		if err := tx.First(&current, batch.ID).Error; err != nil {
			return err
		}
		if current.LeaseToken != token || current.State != models.BatchStateReconciling {
			return ErrLeaseConflict
		}
		var account models.QuotaAccount
		if err := tx.First(&account, current.QuotaAccountID).Error; err != nil {
			return err
		}
		for _, item := range observations {
			if item.verified {
				result := tx.Model(&models.RotationQuotaBatchFile{}).Where("id = ? AND batch_id = ? AND state IN ?", item.file.ID, current.ID, []string{models.BatchFileStateActive, models.BatchFileStateUnknown}).Updates(map[string]interface{}{"state": models.BatchFileStateCommitted, "remote_path": item.object.Path, "remote_size": item.object.Size, "verified_at": e.now()})
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return ErrLeaseConflict
				}
				result = tx.Model(&models.QuotaReservation{}).Where("batch_file_id = ? AND batch_id = ? AND state IN ?", item.file.ID, current.ID, []string{models.ReservationStateActive, models.ReservationStateUnknown}).Update("state", models.ReservationStateCommitted)
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return ErrLeaseConflict
				}
			} else {
				result := tx.Model(&models.RotationQuotaBatchFile{}).Where("id = ? AND batch_id = ? AND state IN ?", item.file.ID, current.ID, []string{models.BatchFileStateActive, models.BatchFileStateUnknown}).Update("state", models.BatchFileStateUnknown)
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return ErrLeaseConflict
				}
				result = tx.Model(&models.QuotaReservation{}).Where("batch_file_id = ? AND batch_id = ? AND state IN ?", item.file.ID, current.ID, []string{models.ReservationStateActive, models.ReservationStateUnknown}).Update("state", models.ReservationStateUnknown)
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return ErrLeaseConflict
				}
			}
		}
		state := models.BatchStateUnknown
		if allVerified && current.LimitMarker == "" {
			state = models.BatchStateSucceeded
		}
		result := tx.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state = ?", current.ID, token, models.BatchStateReconciling).Update("state", state)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLeaseConflict
		}
		return nil
	})
}

func (e *Executor) failBeforeProcess(batchID uint, token string, cause error) error {
	err := e.DB.Transaction(func(tx *gorm.DB) error {
		var batch models.RotationQuotaBatch
		if err := tx.First(&batch, batchID).Error; err != nil {
			return err
		}
		if batch.LeaseToken != token || batch.State != models.BatchStateRunning {
			return ErrLeaseConflict
		}
		var account models.QuotaAccount
		if err := tx.First(&account, batch.QuotaAccountID).Error; err != nil {
			return err
		}
		if result := tx.Model(&models.QuotaReservation{}).Where("batch_id = ? AND state = ?", batchID, models.ReservationStateActive).Updates(map[string]interface{}{"state": models.ReservationStateReleased, "released_at": e.now(), "last_error": cause.Error()}); result.Error != nil {
			return result.Error
		} else if result.RowsAffected == 0 {
			return ErrLeaseConflict
		}
		if result := tx.Model(&models.RotationQuotaBatchFile{}).Where("batch_id = ? AND state = ?", batchID, models.BatchFileStateActive).Update("state", models.BatchFileStateFailed); result.Error != nil {
			return result.Error
		} else if result.RowsAffected == 0 {
			return ErrLeaseConflict
		}
		result := tx.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state = ?", batchID, token, models.BatchStateRunning).Updates(map[string]interface{}{"state": models.BatchStateFailed, "last_error": cause.Error(), "finished_at": e.now()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLeaseConflict
		}
		return nil
	})
	if err != nil {
		return err
	}
	return cause
}

func (e *Executor) markUnknown(batchID uint, token string, cause error) error {
	return e.DB.Transaction(func(tx *gorm.DB) error {
		var batch models.RotationQuotaBatch
		if err := tx.First(&batch, batchID).Error; err != nil {
			return err
		}
		if batch.LeaseToken != token || !models.IsActiveBatchState(batch.State) {
			return ErrLeaseConflict
		}
		var account models.QuotaAccount
		if err := tx.First(&account, batch.QuotaAccountID).Error; err != nil {
			return err
		}
		if result := tx.Model(&models.RotationQuotaBatchFile{}).Where("batch_id = ? AND state IN ?", batchID, []string{models.BatchFileStateHeld, models.BatchFileStateActive, models.BatchFileStateUnknown}).Update("state", models.BatchFileStateUnknown); result.Error != nil {
			return result.Error
		} else if result.RowsAffected == 0 {
			return ErrLeaseConflict
		}
		if result := tx.Model(&models.QuotaReservation{}).Where("batch_id = ? AND state IN ?", batchID, []string{models.ReservationStateHeld, models.ReservationStateActive, models.ReservationStateUnknown}).Update("state", models.ReservationStateUnknown); result.Error != nil {
			return result.Error
		} else if result.RowsAffected == 0 {
			return ErrLeaseConflict
		}
		result := tx.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state = ?", batchID, token, batch.State).Updates(map[string]interface{}{"state": models.BatchStateUnknown, "last_error": cause.Error()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLeaseConflict
		}
		return nil
	})
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}
func randomToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func validateRuntimeConfig(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("invalid pinned config")
	}
	return nil
}

func (e *Executor) writeTaskLog(taskID uint, message string) {
	_ = logger.WriteLog(fmt.Sprintf("task_%d.log", taskID), message)
	// Fallback: always write to backend.log so we don't silently lose logs
	// when the task log file isn't accessible.
	_ = logger.WriteLog("backend.log", fmt.Sprintf("[task-%d] %s", taskID, message))
}
