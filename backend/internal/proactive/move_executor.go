package proactive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/quota"
)

func (e *Executor) claimMove(batchID uint) (models.RotationQuotaBatch, []models.RotationQuotaBatchFile, string, error) {
	token := randomToken()
	var batch models.RotationQuotaBatch
	var files []models.RotationQuotaBatchFile
	err := e.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&batch, batchID).Error; err != nil {
			return err
		}
		if batch.TransferMode != models.TransferModeMove || batch.StartedAt != nil || (batch.State != models.BatchStateReserved && batch.State != models.BatchStatePlanned) {
			return fmt.Errorf("batch %d is not a move batch ready to run", batchID)
		}
		if err := e.claimScope(tx, batch, batchID); err != nil {
			return err
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

func (e *Executor) runMoveBatch(ctx context.Context, batchID uint) error {
	moveRunner, ok := e.Runner.(MoveRunner)
	if !ok {
		return errors.New("move runner is unavailable while move gate is closed")
	}
	batch, files, token, err := e.claimMove(batchID)
	if err != nil {
		return err
	}
	if !models.IsValidOwnerToken(batch.OwnerToken) {
		return e.markUnknown(batchID, token, errors.New("invalid persisted owner token"))
	}
	heartbeat := e.startLeaseHeartbeat(ctx, batchID, token)
	defer heartbeat.stop()
	root, err := quota.OpenSourceRoot(batch.SourceRoot)
	if err != nil {
		return e.movePreHandoffFailure(batch, token, err)
	}
	defer root.Close()
	if root.Device != batch.SourceRootDevice || root.Inode != batch.SourceRootInode {
		return e.movePreHandoffFailure(batch, token, errors.New("source root identity changed"))
	}
	for _, file := range files {
		ok, validateErr := root.Validate(quota.LocalSnapshot{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode, RootDevice: root.Device, RootInode: root.Inode})
		if validateErr != nil || !ok {
			if validateErr == nil {
				validateErr = errors.New("source file changed")
			}
			return e.movePreHandoffFailure(batch, token, validateErr)
		}
	}
	if err := validateRuntimeConfig(batch.RcloneConfigPath); err != nil {
		return e.movePreHandoffFailure(batch, token, err)
	}
	manifestPath, manifestHash, _, err := e.Manifest.Write(e.ManifestDir, batch, files)
	if err != nil {
		return e.movePreHandoffFailure(batch, token, err)
	}
	if err := e.persistManifest(batchID, token, manifestPath, manifestHash); err != nil {
		return err
	}
	quarantine, err := quota.PrepareMoveQuarantine(root, batch.ID, batch.OwnerToken)
	if err != nil {
		return e.movePreHandoffFailure(batch, token, err)
	}
	defer quarantine.Close()
	quarantineDevice, quarantineInode, err := quarantine.Identity()
	if err != nil {
		return e.movePreHandoffFailure(batch, token, err)
	}
	if err := e.persistMoveContract(batch, token, quarantine.Path(), quarantineDevice, quarantineInode, files); err != nil {
		return err
	}
	batch.MoveHandoffContractVersion = models.MoveHandoffVersion
	batch.MoveQuarantinePath = quarantine.Path()
	batch.MoveQuarantineDevice = quarantineDevice
	batch.MoveQuarantineInode = quarantineInode
	for i := range files {
		file := &files[i]
		if err := heartbeat.err(); err != nil {
			return e.movePreStartRestore(batch, files, token, root, quarantine, err)
		}
		handoffDevice, handoffInode, moveErr := quarantine.Move(file.RelativePath, quota.LocalSnapshot{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode})
		if moveErr != nil {
			return e.moveUnknown(batchID, token, moveErr)
		}
		if err := e.updateMoveFile(batchID, token, file.ID, models.MoveHandoffQuarantined, handoffDevice, handoffInode); err != nil {
			return e.moveUnknown(batchID, token, err)
		}
		file.MoveHandoffState = models.MoveHandoffQuarantined
		file.MoveHandoffDevice, file.MoveHandoffInode = handoffDevice, handoffInode
	}
	if err := heartbeat.err(); err != nil {
		return e.movePreStartRestore(batch, files, token, root, quarantine, err)
	}
	heartbeat.stop()
	if err := e.startMoveIntent(batchID, token); err != nil {
		return e.movePreStartRestore(batch, files, token, root, quarantine, err)
	}
	process, err := moveRunner.StartMove(ctx, MoveSpec{ConfigPath: batch.RcloneConfigPath, ManifestPath: manifestPath, SourceRoot: quarantineFile(quarantine), DestinationRemote: batch.DestinationRemote, DestinationPath: batch.DestinationPath, Transfers: normalizeTransfers(batch.RcloneTransfers)})
	if err != nil {
		var started *StartedProcessIdentityError
		if errors.As(err, &started) {
			if validProcessResult(started.Result) {
				return e.finishMoveProcess(ctx, batch, files, token, quarantine, started.Result, started.WaitErr, err)
			}
			return e.moveUnknown(batchID, token, err)
		}
		return e.movePreStartRestore(batch, files, token, root, quarantine, err)
	}
	persistProcess := e.persistProcess
	if e.PersistProcessFunc != nil {
		persistProcess = e.PersistProcessFunc
	}
	if err := persistProcess(batchID, token, process); err != nil {
		stopErr := process.Stop()
		result, waitErr := process.Wait()
		if !validProcessResult(result) {
			return e.moveUnknown(batchID, token, fmt.Errorf("move process metadata write failed and process stop unconfirmed: stop=%v wait=%v", stopErr, waitErr))
		}
		return e.finishMoveProcess(ctx, batch, files, token, quarantine, result, waitErr, err)
	}
	result, waitErr := process.Wait()
	return e.finishMoveProcess(ctx, batch, files, token, quarantine, result, waitErr, nil)
}

func quarantineFile(quarantine *quota.MoveQuarantine) *os.File { return quarantine.File() }

func (e *Executor) persistMoveContract(batch models.RotationQuotaBatch, token, path string, device, inode int64, files []models.RotationQuotaBatchFile) error {
	now := e.now()
	return e.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state IN ?", batch.ID, token, []string{models.BatchStateReserved, models.BatchStatePlanned}).Updates(map[string]interface{}{"move_handoff_contract_version": models.MoveHandoffVersion, "move_quarantine_path": path, "move_quarantine_device": device, "move_quarantine_inode": inode, "move_handoff_started_at": now, "state": models.BatchStatePlanned})
		if result.Error != nil || result.RowsAffected != 1 {
			if result.Error != nil {
				return result.Error
			}
			return ErrLeaseConflict
		}
		for _, file := range files {
			result = tx.Model(&models.RotationQuotaBatchFile{}).Where("id = ? AND batch_id = ?", file.ID, batch.ID).Updates(map[string]interface{}{"move_handoff_state": models.MoveHandoffReady, "move_handoff_size": file.SizeBytes, "move_handoff_mtime_ns": file.MtimeNS})
			if result.Error != nil || result.RowsAffected != 1 {
				if result.Error != nil {
					return result.Error
				}
				return ErrLeaseConflict
			}
		}
		return nil
	})
}

func (e *Executor) updateMoveFile(batchID uint, token string, fileID uint, state string, device, inode int64) error {
	result := e.DB.Model(&models.RotationQuotaBatchFile{}).Where("id = ? AND batch_id = ? AND move_handoff_state IN ?", fileID, batchID, []string{"", models.MoveHandoffReady, models.MoveHandoffQuarantined}).Updates(map[string]interface{}{"move_handoff_state": state, "move_handoff_device": device, "move_handoff_inode": inode})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLeaseConflict
	}
	return nil
}

func (e *Executor) startMoveIntent(batchID uint, token string) error {
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
		if !account.Enabled || (account.ProviderBlockedUntil != nil && account.ProviderBlockedUntil.After(e.now())) {
			return ErrAccountBlocked
		}
		var reservations []models.QuotaReservation
		if err := tx.Where("batch_id = ?", batchID).Find(&reservations).Error; err != nil {
			return err
		}
		if len(reservations) == 0 {
			return errors.New("batch has no reservations")
		}
		now := e.now()
		for _, reservation := range reservations {
			if reservation.State != models.ReservationStateHeld || reservation.ExpiresAt == nil || !reservation.ExpiresAt.After(now) {
				return errors.New("batch reservation is not held and unexpired")
			}
		}
		if result := tx.Model(&models.QuotaReservation{}).Where("batch_id = ? AND state = ?", batchID, models.ReservationStateHeld).Updates(map[string]interface{}{"state": models.ReservationStateActive, "started_at": now}); result.Error != nil || result.RowsAffected != int64(len(reservations)) {
			if result.Error != nil {
				return result.Error
			}
			return ErrLeaseConflict
		}
		if result := tx.Model(&models.RotationQuotaBatchFile{}).Where("batch_id = ? AND state = ?", batchID, models.BatchFileStateHeld).Update("state", models.BatchFileStateActive); result.Error != nil || result.RowsAffected != int64(len(reservations)) {
			if result.Error != nil {
				return result.Error
			}
			return ErrLeaseConflict
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

func (e *Executor) movePreStartRestore(batch models.RotationQuotaBatch, files []models.RotationQuotaBatchFile, token string, root *quota.SourceRootHandle, quarantine *quota.MoveQuarantine, cause error) error {
	if err := validateMoveIdentities(batch, root, quarantine); err != nil {
		return e.moveUnknown(batch.ID, token, err)
	}
	for i := range files {
		file := &files[i]
		snapshot := quota.LocalSnapshot{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode, RootDevice: root.Device, RootInode: root.Inode}
		quarantined, _, _, err := quarantine.Present(file.RelativePath, snapshot)
		if err != nil {
			return e.moveUnknown(batch.ID, token, fmt.Errorf("move handoff discovery failed: %w", err))
		}
		original, validateErr := root.Validate(snapshot)
		if validateErr != nil {
			return e.moveUnknown(batch.ID, token, validateErr)
		}
		if quarantined && original {
			return e.moveUnknown(batch.ID, token, errors.New("move handoff found both original and quarantine files"))
		}
		if quarantined {
			if err := quarantine.Restore(file.RelativePath, snapshot); err != nil {
				return e.moveUnknown(batch.ID, token, fmt.Errorf("move pre-start restore failed: %w", err))
			}
			if err := e.updateMoveFile(batch.ID, token, file.ID, models.MoveHandoffRestored, file.Device, file.Inode); err != nil {
				return e.moveUnknown(batch.ID, token, err)
			}
			file.MoveHandoffState = models.MoveHandoffRestored
			continue
		}
		if !original {
			return e.moveUnknown(batch.ID, token, errors.New("move handoff file is absent from source and quarantine"))
		}
		if file.MoveHandoffState == models.MoveHandoffQuarantined {
			if err := e.updateMoveFile(batch.ID, token, file.ID, models.MoveHandoffRestored, file.Device, file.Inode); err != nil {
				return e.moveUnknown(batch.ID, token, err)
			}
		}
	}
	if err := e.releaseMoveBeforeStart(batch.ID, token, cause); err != nil {
		return err
	}
	return cause
}

func validateMoveIdentities(batch models.RotationQuotaBatch, root *quota.SourceRootHandle, quarantine *quota.MoveQuarantine) error {
	if root == nil || root.Device != batch.SourceRootDevice || root.Inode != batch.SourceRootInode {
		return errors.New("source root identity changed during move recovery")
	}
	device, inode, err := quarantine.Identity()
	if err != nil {
		return err
	}
	if device != batch.MoveQuarantineDevice || inode != batch.MoveQuarantineInode {
		return errors.New("move quarantine identity changed during recovery")
	}
	return nil
}

func (e *Executor) releaseMoveBeforeStart(batchID uint, token string, cause error) error {
	return e.DB.Transaction(func(tx *gorm.DB) error {
		now := e.now()
		if result := tx.Model(&models.QuotaReservation{}).Where("batch_id = ? AND state IN ?", batchID, []string{models.ReservationStateHeld, models.ReservationStateActive}).Updates(map[string]interface{}{"state": models.ReservationStateReleased, "released_at": now, "last_error": cause.Error()}); result.Error != nil {
			return result.Error
		}
		if result := tx.Model(&models.RotationQuotaBatchFile{}).Where("batch_id = ? AND state IN ?", batchID, []string{models.BatchFileStateHeld, models.BatchFileStateActive}).Update("state", models.BatchFileStateFailed); result.Error != nil {
			return result.Error
		}
		result := tx.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state IN ?", batchID, token, []string{models.BatchStateReserved, models.BatchStatePlanned, models.BatchStateRunning}).Updates(map[string]interface{}{"state": models.BatchStateFailed, "last_error": cause.Error(), "finished_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLeaseConflict
		}
		return nil
	})
}

func (e *Executor) finishMoveProcess(ctx context.Context, batch models.RotationQuotaBatch, files []models.RotationQuotaBatchFile, token string, quarantine *quota.MoveQuarantine, result ProcessResult, waitErr error, processErr error) error {
	message := result.Stderr
	if message == "" {
		message = result.Stdout
	}
	if waitErr != nil {
		message = message + ": " + waitErr.Error()
	}
	if err := e.DB.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state = ?", batch.ID, token, models.BatchStateRunning).Updates(map[string]interface{}{"state": models.BatchStateReconciling, "exit_code": result.ExitCode, "last_error": message}).Error; err != nil {
		return err
	}
	if err := e.reconcileMove(batch, files, token, quarantine); err != nil {
		return e.moveUnknown(batch.ID, token, err)
	}
	return processErr
}

func (e *Executor) reconcileMove(batch models.RotationQuotaBatch, files []models.RotationQuotaBatchFile, token string, quarantine *quota.MoveQuarantine) error {
	return e.DB.Transaction(func(tx *gorm.DB) error {
		var current models.RotationQuotaBatch
		if err := tx.First(&current, batch.ID).Error; err != nil {
			return err
		}
		if current.LeaseToken != token || current.State != models.BatchStateReconciling {
			return ErrLeaseConflict
		}
		allMoved := true
		for _, file := range files {
			present, device, inode, err := quarantine.Present(file.RelativePath, quota.LocalSnapshot{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode})
			if err != nil {
				return err
			}
			if present {
				allMoved = false
				if result := tx.Model(&models.RotationQuotaBatchFile{}).Where("id = ? AND batch_id = ? AND state IN ?", file.ID, current.ID, []string{models.BatchFileStateActive, models.BatchFileStateUnknown}).Updates(map[string]interface{}{"state": models.BatchFileStateUnknown, "move_handoff_state": models.MoveHandoffUnknown, "move_handoff_device": device, "move_handoff_inode": inode}); result.Error != nil || result.RowsAffected != 1 {
					if result.Error != nil {
						return result.Error
					}
					return ErrLeaseConflict
				}
				if result := tx.Model(&models.QuotaReservation{}).Where("batch_file_id = ? AND batch_id = ? AND state IN ?", file.ID, current.ID, []string{models.ReservationStateActive, models.ReservationStateUnknown}).Update("state", models.ReservationStateUnknown); result.Error != nil || result.RowsAffected != 1 {
					if result.Error != nil {
						return result.Error
					}
					return ErrLeaseConflict
				}
			} else {
				if result := tx.Model(&models.RotationQuotaBatchFile{}).Where("id = ? AND batch_id = ? AND state IN ?", file.ID, current.ID, []string{models.BatchFileStateActive, models.BatchFileStateUnknown}).Updates(map[string]interface{}{"state": models.BatchFileStateCommitted, "move_handoff_state": models.MoveHandoffMoved}); result.Error != nil || result.RowsAffected != 1 {
					if result.Error != nil {
						return result.Error
					}
					return ErrLeaseConflict
				}
				if result := tx.Model(&models.QuotaReservation{}).Where("batch_file_id = ? AND batch_id = ? AND state IN ?", file.ID, current.ID, []string{models.ReservationStateActive, models.ReservationStateUnknown}).Update("state", models.ReservationStateCommitted); result.Error != nil || result.RowsAffected != 1 {
					if result.Error != nil {
						return result.Error
					}
					return ErrLeaseConflict
				}
			}
		}
		updates := map[string]interface{}{"state": models.BatchStateUnknown, "completion_evidence": models.CompletionEvidenceLocal, "completion_evidence_version": models.MoveCompletionEvidenceVersion}
		if allMoved {
			updates["state"] = models.BatchStateSucceeded
			updates["finished_at"] = e.now()
		}
		result := tx.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state = ?", current.ID, token, models.BatchStateReconciling).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLeaseConflict
		}
		return nil
	})
}

func (e *Executor) movePreHandoffFailure(batch models.RotationQuotaBatch, token string, cause error) error {
	return e.releaseMoveBeforeStart(batch.ID, token, cause)
}

func (e *Executor) moveUnknown(batchID uint, token string, cause error) error {
	if err := e.markUnknown(batchID, token, cause); err != nil {
		return err
	}
	return cause
}

func (e *Executor) recoverMoveBatch(ctx context.Context, batchID uint) error {
	var batch models.RotationQuotaBatch
	if err := e.DB.First(&batch, batchID).Error; err != nil {
		return err
	}
	var files []models.RotationQuotaBatchFile
	if err := e.DB.Where("batch_id = ?", batchID).Order("relative_path").Find(&files).Error; err != nil {
		return err
	}
	for _, file := range files {
		if file.MoveResolutionState == models.MoveResolutionFrozen {
			return e.moveUnknown(batchID, batch.LeaseToken, errors.New("manual move resolution is frozen; operator review required"))
		}
	}
	root, err := quota.OpenSourceRoot(batch.SourceRoot)
	if err != nil {
		for _, file := range files {
			if file.MoveResolutionState == models.MoveResolutionResolving {
				_ = e.freezeMoveResolution(batchID, file.ID, errors.New("resolving claim source root unavailable during restart"))
			}
		}
		return err
	}
	defer root.Close()
	if root.Device != batch.SourceRootDevice || root.Inode != batch.SourceRootInode {
		for _, file := range files {
			if file.MoveResolutionState == models.MoveResolutionResolving {
				_ = e.freezeMoveResolution(batchID, file.ID, errors.New("resolving claim source root identity changed during restart"))
			}
		}
		return e.moveUnknown(batchID, batch.LeaseToken, errors.New("source root identity changed during recovery"))
	}
	quarantine, err := quota.OpenMoveQuarantine(root, batch.ID, batch.OwnerToken)
	contractValid := batch.MoveHandoffContractVersion == models.MoveHandoffVersion && batch.MoveQuarantinePath != "" && batch.MoveQuarantineDevice > 0 && batch.MoveQuarantineInode > 0
	if !contractValid {
		preHandoff := batch.StartedAt == nil && batch.ProcessID == 0 && (batch.State == models.BatchStateReserved || batch.State == models.BatchStatePlanned) && !moveHandoffContractPresent(batch)
		if preHandoff && (errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT)) {
			cause := errors.New("move recovered before handoff")
			if releaseErr := e.releaseMoveBeforeStart(batchID, batch.LeaseToken, errors.New("move recovered before handoff")); releaseErr != nil {
				return releaseErr
			}
			return fmt.Errorf("%w: %w", ErrRetryableExecutor, cause)
		}
		if err == nil {
			_ = quarantine.Close()
		}
		for _, file := range files {
			if file.MoveResolutionState == models.MoveResolutionResolving {
				_ = e.freezeMoveResolution(batchID, file.ID, errors.New("resolving claim handoff contract is incomplete"))
			}
		}
		return e.moveUnknown(batchID, batch.LeaseToken, errors.New("move handoff contract is incomplete"))
	}
	if err != nil {
		for _, file := range files {
			if file.MoveResolutionState == models.MoveResolutionResolving {
				_ = e.freezeMoveResolution(batchID, file.ID, errors.New("resolving claim quarantine unavailable during restart"))
			}
		}
		return e.moveUnknown(batchID, batch.LeaseToken, err)
	}
	defer quarantine.Close()
	if quarantine.Path() != batch.MoveQuarantinePath {
		for _, file := range files {
			if file.MoveResolutionState == models.MoveResolutionResolving {
				_ = e.freezeMoveResolution(batchID, file.ID, errors.New("resolving claim quarantine path changed"))
			}
		}
		return e.moveUnknown(batchID, batch.LeaseToken, errors.New("move quarantine path changed"))
	}
	if err := validateMoveIdentities(batch, root, quarantine); err != nil {
		for _, file := range files {
			if file.MoveResolutionState == models.MoveResolutionResolving {
				_ = e.freezeMoveResolution(batchID, file.ID, err)
			}
		}
		return e.moveUnknown(batchID, batch.LeaseToken, err)
	}
	for _, file := range files {
		if file.MoveResolutionState == models.MoveResolutionResolving {
			if err := e.recoverResolvingMoveFile(batch, file, root, quarantine); err != nil {
				return err
			}
		}
	}
	if batch.StartedAt == nil {
		err := e.movePreStartRestore(batch, files, batch.LeaseToken, root, quarantine, errors.New("move recovered before process start"))
		if err != nil {
			var current models.RotationQuotaBatch
			if loadErr := e.DB.First(&current, batchID).Error; loadErr == nil && current.State == models.BatchStateFailed {
				return nil
			}
		}
		return err
	}
	if batch.ProcessID <= 0 || batch.ProcessStartToken == "" {
		return e.moveUnknown(batchID, batch.LeaseToken, errors.New("move process identity was not durably recorded"))
	}
	if batch.State == models.BatchStateRunning || batch.State == models.BatchStateUnknown {
		result := e.DB.Model(&models.RotationQuotaBatch{}).Where("id = ? AND lease_token = ? AND state IN ?", batchID, batch.LeaseToken, []string{models.BatchStateRunning, models.BatchStateUnknown}).Update("state", models.BatchStateReconciling)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLeaseConflict
		}
	}
	return e.reconcileMove(batch, files, batch.LeaseToken, quarantine)
}

func (e *Executor) freezeMoveResolution(batchID, fileID uint, cause error) error {
	return e.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.RotationQuotaBatchFile{}).Where("id = ? AND batch_id = ? AND move_resolution_state = ?", fileID, batchID, models.MoveResolutionResolving).Update("move_resolution_state", models.MoveResolutionFrozen).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.RotationQuotaBatchFile{}).Where("id = ? AND batch_id = ?", fileID, batchID).Update("last_error", cause.Error()).Error; err != nil {
			return err
		}
		return tx.Model(&models.RotationQuotaBatch{}).Where("id = ?", batchID).Update("last_error", cause.Error()).Error
	})
}

func (e *Executor) recoverResolvingMoveFile(batch models.RotationQuotaBatch, file models.RotationQuotaBatchFile, root *quota.SourceRootHandle, quarantine *quota.MoveQuarantine) error {
	var reservation models.QuotaReservation
	if err := e.DB.Where("batch_id = ? AND batch_file_id = ?", batch.ID, file.ID).First(&reservation).Error; err != nil {
		return err
	}
	snapshot := quota.LocalSnapshot{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode, RootDevice: root.Device, RootInode: root.Inode}
	present, _, _, err := quarantine.Present(file.RelativePath, snapshot)
	if err != nil {
		return e.freezeMoveResolution(batch.ID, file.ID, fmt.Errorf("resolving claim quarantine evidence changed: %w", err))
	}
	original, err := root.Validate(snapshot)
	if err != nil {
		return e.freezeMoveResolution(batch.ID, file.ID, fmt.Errorf("resolving claim source evidence unavailable: %w", err))
	}
	request := MoveResolutionRequest{BatchID: batch.ID, FileID: file.ID, ExpectedState: models.BatchFileStateUnknown, ExpectedUpdatedAt: file.UpdatedAt, OperationToken: file.MoveResolutionToken}
	if original && !present {
		_, err = commitMoveResolution(e.DB, batch, file, reservation, request, models.BatchFileStateFailed, models.MoveHandoffRestored, models.ReservationStateReleased)
		return err
	}
	if !original && !present {
		_, err = commitMoveResolution(e.DB, batch, file, reservation, request, models.BatchFileStateCommitted, models.MoveHandoffMoved, models.ReservationStateCommitted)
		return err
	}
	return e.freezeMoveResolution(batch.ID, file.ID, errors.New("resolving claim outcome is ambiguous during restart"))
}

func moveHandoffContractPresent(batch models.RotationQuotaBatch) bool {
	return batch.MoveHandoffContractVersion != 0 || batch.MoveQuarantinePath != "" || batch.MoveQuarantineDevice != 0 || batch.MoveQuarantineInode != 0
}
