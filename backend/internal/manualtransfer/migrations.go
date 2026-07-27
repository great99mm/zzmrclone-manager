package manualtransfer

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// EnsureSchema upgrades the pre-Oracle Gate 1 shape without touching legacy
// rolling/proactive tables. AutoMigrate adds columns; this function repairs all
// changed manual indexes before recreating the active-run fence.
func EnsureSchema(database *gorm.DB) error {
	if database == nil || !database.Migrator().HasTable(&ManualTransferRun{}) {
		return nil
	}
	return database.Transaction(func(tx *gorm.DB) error {
		for _, index := range []string{
			ManualRunIdempotencyIndex,
			ManualRunActiveIndex,
			ManualRunFilesRunPathIndex,
			ManualRunFilesPathUniqueIndex,
			ManualRunFilesSnapshotUniqueIndex,
			ManualRunAllocationsRunPathIndex,
			ManualRunAllocationsPathUniqueIndex,
			ManualRunAllocationsSnapshotUniqueIndex,
			ManualRunWorkersRunPositionIndex,
			ManualRunWorkerAttemptsNumberIndex,
			ManualRunWorkerFilesPathIndex,
			ManualRunWorkerProgressSequenceIndex,
			ManualRunWorkerLogsWorkerIndex,
		} {
			if err := tx.Exec("DROP INDEX IF EXISTS " + index).Error; err != nil {
				return err
			}
		}
		if err := reconcileDuplicateAnalyzing(tx); err != nil {
			return err
		}
		if err := tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS " + ManualRunIdempotencyIndex + " ON manual_transfer_runs(task_id, idempotency_key)").Error; err != nil {
			return err
		}
		if err := tx.Exec("CREATE INDEX IF NOT EXISTS " + ManualRunFilesRunPathIndex + " ON manual_run_files(run_id, generation, relative_path)").Error; err != nil {
			return err
		}
		if err := tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS " + ManualRunFilesPathUniqueIndex + " ON manual_run_files(run_id, generation, relative_path)").Error; err != nil {
			return err
		}
		if err := tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS " + ManualRunFilesSnapshotUniqueIndex + " ON manual_run_files(run_id, generation, snapshot_key)").Error; err != nil {
			return err
		}
		if tx.Migrator().HasTable(&ManualRunAllocation{}) {
			if err := tx.Exec("CREATE INDEX IF NOT EXISTS " + ManualRunAllocationsRunPathIndex + " ON manual_run_allocations(run_id, generation, relative_path)").Error; err != nil {
				return err
			}
			if err := tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS " + ManualRunAllocationsPathUniqueIndex + " ON manual_run_allocations(run_id, generation, relative_path)").Error; err != nil {
				return err
			}
			if err := tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS " + ManualRunAllocationsSnapshotUniqueIndex + " ON manual_run_allocations(run_id, generation, snapshot_key)").Error; err != nil {
				return err
			}
		}
		if manualTableExists(tx, "manual_run_workers") {
			if err := tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS " + ManualRunWorkersRunPositionIndex + " ON manual_run_workers(run_id, account_position)").Error; err != nil {
				return err
			}
		}
		if manualTableExists(tx, "manual_worker_attempts") {
			if err := tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS " + ManualRunWorkerAttemptsNumberIndex + " ON manual_worker_attempts(worker_id, attempt_number)").Error; err != nil {
				return err
			}
		}
		if manualTableExists(tx, "manual_worker_files") {
			if err := tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS " + ManualRunWorkerFilesPathIndex + " ON manual_worker_files(worker_id, relative_path)").Error; err != nil {
				return err
			}
		}
		if manualTableExists(tx, "manual_worker_progress") {
			if err := tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS " + ManualRunWorkerProgressSequenceIndex + " ON manual_worker_progress(worker_id, sequence)").Error; err != nil {
				return err
			}
		}
		if manualTableExists(tx, "manual_worker_logs") {
			if err := tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS " + ManualRunWorkerLogsWorkerIndex + " ON manual_worker_logs(worker_id)").Error; err != nil {
				return err
			}
		}
		if err := tx.Exec("UPDATE manual_transfer_runs SET snapshot_generation = 1 WHERE state = ? AND snapshot_generation = 0", ManualRunStateAnalyzed).Error; err != nil {
			return err
		}
		if err := tx.Exec("UPDATE manual_run_files SET generation = 1 WHERE generation <= 0").Error; err != nil {
			return err
		}
		if err := tx.Exec(`
			UPDATE manual_run_files
			SET activated_at = COALESCE(
				(SELECT analyzed_at FROM manual_transfer_runs WHERE manual_transfer_runs.id = manual_run_files.run_id),
				CURRENT_TIMESTAMP
			)
			WHERE activated_at IS NULL
			  AND run_id IN (SELECT id FROM manual_transfer_runs WHERE state = ?)
		`, ManualRunStateAnalyzed).Error; err != nil {
			return err
		}
		return tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS " + ManualRunActiveIndex + " ON manual_transfer_runs(task_id) WHERE state IN ('analyzing','allocating')").Error
	})
}

func manualTableExists(tx *gorm.DB, name string) bool {
	var count int64
	if err := tx.Raw("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&count).Error; err != nil {
		return false
	}
	return count > 0
}

func reconcileDuplicateAnalyzing(tx *gorm.DB) error {
	var runs []ManualTransferRun
	if err := tx.Where("state = ?", ManualRunStateAnalyzing).Order("task_id ASC, id ASC").Find(&runs).Error; err != nil {
		return err
	}
	byTask := make(map[uint][]ManualTransferRun)
	for _, run := range runs {
		byTask[run.TaskID] = append(byTask[run.TaskID], run)
	}
	for taskID, taskRuns := range byTask {
		if len(taskRuns) <= 1 {
			continue
		}
		for _, run := range taskRuns[:len(taskRuns)-1] {
			if err := terminalizeMigrationDuplicate(tx, run, taskID); err != nil {
				return err
			}
		}
	}
	return nil
}

func terminalizeMigrationDuplicate(tx *gorm.DB, run ManualTransferRun, taskID uint) error {
	now := time.Now().UTC()
	if err := tx.Where("run_id = ?", run.ID).Delete(&ManualRunFile{}).Error; err != nil {
		return err
	}
	message := "duplicate analyzing run reconciled during manual schema migration; explicit reanalyze required"
	result := tx.Model(&ManualTransferRun{}).Where("id = ? AND task_id = ? AND state = ? AND revision = ?", run.ID, taskID, ManualRunStateAnalyzing, run.Revision).Updates(map[string]interface{}{
		"state":      ManualRunStateAnalysisFailed,
		"revision":   gorm.Expr("revision + 1"),
		"failed_at":  now,
		"last_error": message,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("duplicate analyzing run changed during schema migration")
	}
	return tx.Create(&ManualRunEvent{
		RunID: run.ID, EventType: ManualRunEventMigrationReconciled,
		FromState: ManualRunStateAnalyzing, ToState: ManualRunStateAnalysisFailed,
		ActorIdentity: "system", ActorType: "system", Details: fmt.Sprintf("task %d: %s", taskID, message),
	}).Error
}
