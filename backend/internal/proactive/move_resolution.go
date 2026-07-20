package proactive

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/quota"
)

var (
	ErrMoveResolutionConflict = errors.New("move resolution optimistic conflict")
	ErrMoveResolutionEvidence = errors.New("move resolution evidence is insufficient or ambiguous")
)

type MoveResolutionRequest struct {
	TaskID            uint
	BatchID           uint
	FileID            uint
	Action            string
	ExpectedState     string
	ExpectedUpdatedAt time.Time
	OperationToken    string
	Inspector         ProcessInspector
}

type MoveResolutionResult struct {
	BatchID uint
	FileID  uint
	State   string
}

func ResolveUnknownMoveFile(database *gorm.DB, request MoveResolutionRequest) (result MoveResolutionResult, err error) {
	defer func() {
		if errors.Is(err, ErrMoveResolutionEvidence) {
			_ = recordMoveResolutionEvidence(database, request.BatchID, request.FileID, err.Error())
		}
	}()
	if database == nil || request.BatchID == 0 || request.FileID == 0 || request.ExpectedUpdatedAt.IsZero() {
		return MoveResolutionResult{}, ErrMoveResolutionConflict
	}
	if request.Action != "accept_moved" && request.Action != "restore_and_release" {
		return MoveResolutionResult{}, fmt.Errorf("unsupported move resolution action %q", request.Action)
	}
	var batch models.RotationQuotaBatch
	var file models.RotationQuotaBatchFile
	batchQuery := database
	if request.TaskID != 0 {
		batchQuery = batchQuery.Where("task_id = ?", request.TaskID)
	}
	if err := batchQuery.First(&batch, request.BatchID).Error; err != nil {
		return MoveResolutionResult{}, err
	}
	if batch.TransferMode != models.TransferModeMove || batch.MoveHandoffContractVersion != models.MoveHandoffVersion || batch.State != models.BatchStateUnknown {
		return MoveResolutionResult{}, ErrMoveResolutionConflict
	}
	if batch.StartedAt != nil || batch.ProcessID > 0 {
		if request.Inspector == nil || batch.ProcessID <= 0 || batch.ProcessStartToken == "" {
			return MoveResolutionResult{}, ErrMoveResolutionConflict
		}
		status, inspectErr := request.Inspector.Inspect(batch.ProcessID, batch.ProcessStartToken)
		if inspectErr != nil || !status.Confirmed || status.Alive {
			return MoveResolutionResult{}, ErrMoveResolutionConflict
		}
	}
	if err := database.Where("id = ? AND batch_id = ?", request.FileID, request.BatchID).First(&file).Error; err != nil {
		return MoveResolutionResult{}, err
	}
	if request.ExpectedState != "" && file.State != request.ExpectedState {
		return MoveResolutionResult{}, ErrMoveResolutionConflict
	}
	if file.State != models.BatchFileStateUnknown || file.MoveHandoffState != models.MoveHandoffUnknown || !file.UpdatedAt.Equal(request.ExpectedUpdatedAt) {
		return MoveResolutionResult{}, ErrMoveResolutionConflict
	}
	var reservation models.QuotaReservation
	if err := database.Where("batch_id = ? AND batch_file_id = ?", request.BatchID, request.FileID).First(&reservation).Error; err != nil {
		return MoveResolutionResult{}, err
	}
	if reservation.State != models.ReservationStateUnknown {
		return MoveResolutionResult{}, ErrMoveResolutionConflict
	}

	root, err := quota.OpenSourceRoot(batch.SourceRoot)
	if err != nil {
		return MoveResolutionResult{}, fmt.Errorf("%w: source root unavailable", ErrMoveResolutionEvidence)
	}
	defer root.Close()
	if root.Device != batch.SourceRootDevice || root.Inode != batch.SourceRootInode {
		return MoveResolutionResult{}, fmt.Errorf("%w: source root identity changed", ErrMoveResolutionEvidence)
	}
	quarantine, err := quota.OpenMoveQuarantine(root, batch.ID, batch.OwnerToken)
	if err != nil {
		return MoveResolutionResult{}, fmt.Errorf("%w: quarantine unavailable", ErrMoveResolutionEvidence)
	}
	defer quarantine.Close()
	expectedQuarantine := filepath.Join(filepath.Clean(batch.SourceRoot), ".rclone-manager-move", fmt.Sprintf("%d-%s", batch.ID, batch.OwnerToken))
	if batch.MoveQuarantinePath != expectedQuarantine || quarantine.Path() != batch.MoveQuarantinePath {
		return MoveResolutionResult{}, fmt.Errorf("%w: quarantine path changed", ErrMoveResolutionEvidence)
	}
	qDevice, qInode, err := quarantine.Identity()
	if err != nil || qDevice != batch.MoveQuarantineDevice || qInode != batch.MoveQuarantineInode {
		return MoveResolutionResult{}, fmt.Errorf("%w: quarantine identity changed", ErrMoveResolutionEvidence)
	}
	snapshot := quota.LocalSnapshot{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode, RootDevice: root.Device, RootInode: root.Inode}
	present, _, _, err := quarantine.Present(file.RelativePath, snapshot)
	if err != nil {
		return MoveResolutionResult{}, fmt.Errorf("%w: quarantined inode changed", ErrMoveResolutionEvidence)
	}
	original, err := root.Validate(snapshot)
	if err != nil {
		return MoveResolutionResult{}, fmt.Errorf("%w: source evidence unavailable", ErrMoveResolutionEvidence)
	}
	if present && original {
		return MoveResolutionResult{}, fmt.Errorf("%w: source collision", ErrMoveResolutionEvidence)
	}
	if request.Action == "accept_moved" {
		if present || original {
			return MoveResolutionResult{}, fmt.Errorf("%w: file is not durably absent from local source and quarantine", ErrMoveResolutionEvidence)
		}
		claimed, claimErr := claimMoveResolution(database, batch, file, request)
		if claimErr != nil {
			return MoveResolutionResult{}, claimErr
		}
		request.OperationToken = claimed.MoveResolutionToken
		request.ExpectedUpdatedAt = claimed.UpdatedAt
		return commitMoveResolution(database, batch, claimed, reservation, request, models.BatchFileStateCommitted, models.MoveHandoffMoved, models.ReservationStateCommitted)
	}
	if !present || original {
		return MoveResolutionResult{}, fmt.Errorf("%w: exact quarantined file is not restorable", ErrMoveResolutionEvidence)
	}
	claimed, claimErr := claimMoveResolution(database, batch, file, request)
	if claimErr != nil {
		return MoveResolutionResult{}, claimErr
	}
	request.OperationToken = claimed.MoveResolutionToken
	request.ExpectedUpdatedAt = claimed.UpdatedAt
	if err := quarantine.Restore(file.RelativePath, snapshot); err != nil {
		return MoveResolutionResult{}, fmt.Errorf("%w: restore failed: %v", ErrMoveResolutionEvidence, err)
	}
	result, err = commitMoveResolution(database, batch, claimed, reservation, request, models.BatchFileStateFailed, models.MoveHandoffRestored, models.ReservationStateReleased)
	if err != nil {
		return result, fmt.Errorf("%w: restored file ledger update failed: %v", ErrMoveResolutionEvidence, err)
	}
	return result, nil
}

func claimMoveResolution(database *gorm.DB, batch models.RotationQuotaBatch, file models.RotationQuotaBatchFile, request MoveResolutionRequest) (models.RotationQuotaBatchFile, error) {
	token := randomToken()
	started := time.Now()
	result := database.Model(&models.RotationQuotaBatchFile{}).Where("id = ? AND batch_id = ? AND state = ? AND move_handoff_state = ? AND move_resolution_state IN ('', ? ) AND updated_at = ?", file.ID, batch.ID, models.BatchFileStateUnknown, models.MoveHandoffUnknown, models.MoveResolutionFrozen, request.ExpectedUpdatedAt).Updates(map[string]interface{}{"move_resolution_state": models.MoveResolutionResolving, "move_resolution_token": token, "move_resolution_started_at": started, "last_error": "manual move resolution in progress"})
	if result.Error != nil {
		return models.RotationQuotaBatchFile{}, result.Error
	}
	if result.RowsAffected != 1 {
		return models.RotationQuotaBatchFile{}, ErrMoveResolutionConflict
	}
	var claimed models.RotationQuotaBatchFile
	if err := database.First(&claimed, file.ID).Error; err != nil {
		return models.RotationQuotaBatchFile{}, err
	}
	return claimed, nil
}

func recordMoveResolutionEvidence(database *gorm.DB, batchID, fileID uint, message string) error {
	return database.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.RotationQuotaBatchFile{}).Where("id = ? AND batch_id = ? AND state = ?", fileID, batchID, models.BatchFileStateUnknown).Update("last_error", message).Error; err != nil {
			return err
		}
		return tx.Model(&models.RotationQuotaBatch{}).Where("id = ? AND state IN ?", batchID, []string{models.BatchStateUnknown, models.BatchStateReconciling}).Update("last_error", message).Error
	})
}

func commitMoveResolution(database *gorm.DB, batch models.RotationQuotaBatch, file models.RotationQuotaBatchFile, reservation models.QuotaReservation, request MoveResolutionRequest, fileState, handoffState, reservationState string) (MoveResolutionResult, error) {
	err := database.Transaction(func(tx *gorm.DB) error {
		fileQuery := tx.Model(&models.RotationQuotaBatchFile{}).Where("id = ? AND batch_id = ? AND state = ? AND move_handoff_state = ? AND updated_at = ?", file.ID, batch.ID, models.BatchFileStateUnknown, models.MoveHandoffUnknown, request.ExpectedUpdatedAt)
		if request.OperationToken != "" {
			fileQuery = fileQuery.Where("move_resolution_state = ? AND move_resolution_token = ?", models.MoveResolutionResolving, request.OperationToken)
		}
		fileUpdate := fileQuery.Updates(map[string]interface{}{"state": fileState, "move_handoff_state": handoffState, "move_resolution_state": "", "move_resolution_token": "", "move_resolution_started_at": nil, "last_error": ""})
		if fileUpdate.Error != nil {
			return fileUpdate.Error
		}
		if fileUpdate.RowsAffected != 1 {
			return ErrMoveResolutionConflict
		}
		reservationUpdates := map[string]interface{}{"state": reservationState}
		if reservationState == models.ReservationStateReleased {
			reservationUpdates["released_at"] = time.Now()
		}
		reservationUpdate := tx.Model(&models.QuotaReservation{}).Where("id = ? AND batch_id = ? AND batch_file_id = ? AND state = ?", reservation.ID, batch.ID, file.ID, models.ReservationStateUnknown).Updates(reservationUpdates)
		if reservationUpdate.Error != nil {
			return reservationUpdate.Error
		}
		if reservationUpdate.RowsAffected != 1 {
			return ErrMoveResolutionConflict
		}
		var remaining int64
		if err := tx.Model(&models.RotationQuotaBatchFile{}).Where("batch_id = ? AND state = ?", batch.ID, models.BatchFileStateUnknown).Count(&remaining).Error; err != nil {
			return err
		}
		updates := map[string]interface{}{"completion_evidence": models.CompletionEvidenceLocal, "completion_evidence_version": models.MoveCompletionEvidenceVersion, "last_error": fmt.Sprintf("manual move resolution: %s", request.Action)}
		if remaining == 0 {
			var failed int64
			if err := tx.Model(&models.RotationQuotaBatchFile{}).Where("batch_id = ? AND state = ?", batch.ID, models.BatchFileStateFailed).Count(&failed).Error; err != nil {
				return err
			}
			updates["state"] = models.BatchStateSucceeded
			if failed > 0 {
				updates["state"] = models.BatchStateFailed
			}
			updates["finished_at"] = time.Now()
		}
		result := tx.Model(&models.RotationQuotaBatch{}).Where("id = ? AND state IN ?", batch.ID, []string{models.BatchStateUnknown, models.BatchStateReconciling}).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrMoveResolutionConflict
		}
		return nil
	})
	if err != nil {
		return MoveResolutionResult{}, err
	}
	var current models.RotationQuotaBatch
	if err := database.First(&current, batch.ID).Error; err != nil {
		return MoveResolutionResult{}, err
	}
	return MoveResolutionResult{BatchID: batch.ID, FileID: file.ID, State: current.State}, nil
}
