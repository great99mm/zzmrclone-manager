package proactive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/quota"
)

type LocalScanner interface {
	Scan(string, time.Duration) ([]quota.LocalSnapshot, error)
}
type scannerOutcome interface {
	ScanWithOutcome(string, time.Duration) (quota.ScanOutcome, error)
}
type ScannerFactory func(models.Task) LocalScanner
type BatchExecutor interface {
	RunBatch(context.Context, uint) error
}
type RecoveryExecutor interface {
	RecoverBatch(context.Context, uint) error
}
type ProcessInspector interface {
	Inspect(int, string) (ProcessStatus, error)
}
type ProcessController interface {
	StopVerified(int, string) error
}
type ProcessStatus struct {
	Alive     bool
	Confirmed bool
}
type WakeScheduler interface{ ScheduleWake(uint, time.Time) }

var ErrPendingSuperseded = errors.New("proactive rescan was superseded by a newer generation")

type Dispatcher struct {
	DB                             *gorm.DB
	Quota                          *quota.Service
	Executor                       BatchExecutor
	Scanner                        ScannerFactory
	Inspector                      ProcessInspector
	Wake                           WakeScheduler
	Now                            func() time.Time
	RetrySleep                     func(time.Duration)
	RetryDelay                     time.Duration
	RetryMax                       int
	ManagerDataDir                 string
	MoveEnabled                    func() bool
	ScannerLeaseDuration           time.Duration
	ScannerLeaseHeartbeat          time.Duration
	ConfigResolver                 quota.ConfigResolver
	maintenanceRecoveryBeforeClaim func(models.DestinationScopeMaintenance)
	mu                             sync.Mutex
	active                         map[uint]bool
}

var errIncompleteQuotaAccountMapping = errors.New("one or more quota accounts are missing")

func (d *Dispatcher) RequestScan(ctx context.Context, taskID uint) error {
	if d == nil || d.DB == nil || d.Quota == nil || d.Executor == nil {
		return errors.New("proactive dispatcher dependencies are required")
	}
	generation, err := d.markPending(taskID)
	if err != nil {
		return err
	}
	d.mu.Lock()
	if d.active == nil {
		d.active = map[uint]bool{}
	}
	if d.active[taskID] {
		d.mu.Unlock()
		return nil
	}
	d.active[taskID] = true
	d.mu.Unlock()
	defer func() { d.mu.Lock(); delete(d.active, taskID); d.mu.Unlock() }()
	defer d.finishPauseIfRequested(taskID)
	var task models.Task
	if err := d.DB.First(&task, taskID).Error; err != nil {
		return d.persistRequestError(taskID, err)
	}
	if task.RotationStopRequested {
		return nil
	}
	if err := validateProactiveTask(task, d.MoveEnabled != nil && d.MoveEnabled()); err != nil {
		return d.persistRequestError(taskID, err)
	}
	if err := validateSourceOutsideManager(task.SourceDir, d.ManagerDataDir); err != nil {
		return d.persistRequestError(taskID, err)
	}
	resolved := strings.TrimSpace(task.RcloneConfig)
	if d.DB.Migrator().HasTable(&models.DestinationScopeMaintenance{}) {
		if resolved == "" {
			resolved = models.DefaultRcloneConfigPath
		}
		resolved, err = d.Quota.ResolveConfigPath(resolved)
		if err != nil {
			return d.persistRequestError(taskID, err)
		}
		paused, pauseErr := d.maintenancePaused(task, resolved, d.now())
		if pauseErr != nil {
			return d.persistRequestError(taskID, pauseErr)
		}
		if paused {
			return nil
		}
	}
	scope := destinationMaintenanceScope(task, resolved)
	scannerLease, leaseErr := d.acquireScannerLease(scope, d.now())
	if leaseErr != nil {
		if errors.Is(leaseErr, ErrCoordinatorConflict) {
			return d.persistRetryWake(taskID)
		}
		return d.persistRequestError(taskID, leaseErr)
	}
	heartbeat := d.startScannerLeaseHeartbeat(scope, scannerLease)
	defer func() {
		heartbeat.Stop()
		_ = d.releaseScannerLease(scope, scannerLease)
	}()
	if err := heartbeat.Err(); err != nil {
		return d.persistRequestError(taskID, err)
	}
	minAge, err := time.ParseDuration(strings.TrimSpace(task.MinAge))
	if strings.TrimSpace(task.MinAge) == "" {
		minAge = 0
		err = nil
	}
	if err != nil {
		return d.persistRequestError(taskID, err)
	}
	if err := heartbeat.Err(); err != nil {
		return d.persistRequestError(taskID, err)
	}
	scanner := LocalScanner(quota.Scanner{})
	if d.Scanner != nil {
		scanner = d.Scanner(task)
	}
	var snapshots []quota.LocalSnapshot
	var nextEligibleAt *time.Time
	if outcomeScanner, ok := scanner.(scannerOutcome); ok {
		outcome, scanErr := outcomeScanner.ScanWithOutcome(task.SourceDir, minAge)
		snapshots, nextEligibleAt, err = outcome.Snapshots, outcome.NextEligibleAt, scanErr
	} else {
		snapshots, err = scanner.Scan(task.SourceDir, minAge)
	}
	if err != nil {
		return d.persistRequestError(taskID, err)
	}
	if err := heartbeat.Err(); err != nil {
		return d.persistRequestError(taskID, err)
	}
	now := d.now()
	keys, err := completeQuotaKeys(task, resolved)
	if err != nil {
		return d.persistRequestError(taskID, err)
	}
	if err := heartbeat.Err(); err != nil {
		return d.persistRequestError(taskID, err)
	}
	if err := d.upsertAccounts(task, resolved, keys); err != nil {
		return d.persistRequestError(taskID, err)
	}
	task.RcloneConfig = resolved
	task.RotationQuotaKeys = models.EncodeRotationQuotaKeys(keys)
	activeKeys, err := d.activeRequestKeys(taskID)
	if err != nil {
		return d.persistRequestError(taskID, err)
	}
	if len(activeKeys) > 0 {
		if err := heartbeat.Err(); err != nil {
			return d.persistRequestError(taskID, err)
		}
		for _, key := range activeKeys {
			if err := heartbeat.Err(); err != nil {
				return d.persistRequestError(taskID, err)
			}
			if err := d.executeGroup(ctx, taskID, key); err != nil {
				wakeErr := d.persistScopeWake(resolved, task.RemoteDir, now)
				return errors.Join(err, wakeErr, d.persistRequestError(taskID, err))
			}
		}
		if err := heartbeat.Err(); err != nil {
			return d.persistRequestError(taskID, err)
		}
		remaining, blocked, err := d.filterSnapshotKeys(taskID, snapshots)
		if err != nil {
			return d.persistRequestError(taskID, err)
		}
		if len(remaining) == 0 && !blocked {
			if err := heartbeat.Err(); err != nil {
				return d.persistRequestError(taskID, err)
			}
			return d.clearPendingOrWake(taskID, generation)
		}
		if err := heartbeat.Err(); err != nil {
			return d.persistRequestError(taskID, err)
		}
		return d.persistScopeWake(resolved, task.RemoteDir, now)
	}
	eligible, blocked, err := d.filterSnapshotKeys(taskID, snapshots)
	if err != nil {
		return d.persistRequestError(taskID, err)
	}
	if len(eligible) == 0 {
		if err := heartbeat.Err(); err != nil {
			return d.persistRequestError(taskID, err)
		}
		if nextEligibleAt != nil {
			return d.setEarliestWake(taskID, *nextEligibleAt)
		}
		if blocked {
			return d.persistScopeWake(resolved, task.RemoteDir, now)
		}
		return d.clearPendingOrWake(taskID, generation)
	}
	requestKey := randomToken()
	if err := heartbeat.Err(); err != nil {
		return d.persistRequestError(taskID, err)
	}
	result, reserveErr := d.Quota.Reserve(quota.PackReserveRequest{Task: task, Snapshots: eligible, RequestIdempotencyKey: requestKey, SourceRoot: filepath.Clean(task.SourceDir), DestinationPath: task.RemoteDir, CoordinatorLeaseToken: scannerLease})
	if reserveErr != nil {
		if heartbeatErr := heartbeat.Err(); heartbeatErr != nil {
			return d.persistRequestError(taskID, heartbeatErr)
		}
		wakeErr := d.persistScopeWake(resolved, task.RemoteDir, now)
		return errors.Join(reserveErr, wakeErr, d.persistRequestError(taskID, reserveErr))
	}
	if err := heartbeat.Err(); err != nil {
		return d.persistRequestError(taskID, err)
	}
	if result.Classification == models.ReserveClassAccountBlocked {
		if err := heartbeat.Err(); err != nil {
			return d.persistRequestError(taskID, err)
		}
		blockerErr := errors.New("quota account blocked by active or unknown work in another destination scope")
		if err := d.persistPendingState(task, keys, result.Pending, now, generation, false); err != nil {
			return d.persistRequestError(taskID, err)
		}
		if err := d.setTaskError(taskID, blockerErr); err != nil {
			return err
		}
		wake := now.Add(time.Minute)
		if result.RetryAt != nil && result.RetryAt.After(now) {
			wake = *result.RetryAt
		}
		return d.setRetryWake(taskID, wake)
	}
	if err := heartbeat.Err(); err != nil {
		return d.persistRequestError(taskID, err)
	}
	if err := d.updateTaskRetry("id = ?", []interface{}{taskID}, map[string]interface{}{"rotation_last_scan_at": now, "last_run": now, "last_error": ""}); err != nil {
		return d.persistRequestError(taskID, err)
	}
	if len(result.Pending) > 0 {
		if err := heartbeat.Err(); err != nil {
			return d.persistRequestError(taskID, err)
		}
		if err := d.persistPendingState(task, keys, result.Pending, now, generation, len(result.Batches) == 0); err != nil {
			return d.persistRequestError(taskID, err)
		}
	}
	if len(result.Batches) == 0 {
		if err := heartbeat.Err(); err != nil {
			return d.persistRequestError(taskID, err)
		}
		if len(result.Pending) == 0 {
			return d.clearPendingOrWake(taskID, generation)
		} else {
			return d.persistWake(taskID, keys, now)
		}
	}
	if err := d.executeGroup(ctx, taskID, requestKey); err != nil {
		wakeErr := errors.Join(d.persistScopeWake(resolved, task.RemoteDir, now), d.persistAccountWakeForKeys(keys, now, true))
		return errors.Join(err, wakeErr, d.persistRequestError(taskID, err))
	}
	if d.groupTerminal(taskID, requestKey) {
		if err := d.persistAccountWakeForKeys(keys, now, true); err != nil {
			return err
		}
		if len(result.Pending) > 0 {
			// The file-count batch limit leaves eligible files pending even though
			// every batch in this group completed. Continue packing them without
			// waiting for another filesystem event or a quota expiry.
			return d.persistFollowUpWake(taskID)
		}
		if err := heartbeat.Err(); err != nil {
			return d.persistRequestError(taskID, err)
		}
		if err := d.persistScopeWake(resolved, task.RemoteDir, now); err != nil {
			return err
		}
		return d.clearPendingOrWake(taskID, generation)
	} else {
		if err := heartbeat.Err(); err != nil {
			return d.persistRequestError(taskID, err)
		}
		return d.persistWake(taskID, keys, now)
	}
}

func (d *Dispatcher) Recover(ctx context.Context) error {
	if err := d.recoverMaintenanceDedupe(ctx, true); err != nil {
		return err
	}
	var tasks []models.Task
	if err := d.DB.Where("enabled = ? AND task_type = ? AND rotation_strategy = ?", true, "rotation", "proactive_quota").Find(&tasks).Error; err != nil {
		return err
	}
	retryErrors := map[uint]error{}
	for _, task := range tasks {
		if _, err := d.markPending(task.ID); err != nil {
			return err
		}
		if resolved, err := d.Quota.ResolveConfigPath(task.RcloneConfig); err == nil {
			if keys, keyErr := completeQuotaKeys(task, resolved); keyErr == nil {
				if err := d.persistWake(task.ID, keys, d.now()); err != nil {
					return err
				}
			}
		} else {
			return err
		}
	}
	if err := d.recoverDisabledMoveBatches(ctx, tasks); err != nil {
		return err
	}
	for _, task := range tasks {
		var batches []models.RotationQuotaBatch
		if err := d.DB.Where("task_id = ? AND state IN ?", task.ID, []string{models.BatchStateReserved, models.BatchStatePlanned, models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Order("id").Find(&batches).Error; err != nil {
			return err
		}
		blockedGroups := map[string]bool{}
		for _, batch := range batches {
			if batch.TransferMode == models.TransferModeMove {
				if err := d.recoverMoveAtStartup(ctx, batch); err != nil {
					if isRetryableDispatchError(err) {
						if wakeErr := d.persistRetryWake(task.ID); wakeErr != nil {
							return wakeErr
						}
						retryErrors[task.ID] = errors.Join(retryErrors[task.ID], err)
						continue
					}
					if markErr := d.markBatchUnknown(batch.ID, err.Error()); markErr != nil {
						return markErr
					}
					blockedGroups[batch.RequestKey] = true
				}
				continue
			}
			if batch.StartedAt != nil {
				status := ProcessStatus{}
				if d.Inspector == nil {
					if err := d.markBatchUnknown(batch.ID, "process inspector unavailable"); err != nil {
						return err
					}
					blockedGroups[batch.RequestKey] = true
					continue
				}
				var inspectErr error
				status, inspectErr = d.Inspector.Inspect(batch.ProcessID, batch.ProcessStartToken)
				if inspectErr != nil || !status.Confirmed || status.Alive {
					reason := "process identity was not strictly confirmed stopped"
					if inspectErr != nil {
						reason = inspectErr.Error()
					}
					if err := d.markBatchUnknown(batch.ID, reason); err != nil {
						return err
					}
					blockedGroups[batch.RequestKey] = true
					continue
				}
				if recovery, ok := d.Executor.(RecoveryExecutor); ok {
					if err := recovery.RecoverBatch(ctx, batch.ID); err != nil {
						if markErr := d.markBatchUnknown(batch.ID, err.Error()); markErr != nil {
							return markErr
						}
						blockedGroups[batch.RequestKey] = true
					}
				} else {
					if err := d.markBatchUnknown(batch.ID, "recovery executor unavailable"); err != nil {
						return err
					}
					blockedGroups[batch.RequestKey] = true
				}
			}
		}
		keys := map[string]struct{}{}
		for _, batch := range batches {
			keys[batch.RequestKey] = struct{}{}
		}
		orderedKeys := make([]string, 0, len(keys))
		for key := range keys {
			orderedKeys = append(orderedKeys, key)
		}
		sort.Strings(orderedKeys)
		for _, key := range orderedKeys {
			if blockedGroups[key] {
				continue
			}
			if err := d.executeGroup(ctx, task.ID, key); err != nil {
				if isRetryableDispatchError(err) {
					if wakeErr := d.persistRetryWake(task.ID); wakeErr != nil {
						return wakeErr
					}
					if taskErr := d.setTaskError(task.ID, err); taskErr != nil {
						return taskErr
					}
					retryErrors[task.ID] = errors.Join(retryErrors[task.ID], err)
					continue
				}
				resolved, resolveErr := d.Quota.ResolveConfigPath(task.RcloneConfig)
				if resolveErr != nil {
					return resolveErr
				}
				if wakeErr := d.persistScopeWake(resolved, task.RemoteDir, d.now()); wakeErr != nil {
					return wakeErr
				}
				if keys, keyErr := completeQuotaKeys(task, resolved); keyErr != nil {
					return keyErr
				} else if wakeErr := d.persistAccountWakeForKeys(keys, d.now(), true); wakeErr != nil {
					return wakeErr
				}
				return err
			}
		}
		resolved, resolveErr := d.Quota.ResolveConfigPath(task.RcloneConfig)
		if resolveErr != nil {
			return resolveErr
		}
		if wakeErr := d.persistScopeWake(resolved, task.RemoteDir, d.now()); wakeErr != nil {
			return wakeErr
		}
		quotaKeys, keyErr := completeQuotaKeys(task, resolved)
		if keyErr != nil {
			return keyErr
		}
		if wakeErr := d.persistAccountWakeForKeys(quotaKeys, d.now(), true); wakeErr != nil {
			return wakeErr
		}
		if len(blockedGroups) == 0 {
			if err := d.PersistImmediateWake(task.ID); err != nil {
				return err
			}
		}
	}
	if err := d.ProjectStatuses(); err != nil {
		return err
	}
	// Projection is ledger-derived and may clear a prior status message; retain
	// each retryable recovery conflict as observable task-level evidence.
	for taskID, err := range retryErrors {
		if err := d.setTaskError(taskID, err); err != nil {
			return err
		}
	}
	return nil
}

func (d *Dispatcher) recoverMaintenanceDedupe(ctx context.Context, force bool) error {
	if !d.DB.Migrator().HasTable(&models.DestinationScopeMaintenance{}) {
		return nil
	}
	if force {
		var pending []models.DestinationScopeMaintenance
		if err := d.DB.Where("reason = ? AND state = ? AND dedupe_state IN ?", models.MaintenanceReasonQuotaExhaustion, models.MaintenanceStateExhausted, []string{"", models.DedupeStatePending}).Find(&pending).Error; err != nil {
			return err
		}
		for _, row := range pending {
			if err := d.closeRecoveredLegacyQuota(row, "legacy quota exhaustion epoch was stopped before migration"); err != nil {
				return err
			}
		}
	}
	var rows []models.DestinationScopeMaintenance
	query := d.DB.Where("dedupe_state IN ?", []string{models.DedupeStateClaimed, models.DedupeStateRunning})
	if !force {
		query = query.Where("lease_until IS NULL OR lease_until <= ?", d.now())
	}
	if err := query.Find(&rows).Error; err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return nil
		}
		return err
	}
	controller, _ := d.Inspector.(ProcessController)
	for _, row := range rows {
		recoveryNow := d.now()
		if !force && (row.LeaseUntil == nil || row.LeaseUntil.After(recoveryNow)) {
			continue
		}
		if d.maintenanceRecoveryBeforeClaim != nil {
			d.maintenanceRecoveryBeforeClaim(row)
		}
		// Inspect before taking a force-recovery lease. A future lease may still
		// belong to a live server; the exact lease CAS below protects that owner
		// from a concurrent heartbeat between inspection and takeover.
		hasProcess := row.ProcessID > 0 && row.ProcessStartToken != ""
		if hasProcess && d.Inspector != nil {
			if _, inspectErr := d.Inspector.Inspect(row.ProcessID, row.ProcessStartToken); inspectErr != nil {
				// The owned recovery below records this ambiguity as unknown.
			}
		}
		recoveryToken := randomToken()
		recoveryUntil := recoveryNow.Add(2 * time.Minute)
		claim := d.DB.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND state = ? AND dedupe_state = ? AND lease_token = ?", row.ID, models.MaintenanceStateExhausted, row.DedupeState, row.LeaseToken)
		if row.LeaseUntil == nil {
			claim = claim.Where("lease_until IS NULL")
		} else {
			claim = claim.Where("lease_until = ?", *row.LeaseUntil)
			if !force {
				claim = claim.Where("lease_until <= ?", recoveryNow)
			}
		}
		claim = claim.Updates(map[string]interface{}{"lease_token": recoveryToken, "lease_until": recoveryUntil})
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected != 1 {
			continue
		}
		row.LeaseToken = recoveryToken
		row.LeaseUntil = &recoveryUntil
		if row.DedupeState == models.DedupeStateClaimed && !hasProcess {
			if row.Reason == models.MaintenanceReasonManualMerge {
				if err := d.closeRecoveredManual(row, recoveryToken, recoveryUntil, []string{models.DedupeStateClaimed}, "manual merge claim interrupted before process start"); err != nil {
					return err
				}
				continue
			}
			if row.Reason == models.MaintenanceReasonQuotaExhaustion {
				if err := d.closeRecoveredLegacyQuota(row, "legacy quota exhaustion claim had no process"); err != nil {
					return err
				}
				continue
			}
			result := d.DB.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND state = ? AND dedupe_state = ? AND lease_token = ? AND lease_until = ?", row.ID, models.MaintenanceStateExhausted, row.DedupeState, recoveryToken, recoveryUntil).Updates(map[string]interface{}{"dedupe_state": models.DedupeStateFailed, "result": models.DedupeStateFailed, "last_error": "dedupe claim interrupted before process start"})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				continue
			}
			continue
		}
		if row.Reason == models.MaintenanceReasonQuotaExhaustion && hasProcess && d.Inspector != nil {
			if !d.maintenanceRecoveryOwned(row.ID, row.DedupeState, recoveryToken, recoveryUntil) {
				continue
			}
			status, inspectErr := d.Inspector.Inspect(row.ProcessID, row.ProcessStartToken)
			if inspectErr == nil && status.Confirmed && !status.Alive {
				if err := d.closeRecoveredLegacyQuota(row, "legacy quota exhaustion process was verified stopped"); err != nil {
					return err
				}
				continue
			}
			if inspectErr == nil && status.Confirmed && status.Alive && controller != nil {
				if !d.maintenanceRecoveryOwned(row.ID, row.DedupeState, recoveryToken, recoveryUntil) {
					continue
				}
				if stopErr := controller.StopVerified(row.ProcessID, row.ProcessStartToken); stopErr == nil {
					status, inspectErr = d.Inspector.Inspect(row.ProcessID, row.ProcessStartToken)
					if inspectErr == nil && status.Confirmed && !status.Alive {
						if err := d.closeRecoveredLegacyQuota(row, "legacy quota exhaustion process was stopped and verified dead"); err != nil {
							return err
						}
						continue
					}
				} else {
					inspectErr = stopErr
				}
			}
			message := "legacy quota exhaustion process became ambiguous during recovery"
			if inspectErr == nil && status.Confirmed && status.Alive {
				message = "legacy quota exhaustion process is still alive; operator resolution required"
			} else if inspectErr != nil {
				message = inspectErr.Error()
			}
			result := d.DB.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND state = ? AND dedupe_state = ? AND lease_token = ? AND lease_until = ?", row.ID, models.MaintenanceStateExhausted, row.DedupeState, recoveryToken, recoveryUntil).Updates(map[string]interface{}{"dedupe_state": models.DedupeStateUnknown, "result": models.DedupeStateUnknown, "last_error": message})
			if result.Error != nil {
				return result.Error
			}
			continue
		}
		if controller == nil || !hasProcess || d.Inspector == nil {
			result := d.DB.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND state = ? AND dedupe_state = ? AND lease_token = ? AND lease_until = ?", row.ID, models.MaintenanceStateExhausted, row.DedupeState, recoveryToken, recoveryUntil).Updates(map[string]interface{}{"dedupe_state": models.DedupeStateUnknown, "result": models.DedupeStateUnknown, "last_error": "dedupe process identity unavailable during recovery"})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				continue
			}
			continue
		}
		if !d.maintenanceRecoveryOwned(row.ID, row.DedupeState, recoveryToken, recoveryUntil) {
			continue
		}
		status, err := d.Inspector.Inspect(row.ProcessID, row.ProcessStartToken)
		knownDead := err == nil && status.Confirmed && !status.Alive
		if err == nil && status.Confirmed && status.Alive {
			if !d.maintenanceRecoveryOwned(row.ID, row.DedupeState, recoveryToken, recoveryUntil) {
				continue
			}
			if stopErr := controller.StopVerified(row.ProcessID, row.ProcessStartToken); stopErr != nil {
				err = stopErr
			} else {
				status, err = d.Inspector.Inspect(row.ProcessID, row.ProcessStartToken)
				if err == nil && (!status.Confirmed || status.Alive) {
					err = errors.New("dedupe process remained live after recovery stop")
				} else if err == nil {
					knownDead = true
				}
			}
		}
		if row.Reason == models.MaintenanceReasonManualMerge && knownDead && d.maintenanceRecoveryOwned(row.ID, row.DedupeState, recoveryToken, recoveryUntil) {
			if err := d.closeRecoveredManual(row, recoveryToken, recoveryUntil, []string{row.DedupeState}, "manual merge process was verified dead during recovery"); err != nil {
				return err
			}
			continue
		}
		message := "dedupe process became ambiguous during recovery"
		if err != nil {
			message = err.Error()
		}
		result := d.DB.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND state = ? AND dedupe_state = ? AND lease_token = ? AND lease_until = ?", row.ID, models.MaintenanceStateExhausted, row.DedupeState, recoveryToken, recoveryUntil).Updates(map[string]interface{}{"dedupe_state": models.DedupeStateUnknown, "result": models.DedupeStateUnknown, "last_error": message})
		if result.Error != nil {
			return result.Error
		}
	}
	_ = ctx
	return nil
}

func (d *Dispatcher) closeRecoveredLegacyQuota(row models.DestinationScopeMaintenance, message string) error {
	now := d.now()
	return d.DB.Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&models.DestinationScopeMaintenance{}).
			Where("id = ? AND reason = ? AND state = ? AND dedupe_state IN ? AND lease_token = ?", row.ID, models.MaintenanceReasonQuotaExhaustion, models.MaintenanceStateExhausted, []string{"", models.DedupeStatePending, models.DedupeStateClaimed, models.DedupeStateRunning}, row.LeaseToken)
		if row.LeaseUntil == nil {
			query = query.Where("lease_until IS NULL")
		} else {
			query = query.Where("lease_until = ?", *row.LeaseUntil)
		}
		result := query.Updates(map[string]interface{}{"state": models.MaintenanceStateClosed, "dedupe_state": models.DedupeStateFailed, "result": models.DedupeStateFailed, "finished_at": now, "lease_token": "", "lease_until": nil, "revision": gorm.Expr("revision + 1"), "last_error": redactMaintenanceError(message, row)})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		if err := d.wakeResolvedScopeTasks(tx, row.DestinationScope, now); err != nil {
			return err
		}
		var coordinator models.DestinationScopeCoordinator
		if err := tx.Where("destination_scope = ?", row.DestinationScope).First(&coordinator).Error; err == nil {
			if err := tx.Model(&coordinator).Where("maintenance_epoch_id = ?", row.ID).Updates(map[string]interface{}{"maintenance_epoch_id": 0, "revision": gorm.Expr("revision + 1")}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (d *Dispatcher) maintenanceRecoveryOwned(id uint, state, token string, until time.Time) bool {
	var count int64
	if err := d.DB.Model(&models.DestinationScopeMaintenance{}).
		Where("id = ? AND state = ? AND dedupe_state = ? AND lease_token = ? AND lease_until = ?", id, models.MaintenanceStateExhausted, state, token, until).
		Count(&count).Error; err != nil {
		return false
	}
	return count == 1
}

func (d *Dispatcher) recoverDisabledMoveBatches(ctx context.Context, enabledTasks []models.Task) error {
	if _, ok := d.Executor.(RecoveryExecutor); !ok {
		return nil
	}
	enabled := make(map[uint]struct{}, len(enabledTasks))
	for _, task := range enabledTasks {
		enabled[task.ID] = struct{}{}
	}
	var batches []models.RotationQuotaBatch
	if err := d.DB.Where("transfer_mode = ? AND state IN ?", models.TransferModeMove, []string{models.BatchStateReserved, models.BatchStatePlanned, models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Find(&batches).Error; err != nil {
		return err
	}
	for _, batch := range batches {
		if _, ok := enabled[batch.TaskID]; ok {
			continue
		}
		if err := d.recoverMoveAtStartup(ctx, batch); err != nil {
			if isRetryableDispatchError(err) {
				continue
			}
			if batch.StartedAt == nil {
				return err
			}
			if markErr := d.markBatchUnknown(batch.ID, err.Error()); markErr != nil {
				return markErr
			}
			continue
		}
	}
	return nil
}

func (d *Dispatcher) recoverMoveAtStartup(ctx context.Context, batch models.RotationQuotaBatch) error {
	if batch.StartedAt != nil {
		if d.Inspector == nil {
			return errors.New("move process inspector unavailable during startup recovery")
		}
		status, err := d.Inspector.Inspect(batch.ProcessID, batch.ProcessStartToken)
		if err != nil {
			return err
		}
		if !status.Confirmed {
			return errors.New("move process identity was not confirmed during startup recovery")
		}
		if status.Alive {
			controller, ok := d.Inspector.(ProcessController)
			if !ok {
				return errors.New("verified move process controller unavailable during startup recovery")
			}
			if err := controller.StopVerified(batch.ProcessID, batch.ProcessStartToken); err != nil {
				return err
			}
			status, err = d.Inspector.Inspect(batch.ProcessID, batch.ProcessStartToken)
			if err != nil {
				return err
			}
			if !status.Confirmed || status.Alive {
				return errors.New("move process remained live after startup recovery stop")
			}
		}
	}
	recovery, ok := d.Executor.(RecoveryExecutor)
	if !ok {
		return errors.New("move recovery executor unavailable")
	}
	return recovery.RecoverBatch(ctx, batch.ID)
}

// StopLiveMoveProcesses is used by task StopAndWait. It controls only a
// process whose persisted PID/start token still identifies the same process;
// any ambiguity freezes the batch instead of sending a signal.
func (d *Dispatcher) StopLiveMoveProcesses(ctx context.Context, taskID uint) error {
	controller, ok := d.Inspector.(ProcessController)
	if !ok {
		return d.freezeLiveMoves(taskID, "verified move process controller unavailable")
	}
	var batches []models.RotationQuotaBatch
	if err := d.DB.Where("task_id = ? AND transfer_mode = ? AND state IN ?", taskID, models.TransferModeMove, []string{models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Find(&batches).Error; err != nil {
		return err
	}
	recovery, canRecover := d.Executor.(RecoveryExecutor)
	for _, batch := range batches {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		if batch.ProcessID <= 0 || batch.ProcessStartToken == "" {
			if err := d.markBatchUnknown(batch.ID, "move process identity is missing during stop"); err != nil {
				return err
			}
			continue
		}
		status, err := d.Inspector.Inspect(batch.ProcessID, batch.ProcessStartToken)
		if err != nil || !status.Confirmed {
			if err == nil {
				err = errors.New("move process identity was not confirmed during stop")
			}
			if markErr := d.markBatchUnknown(batch.ID, err.Error()); markErr != nil {
				return markErr
			}
			continue
		}
		if status.Alive {
			if err := controller.StopVerified(batch.ProcessID, batch.ProcessStartToken); err != nil {
				if markErr := d.markBatchUnknown(batch.ID, err.Error()); markErr != nil {
					return markErr
				}
				continue
			}
		}
		final, inspectErr := d.Inspector.Inspect(batch.ProcessID, batch.ProcessStartToken)
		if inspectErr != nil || !final.Confirmed || final.Alive {
			reason := "move process remained live or identity became ambiguous after stop"
			if inspectErr != nil {
				reason = inspectErr.Error()
			}
			if markErr := d.markBatchUnknown(batch.ID, reason); markErr != nil {
				return markErr
			}
			continue
		}
		if canRecover {
			if err := recovery.RecoverBatch(ctx, batch.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Dispatcher) freezeLiveMoves(taskID uint, reason string) error {
	var batches []models.RotationQuotaBatch
	if err := d.DB.Where("task_id = ? AND transfer_mode = ? AND state IN ?", taskID, models.TransferModeMove, []string{models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Find(&batches).Error; err != nil {
		return err
	}
	for _, batch := range batches {
		if err := d.markBatchUnknown(batch.ID, reason); err != nil {
			return err
		}
	}
	return nil
}

// MarkPending records a new generation without taking ownership of the
// process. Repeated proactive events use this while the existing owner runs.
func (d *Dispatcher) MarkPending(taskID uint) (uint64, error) { return d.markPending(taskID) }

// PersistImmediateWake makes an enabled pending task eligible for the durable
// consumer without requiring an in-memory timer.
func (d *Dispatcher) PersistImmediateWake(taskID uint) error { return d.persistImmediateWake(taskID) }

// ProjectStatuses derives task status from the durable quota ledger. It must
// run after recovery and before trigger registration.
func (d *Dispatcher) ProjectStatuses() error {
	var accountIDs []uint
	if err := d.DB.Model(&models.QuotaAccount{}).Pluck("id", &accountIDs).Error; err != nil {
		return err
	}
	if _, err := quota.AdvanceAccountWindows(d.DB, accountIDs, d.now()); err != nil {
		return err
	}
	var tasks []models.Task
	if err := d.DB.Where("enabled = ? AND task_type = ? AND rotation_strategy = ?", true, "rotation", "proactive_quota").Find(&tasks).Error; err != nil {
		return err
	}
	for _, task := range tasks {
		var batches []models.RotationQuotaBatch
		if err := d.DB.Where("task_id = ?", task.ID).Order("id DESC").Find(&batches).Error; err != nil {
			return err
		}
		status, message := "idle", ""
		for _, batch := range batches {
			if batch.State == models.BatchStateUnknown {
				status, message = "error", batch.LastError
				if message == "" {
					message = "quota batch is unknown"
				}
				break
			}
			if models.IsActiveBatchState(batch.State) {
				status = "running"
			}
		}
		if err := d.DB.Model(&models.Task{}).Where("id = ?", task.ID).Updates(map[string]interface{}{"status": status, "last_error": message}).Error; err != nil {
			return err
		}
	}
	return nil
}

type snapshotClaim struct {
	SnapshotKey      string
	BatchState       string
	FileState        string
	ReservationState string
}

func (d *Dispatcher) activeRequestKeys(taskID uint) ([]string, error) {
	var rows []struct{ RequestKey string }
	if err := d.DB.Model(&models.RotationQuotaBatch{}).Select("DISTINCT request_key").Where("task_id = ? AND state IN ?", taskID, []string{models.BatchStateReserved, models.BatchStatePlanned, models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Order("request_key").Find(&rows).Error; err != nil {
		return nil, err
	}
	keys := make([]string, len(rows))
	for i, row := range rows {
		keys[i] = row.RequestKey
	}
	return keys, nil
}

func (d *Dispatcher) filterSnapshotKeys(taskID uint, snapshots []quota.LocalSnapshot) ([]quota.LocalSnapshot, bool, error) {
	var claims []snapshotClaim
	err := d.DB.Table("rotation_quota_batch_files AS f").Select("f.snapshot_key, b.state AS batch_state, f.state AS file_state, r.state AS reservation_state").Joins("JOIN rotation_quota_batches AS b ON b.id = f.batch_id").Joins("LEFT JOIN quota_reservations AS r ON r.batch_file_id = f.id").Where("b.task_id = ?", taskID).Find(&claims).Error
	if err != nil {
		return nil, false, err
	}
	var oversized []models.RotationQuotaOversize
	if err := d.DB.Where("task_id = ?", taskID).Find(&oversized).Error; err != nil {
		return nil, false, err
	}
	oversize := map[string]string{}
	for _, item := range oversized {
		oversize[item.RelativePath] = item.SnapshotKey
	}
	committed, blocked := map[string]struct{}{}, map[string]struct{}{}
	for _, claim := range claims {
		if claim.BatchState == models.BatchStateUnknown || !models.IsTerminalBatchState(claim.BatchState) || claim.FileState == models.BatchFileStateUnknown || claim.ReservationState == models.ReservationStateUnknown {
			blocked[claim.SnapshotKey] = struct{}{}
		}
		if claim.BatchState == models.BatchStateSucceeded && claim.FileState == models.BatchFileStateCommitted && claim.ReservationState == models.ReservationStateCommitted {
			committed[claim.SnapshotKey] = struct{}{}
		}
	}
	eligible := make([]quota.LocalSnapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if key, ok := oversize[snapshot.RelativePath]; ok && key == snapshot.SnapshotKey {
			continue
		}
		if _, ok := blocked[snapshot.SnapshotKey]; ok {
			continue
		}
		if _, ok := committed[snapshot.SnapshotKey]; ok {
			continue
		}
		eligible = append(eligible, snapshot)
	}
	return eligible, len(blocked) > 0, nil
}

func (d *Dispatcher) IsActive(taskID uint) bool {
	d.mu.Lock()
	active := d.active[taskID]
	d.mu.Unlock()
	if active {
		return true
	}
	var count int64
	_ = d.DB.Model(&models.RotationQuotaBatch{}).Where("task_id = ? AND state IN ?", taskID, []string{models.BatchStateReserved, models.BatchStatePlanned, models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Count(&count).Error
	return count > 0
}

func validateProactiveTask(task models.Task, moveEnabled bool) error {
	if task.TaskType != "rotation" || task.RotationStrategy != "proactive_quota" {
		return errors.New("task is not proactive quota rotation")
	}
	if task.SourceType != "" && task.SourceType != "local" || task.DestType != "" && task.DestType != "remote" {
		return errors.New("proactive task requires local source and remote destination")
	}
	if !filepath.IsAbs(task.SourceDir) || filepath.Clean(task.SourceDir) != task.SourceDir || strings.TrimSpace(task.RemoteName) == "" {
		return errors.New("proactive task source/destination is invalid")
	}
	if task.QBEnabled {
		return errors.New("proactive task must have qB disabled")
	}
	if task.TransferMode != models.TransferModeCopy && (task.TransferMode != models.TransferModeMove || !moveEnabled) {
		return errors.New("proactive task transfer mode is invalid or move is disabled")
	}
	return nil
}

func validateSourceOutsideManager(source, manager string) error {
	if strings.TrimSpace(manager) == "" {
		return nil
	}
	source = filepath.Clean(source)
	manager = filepath.Clean(manager)
	if !filepath.IsAbs(source) || !filepath.IsAbs(manager) {
		return errors.New("proactive manager/source paths must be absolute")
	}
	overlaps := func(a, b string) bool {
		rel, err := filepath.Rel(a, b)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "." || err == nil && rel == "."
	}
	if overlaps(manager, source) || overlaps(source, manager) {
		return fmt.Errorf("proactive source overlaps manager data directory: %s", source)
	}
	return nil
}

func ValidateSourceOutsideManager(source, manager string) error {
	return validateSourceOutsideManager(source, manager)
}

func completeQuotaKeys(task models.Task, resolved string) (map[string]string, error) {
	remotes := models.ParseRotationRemotes(task.RotationRemotes)
	if len(remotes) == 0 {
		return nil, errors.New("rotation remotes are required")
	}
	explicit, err := models.ParseRotationQuotaKeys(task.RotationQuotaKeys)
	if err != nil {
		return nil, err
	}
	keys := map[string]string{}
	for _, remote := range remotes {
		key := strings.TrimSpace(explicit[remote])
		if key == "" {
			key = defaultQuotaKey(resolved, remote)
		}
		keys[remote] = key
	}
	return keys, nil
}

// CompleteQuotaKeysFromAccounts resolves status bindings from the durable
// account identities. This is used when a task has not yet persisted the
// optional key mapping but the dispatcher has already created its account.
func CompleteQuotaKeysFromAccounts(task models.Task, accounts []models.QuotaAccount, resolvedIdentity string) (map[string]string, error) {
	remotes := models.ParseRotationRemotes(task.RotationRemotes)
	if len(remotes) == 0 {
		return nil, errors.New("rotation remotes are required")
	}
	explicit, err := models.ParseRotationQuotaKeys(task.RotationQuotaKeys)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]string, len(remotes))
	for _, remote := range remotes {
		key := strings.TrimSpace(explicit[remote])
		if key == "" {
			for _, account := range accounts {
				if resolvedIdentity == "" || account.ConfigIdentity != resolvedIdentity || (account.RemoteName != "" && account.RemoteName != remote) {
					continue
				}
				if models.DefaultRotationQuotaKey(resolvedIdentity, remote) == account.QuotaKey {
					key = account.QuotaKey
					break
				}
			}
		}
		if key == "" {
			key = models.DefaultRotationQuotaKey(resolvedIdentity, remote)
		}
		keys[remote] = key
	}
	return keys, nil
}

func defaultQuotaKey(config, remote string) string {
	return models.DefaultRotationQuotaKey(config, remote)
}

func (d *Dispatcher) upsertAccounts(task models.Task, resolved string, keys map[string]string) error {
	var lastErr error
	for attempt := 0; attempt < d.retryMax(); attempt++ {
		lastErr = d.upsertAccountsAttempt(resolved, keys)
		if lastErr == nil {
			return nil
		}
		if !retryableSQLiteError(lastErr) {
			return lastErr
		}
		d.retrySleep(attempt)
	}
	return lastErr
}

func (d *Dispatcher) upsertAccountsAttempt(resolved string, keys map[string]string) error {
	return d.DB.Transaction(func(tx *gorm.DB) error {
		remotes := make([]string, 0, len(keys))
		for remote := range keys {
			remotes = append(remotes, remote)
		}
		sort.Strings(remotes)
		for _, remote := range remotes {
			key := keys[remote]
			var account models.QuotaAccount
			err := tx.Where("quota_key = ?", key).First(&account).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				if err := tx.Create(&models.QuotaAccount{QuotaKey: key, RemoteName: remote, ConfigIdentity: resolved, BudgetBytes: models.DefaultRotationQuotaLimitBytes, WindowSeconds: 86400, Enabled: true}).Error; err != nil {
					return err
				}
			} else if err != nil {
				return err
			} else {
				remoteName, configIdentity := account.RemoteName, account.ConfigIdentity
				if remoteName == "" || remote < remoteName {
					remoteName = remote
				}
				if configIdentity == "" || resolved < configIdentity {
					configIdentity = resolved
				}
				if err := tx.Model(&account).Updates(map[string]interface{}{"remote_name": remoteName, "config_identity": configIdentity}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func retryableSQLiteError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database table is locked") || strings.Contains(message, "unique constraint") || strings.Contains(message, "constraint failed")
}

func scanRequestKey(taskID uint, snapshots []quota.LocalSnapshot) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\x00", taskID)
	ordered := append([]quota.LocalSnapshot(nil), snapshots...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].RelativePath < ordered[j].RelativePath })
	for _, s := range ordered {
		fmt.Fprintf(h, "%s\x00%s\x00", s.RelativePath, s.SnapshotKey)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func firstRequestKey(batches []models.RotationQuotaBatch) string {
	if len(batches) > 0 {
		return batches[0].RequestKey
	}
	return ""
}
func (d *Dispatcher) executeGroup(ctx context.Context, taskID uint, requestKey string) error {
	if requestKey == "" {
		return nil
	}
	var task models.Task
	if err := d.DB.Select("rotation_concurrent_batches").First(&task, taskID).Error; err != nil {
		return err
	}
	limit := task.RotationConcurrentBatches
	if limit <= 0 {
		limit = 1
	}

	for {
		var batches []models.RotationQuotaBatch
		if err := d.DB.Where("task_id = ? AND request_key = ?", taskID, requestKey).Order("id").Find(&batches).Error; err != nil {
			return err
		}
		candidates := make([]models.RotationQuotaBatch, 0, limit)
		accounts := make(map[uint]struct{}, limit)
		for i, batch := range batches {
			switch batch.State {
			case models.BatchStateUnknown, models.BatchStateFailed:
				if err := d.cancelLaterHeld(batches[i+1:]); err != nil {
					return err
				}
				return d.persistRetryWake(taskID)
			case models.BatchStateRunning, models.BatchStateReconciling:
				return nil
			case models.BatchStateReserved, models.BatchStatePlanned:
				if len(candidates) >= limit {
					continue
				}
				if _, exists := accounts[batch.QuotaAccountID]; exists {
					continue
				}
				accounts[batch.QuotaAccountID] = struct{}{}
				candidates = append(candidates, batch)
			}
		}
		if len(candidates) == 0 {
			return nil
		}

		type batchExecutionResult struct {
			batchID uint
			err     error
		}
		results := make(chan batchExecutionResult, len(candidates))
		var wg sync.WaitGroup
		for _, batch := range candidates {
			wg.Add(1)
			go func(batchID uint) {
				defer wg.Done()
				results <- batchExecutionResult{batchID: batchID, err: d.Executor.RunBatch(ctx, batchID)}
			}(batch.ID)
		}
		wg.Wait()
		close(results)
		for result := range results {
			if result.err == nil {
				continue
			}
			if errors.Is(result.err, ErrAccountBlocked) {
				if err := d.cancelBlockedHeld(result.batchID); err != nil {
					return errors.Join(result.err, err)
				}
			}
			return result.err
		}
		for _, batch := range candidates {
			var current models.RotationQuotaBatch
			if err := d.DB.Select("state").First(&current, batch.ID).Error; err != nil {
				return err
			}
			if current.State != models.BatchStateSucceeded && current.State != models.BatchStateFailed && current.State != models.BatchStateUnknown {
				return nil
			}
		}
		var task models.Task
		if err := d.DB.Select("rotation_stop_requested").First(&task, taskID).Error; err != nil {
			return err
		}
		if task.RotationStopRequested {
			return nil
		}
	}
}

// cancelBlockedHeld releases only work that has not crossed the durable start
// intent. ReleaseHeldBatch rechecks the same pre-start predicates in its
// transaction, so active or unknown work is never released by this path.
func (d *Dispatcher) cancelBlockedHeld(batchID uint) error {
	var batch models.RotationQuotaBatch
	if err := d.DB.Select("quota_account_id").First(&batch, batchID).Error; err != nil {
		return err
	}
	err := d.Quota.ReleaseHeldBatch(batchID)
	if err == nil || errors.Is(err, quota.ErrHeldBatchNotSafe) || strings.Contains(strings.ToLower(err.Error()), "not an unstarted held batch") {
		if err != nil {
			return nil
		}
		return d.persistAccountWakeForAccounts([]uint{batch.QuotaAccountID}, d.now(), true)
	}
	return err
}

func (d *Dispatcher) finishPauseIfRequested(taskID uint) {
	var task models.Task
	if err := d.DB.Select("rotation_stop_requested").First(&task, taskID).Error; err != nil || !task.RotationStopRequested {
		return
	}
	var active int64
	if err := d.DB.Model(&models.RotationQuotaBatch{}).Where("task_id = ? AND state IN ?", taskID, []string{models.BatchStateRunning, models.BatchStateReconciling}).Count(&active).Error; err != nil || active > 0 {
		return
	}
	_ = d.DB.Model(&models.Task{}).Where("id = ? AND rotation_stop_requested = ?", taskID, true).Updates(map[string]interface{}{"status": "paused", "rotation_rescan_pending": false, "rotation_quota_wake_at": nil}).Error
}

func (d *Dispatcher) cancelLaterHeld(batches []models.RotationQuotaBatch) error {
	for _, batch := range batches {
		if batch.StartedAt == nil && (batch.State == models.BatchStateReserved || batch.State == models.BatchStatePlanned) {
			if err := d.cancelBlockedHeld(batch.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func isRetryableDispatchError(err error) bool {
	if errors.Is(err, ErrAccountBlocked) || errors.Is(err, ErrLeaseConflict) || errors.Is(err, ErrRetryableExecutor) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unknown owner") || strings.Contains(message, "unknown scope owner") || strings.Contains(message, "scope") && (strings.Contains(message, "owned") || strings.Contains(message, "owner") || strings.Contains(message, "lease"))
}

func (d *Dispatcher) groupTerminal(taskID uint, requestKey string) bool {
	var batches []models.RotationQuotaBatch
	if d.DB.Where("task_id = ? AND request_key = ?", taskID, requestKey).Find(&batches).Error != nil || len(batches) == 0 {
		return false
	}
	for _, batch := range batches {
		if batch.State != models.BatchStateSucceeded {
			return false
		}
	}
	return true
}

func (d *Dispatcher) persistRetryWake(taskID uint) error {
	return d.setEarliestWake(taskID, d.now().Add(time.Minute))
}

func (d *Dispatcher) markPending(id uint) (uint64, error) {
	var generation uint64
	var err error
	for attempt := 0; attempt < d.retryMax(); attempt++ {
		generation, err = d.markPendingOnce(id)
		if err == nil {
			return generation, nil
		}
		if !retryableSQLiteError(err) && !errors.Is(err, ErrPendingSuperseded) {
			return generation, err
		}
		d.retrySleep(attempt)
	}
	return generation, err
}
func (d *Dispatcher) markPendingOnce(id uint) (uint64, error) {
	var generation uint64
	err := d.DB.Transaction(func(tx *gorm.DB) error {
		var task models.Task
		if err := tx.First(&task, id).Error; err != nil {
			return err
		}
		generation = task.RotationRescanGeneration + 1
		result := tx.Model(&models.Task{}).Where("id = ? AND rotation_rescan_generation = ?", id, task.RotationRescanGeneration).Updates(map[string]interface{}{"rotation_rescan_pending": true, "rotation_rescan_generation": generation})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrPendingSuperseded
		}
		return nil
	})
	return generation, err
}
func (d *Dispatcher) clearPending(id uint, generation uint64) error {
	for attempt := 0; attempt < d.retryMax(); attempt++ {
		result := d.DB.Model(&models.Task{}).Where("id = ? AND rotation_rescan_generation = ?", id, generation).Updates(map[string]interface{}{"rotation_rescan_pending": false, "rotation_rescan_handled_generation": generation, "rotation_quota_wake_at": nil})
		if result.Error == nil {
			if result.RowsAffected != 1 {
				return ErrPendingSuperseded
			}
			return nil
		}
		if !retryableSQLiteError(result.Error) {
			return result.Error
		}
		d.retrySleep(attempt)
	}
	return errors.New("pending clear retries exhausted")
}
func (d *Dispatcher) clearPendingOrWake(id uint, generation uint64) error {
	err := d.clearPending(id, generation)
	if !errors.Is(err, ErrPendingSuperseded) {
		return err
	}
	wakeErr := d.persistImmediateWake(id)
	return errors.Join(err, wakeErr)
}

// ClearStop releases a durable stop request. The following RequestScan creates
// the new rescan generation, so a stopped task cannot be resumed implicitly.
func (d *Dispatcher) ClearStop(id uint, expected ...uint64) error {
	if len(expected) == 0 {
		return d.updateTaskRetry("id = ?", []interface{}{id}, map[string]interface{}{"rotation_stop_requested": false})
	}
	result := d.DB.Model(&models.Task{}).Where("id = ? AND rotation_stop_generation = ?", id, expected[0]).Updates(map[string]interface{}{"rotation_stop_requested": false})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrPendingSuperseded
	}
	return nil
}

// RequestStop records the durable stop intent and returns the generation that
// a caller may safely clear after its running work has reconciled.
func (d *Dispatcher) RequestStop(id uint) (uint64, error) {
	var generation uint64
	err := d.DB.Transaction(func(tx *gorm.DB) error {
		var task models.Task
		if err := tx.First(&task, id).Error; err != nil {
			return err
		}
		generation = task.RotationRescanGeneration
		result := tx.Model(&models.Task{}).Where("id = ? AND rotation_stop_generation = ?", id, task.RotationStopGeneration).Updates(map[string]interface{}{
			"rotation_stop_requested":  true,
			"rotation_stop_generation": task.RotationStopGeneration + 1,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrPendingSuperseded
		}
		return nil
	})
	return generation, err
}

// ClearPendingGeneration is used by StopAndWait after process reconciliation.
// A newer trigger generation is deliberately left untouched.
func (d *Dispatcher) ClearPendingGeneration(id uint, generation uint64) error {
	return d.clearPending(id, generation)
}

func (d *Dispatcher) retryMax() int {
	if d.RetryMax > 0 {
		return d.RetryMax
	}
	return 8
}
func (d *Dispatcher) retrySleep(attempt int) {
	delay := d.RetryDelay
	if delay <= 0 {
		delay = 50 * time.Millisecond
	}
	for i := 0; i < attempt; i++ {
		delay *= 2
	}
	if d.RetrySleep != nil {
		d.RetrySleep(delay)
	} else {
		time.Sleep(delay)
	}
}
func (d *Dispatcher) updateTaskRetry(where string, args []interface{}, values map[string]interface{}) error {
	for attempt := 0; attempt < d.retryMax(); attempt++ {
		result := d.DB.Model(&models.Task{}).Where(where, args...).Updates(values)
		if result.Error == nil {
			return nil
		}
		if !retryableSQLiteError(result.Error) {
			return result.Error
		}
		d.retrySleep(attempt)
	}
	return errors.New("task update retries exhausted")
}

func (d *Dispatcher) setEarliestWake(id uint, candidate time.Time) error {
	for attempt := 0; attempt < d.retryMax(); attempt++ {
		var chosen time.Time
		err := d.DB.Transaction(func(tx *gorm.DB) error {
			var task models.Task
			if err := tx.First(&task, id).Error; err != nil {
				return err
			}
			if !task.RotationRescanPending || task.RotationStopRequested {
				return nil
			}
			chosen = candidate
			if task.RotationQuotaWakeAt != nil && task.RotationQuotaWakeAt.After(d.now()) && task.RotationQuotaWakeAt.Before(candidate) {
				chosen = *task.RotationQuotaWakeAt
			}
			result := tx.Model(&models.Task{}).Where("id = ? AND rotation_rescan_generation = ? AND rotation_rescan_pending = ?", id, task.RotationRescanGeneration, true).Update("rotation_quota_wake_at", chosen)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrPendingSuperseded
			}
			return nil
		})
		if err == nil {
			if !chosen.IsZero() && d.Wake != nil {
				d.Wake.ScheduleWake(id, chosen)
			}
			return nil
		}
		if !retryableSQLiteError(err) && !errors.Is(err, ErrPendingSuperseded) {
			return err
		}
		d.retrySleep(attempt)
	}
	return errors.New("wake update retries exhausted")
}

func (d *Dispatcher) setRetryWake(id uint, candidate time.Time) error {
	now := d.now()
	if !candidate.After(now) {
		candidate = now.Add(time.Minute)
	}
	return d.setEarliestWake(id, candidate)
}
func (d *Dispatcher) persistImmediateWake(id uint) error {
	return d.setEarliestWake(id, d.now().Add(time.Minute))
}

func (d *Dispatcher) persistFollowUpWake(id uint) error {
	return d.setEarliestWake(id, d.now())
}
func (d *Dispatcher) setTaskError(id uint, err error) error {
	return d.updateTaskRetry("id = ?", []interface{}{id}, map[string]interface{}{"last_error": err.Error()})
}

func (d *Dispatcher) persistRequestError(id uint, original error) error {
	if original == nil {
		return nil
	}
	errs := []error{original}
	if err := d.setTaskError(id, original); err != nil {
		errs = append(errs, err)
	}
	var task models.Task
	if err := d.DB.First(&task, id).Error; err == nil && !task.RotationStopRequested {
		wake := d.now().Add(time.Minute)
		if err := d.setEarliestWake(id, wake); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
func (d *Dispatcher) persistPendingState(task models.Task, keys map[string]string, pending []quota.LocalSnapshot, now time.Time, generation uint64, allowClear bool) error {
	max := task.RotationQuotaLimitBytes
	var accounts []models.QuotaAccount
	if err := d.DB.Where("quota_key IN ?", mapValues(keys)).Find(&accounts).Error; err != nil {
		return err
	}
	allOversized := true
	permanentSnapshots := make([]quota.LocalSnapshot, 0)
	for _, s := range pending {
		permanent := s.SizeBytes > max
		if !permanent {
			fits := false
			for _, a := range accounts {
				limit := a.BudgetBytes
				if max < limit {
					limit = max
				}
				if s.SizeBytes <= limit {
					fits = true
					break
				}
			}
			permanent = !fits
		}
		if permanent {
			permanentSnapshots = append(permanentSnapshots, s)
		}
		if !permanent {
			allOversized = false
		}
	}
	for _, snapshot := range permanentSnapshots {
		var record models.RotationQuotaOversize
		result := d.DB.Where("task_id = ? AND relative_path = ?", task.ID, snapshot.RelativePath).First(&record)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			result = d.DB.Create(&models.RotationQuotaOversize{TaskID: task.ID, RelativePath: snapshot.RelativePath, SnapshotKey: snapshot.SnapshotKey, SizeBytes: snapshot.SizeBytes})
		} else if result.Error == nil {
			result = d.DB.Model(&record).Updates(map[string]interface{}{"snapshot_key": snapshot.SnapshotKey, "size_bytes": snapshot.SizeBytes})
		}
		if result.Error != nil {
			return result.Error
		}
	}
	if len(permanentSnapshots) > 0 {
		if err := d.setTaskError(task.ID, errors.New("pending file exceeds every eligible quota account or task cap")); err != nil {
			return err
		}
	}
	if allOversized && allowClear {
		return d.clearPendingOrWake(task.ID, generation)
	}
	return d.persistWake(task.ID, keys, now)
}
func mapValues(m map[string]string) []string {
	r := make([]string, 0, len(m))
	for _, v := range m {
		r = append(r, v)
	}
	return r
}
func (d *Dispatcher) persistWake(taskID uint, keys map[string]string, now time.Time) error {
	if err := advanceAccountWindowsForKeys(d.DB, keys, now); err != nil {
		var accountCount int64
		if countErr := d.DB.Model(&models.QuotaAccount{}).Where("quota_key IN ?", mapValues(keys)).Count(&accountCount).Error; countErr != nil {
			return countErr
		}
		if accountCount == 0 {
			return d.setEarliestWake(taskID, now.Add(time.Minute))
		}
		return err
	}
	wake, err := d.computeWake(keys, now)
	if err != nil {
		return err
	}
	if wake != nil {
		if err := d.setEarliestWake(taskID, *wake); err != nil {
			return err
		}
	}
	var task models.Task
	if err := d.DB.First(&task, taskID).Error; err != nil {
		return err
	}
	resolved, err := d.Quota.ResolveConfigPath(task.RcloneConfig)
	if err != nil {
		return err
	}
	return d.persistScopeWake(resolved, task.RemoteDir, now)
}

func (d *Dispatcher) persistScopeWakeForTask(task models.Task, _ map[string]string, now time.Time) error {
	resolved, err := d.Quota.ResolveConfigPath(task.RcloneConfig)
	if err != nil {
		return err
	}
	return d.persistScopeWake(resolved, task.RemoteDir, now)
}
func (d *Dispatcher) persistScopeWake(resolved, destination string, now time.Time) error {
	var tasks []models.Task
	if err := d.DB.Where("enabled = ? AND task_type = ? AND rotation_strategy = ? AND rotation_rescan_pending = ?", true, "rotation", "proactive_quota", true).Find(&tasks).Error; err != nil {
		return err
	}
	scope := models.DestinationScope(resolved, destination)
	for _, task := range tasks {
		other, err := d.Quota.ResolveConfigPath(task.RcloneConfig)
		if err != nil {
			return err
		}
		if models.DestinationScope(other, task.RemoteDir) != scope {
			continue
		}
		keys, err := completeQuotaKeys(task, other)
		if err != nil {
			return err
		}
		var accountCount int64
		if err := d.DB.Model(&models.QuotaAccount{}).Where("quota_key IN ?", mapValues(keys)).Count(&accountCount).Error; err != nil {
			return err
		}
		if accountCount == 0 {
			continue
		}
		if err := advanceAccountWindowsForKeys(d.DB, keys, now); err != nil {
			return err
		}
		wake, err := d.computeWake(keys, now)
		if err != nil {
			return err
		}
		if wake == nil {
			var active int64
			if err := d.DB.Model(&models.RotationQuotaBatch{}).Where("destination_scope = ? AND state IN ?", scope, []string{models.BatchStateReserved, models.BatchStatePlanned, models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Count(&active).Error; err != nil {
				return err
			}
			if active > 0 {
				value := now.Add(time.Minute)
				wake = &value
			} else {
				continue
			}
		}
		if err := d.setEarliestWake(task.ID, *wake); err != nil {
			return err
		}
	}
	return nil
}

func (d *Dispatcher) persistAccountWakeForKeys(keys map[string]string, now time.Time, immediate bool) error {
	accountIDs, err := quotaAccountIDsForKeys(d.DB, keys)
	if err != nil {
		if errors.Is(err, errIncompleteQuotaAccountMapping) {
			return nil
		}
		return err
	}
	return d.persistAccountWakeForAccounts(accountIDs, now, immediate)
}

// WakeQuotaAccounts is called after an external ledger transition has
// committed, such as manual move resolution. It deliberately runs outside the
// resolution transaction so pending-task wake updates cannot be rolled back
// with the already durable resolution.
func (d *Dispatcher) WakeQuotaAccounts(accountIDs []uint) error {
	if d == nil || d.DB == nil || d.Quota == nil {
		return errors.New("proactive dispatcher quota dependencies are required")
	}
	return d.persistAccountWakeForAccounts(accountIDs, d.now(), true)
}

func (d *Dispatcher) persistAccountWakeForAccounts(accountIDs []uint, now time.Time, immediate bool) error {
	if len(accountIDs) == 0 {
		return nil
	}
	wanted := make(map[uint]struct{}, len(accountIDs))
	for _, accountID := range accountIDs {
		wanted[accountID] = struct{}{}
	}
	var tasks []models.Task
	if err := d.DB.Where("enabled = ? AND task_type = ? AND rotation_strategy = ? AND rotation_rescan_pending = ?", true, "rotation", "proactive_quota", true).Find(&tasks).Error; err != nil {
		return err
	}
	for _, task := range tasks {
		resolved, err := d.Quota.ResolveConfigPath(task.RcloneConfig)
		if err != nil {
			return err
		}
		keys, err := completeQuotaKeys(task, resolved)
		if err != nil {
			return err
		}
		taskAccountIDs, err := quotaAccountIDsForKeys(d.DB, keys)
		if err != nil {
			if errors.Is(err, errIncompleteQuotaAccountMapping) {
				continue
			}
			return err
		}
		sharesAccount := false
		for _, accountID := range taskAccountIDs {
			if _, ok := wanted[accountID]; ok {
				sharesAccount = true
				break
			}
		}
		if !sharesAccount {
			continue
		}
		if err := advanceAccountWindowsForKeys(d.DB, keys, now); err != nil {
			return err
		}
		blocked, blockerWake, err := quota.AccountWideBlocker(d.DB, taskAccountIDs, models.DestinationScope(resolved, task.RemoteDir), now)
		if err != nil {
			return err
		}
		var wake *time.Time
		if blocked {
			wake = &blockerWake
		} else if immediate {
			value := now
			wake = &value
		} else {
			wake, err = d.computeWake(keys, now)
			if err != nil {
				return err
			}
		}
		if wake != nil {
			if err := d.setEarliestWake(task.ID, *wake); err != nil {
				return err
			}
		}
	}
	return nil
}

func quotaAccountIDsForKeys(database *gorm.DB, keys map[string]string) ([]uint, error) {
	uniqueKeys := uniqueQuotaKeys(keys)
	if len(uniqueKeys) == 0 {
		return nil, nil
	}
	values := make([]string, 0, len(uniqueKeys))
	for key := range uniqueKeys {
		values = append(values, key)
	}
	sort.Strings(values)
	var accounts []models.QuotaAccount
	if err := database.Where("quota_key IN ?", values).Find(&accounts).Error; err != nil {
		return nil, err
	}
	if len(accounts) != len(values) {
		return nil, errIncompleteQuotaAccountMapping
	}
	ids := make([]uint, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	return ids, nil
}

func (d *Dispatcher) computeWake(keys map[string]string, now time.Time) (*time.Time, error) {
	var accounts []models.QuotaAccount
	if err := d.DB.Where("quota_key IN ?", mapValues(keys)).Find(&accounts).Error; err != nil {
		return nil, err
	}
	if len(accounts) != len(uniqueQuotaKeys(keys)) {
		return nil, errors.New("one or more quota accounts are missing")
	}
	var wake *time.Time
	for _, a := range accounts {
		if err := quota.ValidateAccountWindow(a, now); err != nil {
			return nil, err
		}
		candidate := quota.AccountWindowEnd(a)
		if candidate.After(now) && (wake == nil || candidate.Before(*wake)) {
			v := candidate
			wake = &v
		}
	}
	return wake, nil
}

func advanceAccountWindowsForKeys(database *gorm.DB, keys map[string]string, now time.Time) error {
	var accounts []models.QuotaAccount
	if err := database.Where("quota_key IN ?", mapValues(keys)).Find(&accounts).Error; err != nil {
		return err
	}
	if len(accounts) != len(uniqueQuotaKeys(keys)) {
		return errors.New("one or more quota accounts are missing")
	}
	_, err := quota.AdvanceAccountWindows(database, accountIDs(accounts), now)
	return err
}

func uniqueQuotaKeys(keys map[string]string) map[string]struct{} {
	unique := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		unique[key] = struct{}{}
	}
	return unique
}
func accountIDs(accounts []models.QuotaAccount) []uint {
	r := make([]uint, len(accounts))
	for i, a := range accounts {
		r[i] = a.ID
	}
	return r
}
func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}
func (d *Dispatcher) markBatchUnknown(id uint, reason string) error {
	result := d.DB.Model(&models.RotationQuotaBatch{}).Where("id = ? AND state IN ?", id, []string{models.BatchStateReserved, models.BatchStatePlanned, models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Updates(map[string]interface{}{"state": models.BatchStateUnknown, "last_error": reason})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLeaseConflict
	}
	return nil
}
