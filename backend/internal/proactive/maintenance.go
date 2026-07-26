package proactive

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/quota"
)

var (
	ErrMaintenancePaused   = errors.New("destination scope is paused for quota maintenance")
	ErrCoordinatorConflict = errors.New("destination scope coordinator is owned")
	ErrManualMergeConflict = errors.New("manual merge conflicts with active destination scope work")
	ErrUnknownMaintenance  = errors.New("maintenance epoch is not an eligible unknown manual merge")
)

var maintenanceTokenPattern = regexp.MustCompile(`[0-9a-fA-F]{48}`)

type MaintenanceEpoch struct {
	Scope        string
	TaskID       uint
	ConfigPath   string
	ConfigIdent  string
	Remote       string
	RemoteDir    string
	CapacityWake time.Time
}

type dedupeExecutor interface {
	RunDedupe(context.Context, models.DestinationScopeMaintenance) error
}

func destinationMaintenanceScope(task models.Task, resolvedConfig string) string {
	return models.DestinationScope(resolvedConfig, task.RemoteDir)
}

func firstRotationRemote(task models.Task) string {
	remotes := models.ParseRotationRemotes(task.RotationRemotes)
	if len(remotes) == 0 {
		return ""
	}
	return remotes[0]
}

func (d *Dispatcher) maintenancePaused(task models.Task, resolvedConfig string, _ time.Time) (bool, error) {
	if err := d.recoverMaintenanceDedupe(context.Background(), false); err != nil {
		return true, err
	}
	scope := destinationMaintenanceScope(task, resolvedConfig)
	var epoch models.DestinationScopeMaintenance
	result := d.DB.Where("destination_scope = ? AND (state <> ? OR dedupe_state IN ?)", scope, models.MaintenanceStateClosed, []string{models.DedupeStateClaimed, models.DedupeStateRunning, models.DedupeStateUnknown}).Order("epoch DESC").First(&epoch)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if result.Error != nil {
		if strings.Contains(strings.ToLower(result.Error.Error()), "no such table") {
			return false, nil
		}
		return false, result.Error
	}
	if epoch.Reason == models.MaintenanceReasonManualMerge {
		return true, nil
	}
	if epoch.Reason == models.MaintenanceReasonQuotaExhaustion && (epoch.DedupeState == models.DedupeStateClaimed || epoch.DedupeState == models.DedupeStateRunning || epoch.DedupeState == models.DedupeStateUnknown) {
		return true, nil
	}
	return false, nil
}

func (d *Dispatcher) ensureCoordinator(tx *gorm.DB, scope string) (models.DestinationScopeCoordinator, error) {
	var coordinator models.DestinationScopeCoordinator
	result := tx.Where("destination_scope = ?", scope).First(&coordinator)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		coordinator = models.DestinationScopeCoordinator{DestinationScope: scope, Revision: 1}
		if err := tx.Create(&coordinator).Error; err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "unique") {
				return coordinator, err
			}
			if err := tx.Where("destination_scope = ?", scope).First(&coordinator).Error; err != nil {
				return coordinator, err
			}
		}
	} else if result.Error != nil {
		return coordinator, result.Error
	}
	return coordinator, nil
}

func (d *Dispatcher) acquireScannerLease(scope string, now time.Time) (string, error) {
	if !d.DB.Migrator().HasTable(&models.DestinationScopeCoordinator{}) {
		return "", nil
	}
	token := randomToken()
	var err error
	for attempt := 0; attempt < d.retryMax(); attempt++ {
		err = d.DB.Transaction(func(tx *gorm.DB) error {
			coordinator, err := d.ensureCoordinator(tx, scope)
			if err != nil {
				return err
			}
			result := tx.Model(&models.DestinationScopeCoordinator{}).
				Where("id = ? AND (scanner_lease_token = '' OR scanner_lease_until IS NULL OR scanner_lease_until <= ?)", coordinator.ID, now).
				Updates(map[string]interface{}{"scanner_lease_token": token, "scanner_lease_until": now.Add(d.scannerLeaseDuration()), "revision": gorm.Expr("revision + 1")})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrCoordinatorConflict
			}
			if coordinator.MaintenanceEpochID != 0 {
				var epoch models.DestinationScopeMaintenance
				if err := tx.First(&epoch, coordinator.MaintenanceEpochID).Error; err == nil {
					if epoch.State != models.MaintenanceStateClosed || epoch.DedupeState == models.DedupeStateClaimed || epoch.DedupeState == models.DedupeStateRunning || epoch.DedupeState == models.DedupeStateUnknown {
						return ErrCoordinatorConflict
					}
				}
			}
			return nil
		})
		if err == nil || errors.Is(err, ErrCoordinatorConflict) || !retryableSQLiteError(err) {
			return token, err
		}
		d.retrySleep(attempt)
	}
	return token, err
}

type scannerLeaseHeartbeat struct {
	done  chan struct{}
	err   chan error
	check func() error
}

func (h *scannerLeaseHeartbeat) Stop() {
	if h == nil {
		return
	}
	select {
	case <-h.done:
	default:
		close(h.done)
	}
}

func (h *scannerLeaseHeartbeat) Err() error {
	if h == nil {
		return nil
	}
	select {
	case err := <-h.err:
		return err
	default:
		if h.check != nil {
			return h.check()
		}
		return nil
	}
}

func (d *Dispatcher) scannerLeaseDuration() time.Duration {
	if d.ScannerLeaseDuration > 0 {
		return d.ScannerLeaseDuration
	}
	return 2 * time.Minute
}

func (d *Dispatcher) startScannerLeaseHeartbeat(scope, token string) *scannerLeaseHeartbeat {
	h := &scannerLeaseHeartbeat{done: make(chan struct{}), err: make(chan error, 1)}
	if token == "" {
		return h
	}
	h.check = func() error {
		var coordinator models.DestinationScopeCoordinator
		if err := d.DB.Where("destination_scope = ?", scope).First(&coordinator).Error; err != nil {
			return err
		}
		if coordinator.ScannerLeaseToken != token || coordinator.ScannerLeaseUntil == nil || !coordinator.ScannerLeaseUntil.After(d.now()) {
			return ErrCoordinatorConflict
		}
		return nil
	}
	interval := d.ScannerLeaseHeartbeat
	if interval <= 0 {
		interval = d.scannerLeaseDuration() / 3
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				now := d.now()
				result := d.DB.Model(&models.DestinationScopeCoordinator{}).
					Where("destination_scope = ? AND scanner_lease_token = ? AND scanner_lease_until > ?", scope, token, now).
					Updates(map[string]interface{}{"scanner_lease_until": now.Add(d.scannerLeaseDuration()), "revision": gorm.Expr("revision + 1")})
				if result.Error != nil || result.RowsAffected != 1 {
					if result.Error == nil {
						result.Error = ErrCoordinatorConflict
					}
					h.err <- result.Error
					return
				}
			case <-h.done:
				return
			}
		}
	}()
	return h
}

func (d *Dispatcher) releaseScannerLease(scope, token string) error {
	if token == "" {
		return nil
	}
	result := d.DB.Model(&models.DestinationScopeCoordinator{}).Where("destination_scope = ? AND scanner_lease_token = ?", scope, token).Updates(map[string]interface{}{"scanner_lease_token": "", "scanner_lease_until": nil, "revision": gorm.Expr("revision + 1")})
	return result.Error
}

func coordinatorAllowsReserve(tx *gorm.DB, scope, scannerToken string, now time.Time) error {
	var coordinator models.DestinationScopeCoordinator
	result := tx.Where("destination_scope = ?", scope).First(&coordinator)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return nil
	}
	if result.Error != nil {
		return result.Error
	}
	// Touching the row is the coordinator-first write lock. A scanner may
	// reserve only while holding the exact lease it acquired before scanning.
	if err := tx.Model(&coordinator).UpdateColumn("updated_at", gorm.Expr("updated_at")).Error; err != nil {
		return err
	}
	if coordinator.ScannerLeaseToken != "" && coordinator.ScannerLeaseUntil != nil && coordinator.ScannerLeaseUntil.After(now) && coordinator.ScannerLeaseToken != scannerToken {
		return ErrCoordinatorConflict
	}
	return nil
}

func (d *Dispatcher) claimManualMerge(task models.Task) (models.DestinationScopeMaintenance, error) {
	if task.TaskType != "rotation" || task.RotationStrategy != "proactive_quota" {
		return models.DestinationScopeMaintenance{}, ErrManualMergeConflict
	}
	if task.Status != "idle" {
		return models.DestinationScopeMaintenance{}, ErrManualMergeConflict
	}
	resolved := strings.TrimSpace(task.RcloneConfig)
	if resolved == "" {
		resolved = models.DefaultRcloneConfigPath
	}
	var err error
	resolved, err = d.Quota.ResolveConfigPath(resolved)
	if err != nil {
		return models.DestinationScopeMaintenance{}, err
	}
	remote := firstRotationRemote(task)
	if remote == "" || strings.TrimSpace(task.RemoteDir) == "" {
		return models.DestinationScopeMaintenance{}, ErrManualMergeConflict
	}
	scope := destinationMaintenanceScope(task, resolved)
	var claimed models.DestinationScopeMaintenance
	claimAttempt := func() error {
		return d.DB.Transaction(func(tx *gorm.DB) error {
			coordinator, err := d.ensureCoordinator(tx, scope)
			if err != nil {
				return err
			}
			if err := tx.Model(&coordinator).UpdateColumn("updated_at", gorm.Expr("updated_at")).Error; err != nil {
				return err
			}
			if err := tx.First(&coordinator, coordinator.ID).Error; err != nil {
				return err
			}
			if coordinator.ScannerLeaseToken != "" && coordinator.ScannerLeaseUntil != nil && coordinator.ScannerLeaseUntil.After(d.now()) {
				return ErrCoordinatorConflict
			}
			keys, err := completeQuotaKeys(task, resolved)
			if err != nil {
				return err
			}
			var accounts []models.QuotaAccount
			if err := tx.Where("quota_key IN ?", mapValues(keys)).Find(&accounts).Error; err != nil {
				return err
			}
			blocked, _, err := quota.AccountWideBlocker(tx, accountIDs(accounts), scope, d.now())
			if err != nil {
				return err
			}
			if blocked {
				return ErrManualMergeConflict
			}
			var open models.DestinationScopeMaintenance
			result := tx.Where("destination_scope = ? AND (state <> ? OR dedupe_state IN ?)", scope, models.MaintenanceStateClosed, []string{models.DedupeStateClaimed, models.DedupeStateRunning, models.DedupeStateUnknown}).Order("epoch DESC").First(&open)
			if result.Error == nil {
				return ErrManualMergeConflict
			}
			if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
				return result.Error
			}
			var active int64
			if err := tx.Model(&models.RotationQuotaBatch{}).Where("destination_scope = ? AND state IN ?", scope, []string{models.BatchStatePlanned, models.BatchStateReserved, models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Count(&active).Error; err != nil {
				return err
			}
			if active > 0 {
				return ErrManualMergeConflict
			}
			var last models.DestinationScopeMaintenance
			if err := tx.Where("destination_scope = ?", scope).Order("epoch DESC").First(&last).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			epoch := last.Epoch + 1
			claimed = models.DestinationScopeMaintenance{DestinationScope: scope, Epoch: epoch, OwnerTaskID: task.ID, FirstRemote: remote, RemoteDir: models.CanonicalDestinationPath(task.RemoteDir), ResolvedConfigPath: resolved, ResolvedConfigIdentity: resolved, State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStatePending, Reason: models.MaintenanceReasonManualMerge, Revision: 1}
			if err := tx.Create(&claimed).Error; err != nil {
				return err
			}
			return tx.Model(&models.DestinationScopeCoordinator{}).Where("id = ?", coordinator.ID).Updates(map[string]interface{}{"maintenance_epoch_id": claimed.ID, "revision": gorm.Expr("revision + 1")}).Error
		})
	}
	for attempt := 0; attempt < d.retryMax(); attempt++ {
		err = claimAttempt()
		if err == nil || !retryableSQLiteError(err) || attempt == d.retryMax()-1 {
			break
		}
		d.retrySleep(attempt)
	}
	return claimed, err
}

func completeManualMaintenance(db *gorm.DB, epoch models.DestinationScopeMaintenance, dedupeState, result string, exitCode int, message string, now time.Time, resolver quota.ConfigResolver) error {
	return db.Transaction(func(tx *gorm.DB) error {
		return closeManualMaintenanceTx(tx, epoch, []string{models.DedupeStateRunning}, false, dedupeState, result, exitCode, message, now, resolver)
	})
}

func closeManualMaintenanceOwnerTx(tx *gorm.DB, epoch models.DestinationScopeMaintenance, dedupeState, result string, exitCode int, message string, now time.Time, resolver quota.ConfigResolver) error {
	return closeManualMaintenanceTx(tx, epoch, []string{models.DedupeStateRunning}, false, dedupeState, result, exitCode, message, now, resolver)
}

func closeManualMaintenanceTx(tx *gorm.DB, epoch models.DestinationScopeMaintenance, expectedStates []string, exactExpiry bool, dedupeState, result string, exitCode int, message string, now time.Time, resolver quota.ConfigResolver) error {
	query := tx.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND reason = ? AND state = ? AND dedupe_state IN ? AND lease_token = ?", epoch.ID, models.MaintenanceReasonManualMerge, models.MaintenanceStateExhausted, expectedStates, epoch.LeaseToken)
	if exactExpiry {
		if epoch.LeaseUntil == nil {
			query = query.Where("lease_until IS NULL")
		} else {
			query = query.Where("lease_until = ?", *epoch.LeaseUntil)
		}
	}
	updated := query.Updates(map[string]interface{}{"state": models.MaintenanceStateClosed, "dedupe_state": dedupeState, "result": result, "exit_code": exitCode, "finished_at": now, "last_error": redactMaintenanceError(message, epoch), "revision": gorm.Expr("revision + 1")})
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return ErrLeaseConflict
	}
	var coordinator models.DestinationScopeCoordinator
	if err := tx.Where("destination_scope = ?", epoch.DestinationScope).First(&coordinator).Error; err == nil {
		if err := tx.Model(&coordinator).Where("maintenance_epoch_id = ?", epoch.ID).Updates(map[string]interface{}{"maintenance_epoch_id": 0, "revision": gorm.Expr("revision + 1")}).Error; err != nil {
			return err
		}
	}
	var tasks []models.Task
	if err := tx.Where("enabled = ? AND task_type = ? AND rotation_strategy = ?", true, "rotation", "proactive_quota").Find(&tasks).Error; err != nil {
		return err
	}
	for _, task := range tasks {
		taskScope, err := taskDestinationScope(task, resolver)
		if err != nil || taskScope != epoch.DestinationScope {
			continue
		}
		values := map[string]interface{}{"rotation_rescan_pending": true, "rotation_quota_wake_at": now}
		if task.ID == epoch.OwnerTaskID {
			values["last_error"] = redactMaintenanceError(message, epoch)
		}
		if err := tx.Model(&models.Task{}).Where("id = ?", task.ID).Updates(values).Error; err != nil {
			return err
		}
	}
	return nil
}

func (d *Dispatcher) closeRecoveredManual(epoch models.DestinationScopeMaintenance, token string, until time.Time, expectedStates []string, message string) error {
	epoch.LeaseToken = token
	epoch.LeaseUntil = &until
	return d.DB.Transaction(func(tx *gorm.DB) error {
		return closeManualMaintenanceTx(tx, epoch, expectedStates, true, models.DedupeStateFailed, models.DedupeStateFailed, 1, message, d.now(), d.ConfigResolver)
	})
}

func taskDestinationScope(task models.Task, resolver quota.ConfigResolver) (string, error) {
	raw := strings.TrimSpace(task.RcloneConfig)
	if raw == "" {
		raw = models.DefaultRcloneConfigPath
	}
	if resolver != nil {
		resolved, err := resolver(raw)
		if err != nil {
			return "", err
		}
		raw = resolved
	}
	return models.DestinationScope(raw, task.RemoteDir), nil
}

// MutationBlocked reports whether a manual merge owns the task's destination
// scope. The fence covers the full manual epoch claim-to-recovery lifecycle.
func (d *Dispatcher) MutationBlocked(task models.Task) (bool, error) {
	if !d.DB.Migrator().HasTable(&models.DestinationScopeMaintenance{}) {
		return false, nil
	}
	scope, err := taskDestinationScope(task, d.ConfigResolver)
	if err != nil {
		return false, err
	}
	var count int64
	err = d.DB.Model(&models.DestinationScopeMaintenance{}).
		Where("destination_scope = ? AND ((reason = ? AND state = ? AND dedupe_state IN ?) OR (reason = ? AND dedupe_state IN ? AND (state <> ? OR dedupe_state IN ?)))", scope, models.MaintenanceReasonManualMerge, models.MaintenanceStateExhausted, []string{"", models.DedupeStatePending, models.DedupeStateClaimed, models.DedupeStateRunning, models.DedupeStateUnknown}, models.MaintenanceReasonQuotaExhaustion, []string{"", models.DedupeStatePending, models.DedupeStateClaimed, models.DedupeStateRunning, models.DedupeStateUnknown}, models.MaintenanceStateClosed, []string{models.DedupeStateClaimed, models.DedupeStateRunning, models.DedupeStateUnknown}).
		Count(&count).Error
	return count > 0, err
}

func (d *Dispatcher) RunManualMerge(ctx context.Context, taskID uint) error {
	task, epoch, err := d.ClaimManualMerge(taskID)
	if err != nil {
		return err
	}
	return d.RunClaimedManualMerge(ctx, task, epoch)
}

func (d *Dispatcher) ClaimManualMerge(taskID uint) (models.Task, models.DestinationScopeMaintenance, error) {
	var task models.Task
	if err := d.DB.First(&task, taskID).Error; err != nil {
		return task, models.DestinationScopeMaintenance{}, err
	}
	epoch, err := d.claimManualMerge(task)
	return task, epoch, err
}

func (d *Dispatcher) RunClaimedManualMerge(ctx context.Context, task models.Task, epoch models.DestinationScopeMaintenance) error {
	if epoch.Reason != models.MaintenanceReasonManualMerge || epoch.OwnerTaskID != task.ID || epoch.State != models.MaintenanceStateExhausted {
		return ErrManualMergeConflict
	}
	resolved := strings.TrimSpace(task.RcloneConfig)
	if resolved == "" {
		resolved = models.DefaultRcloneConfigPath
	}
	var err error
	if d.ConfigResolver != nil {
		resolved, err = d.ConfigResolver(resolved)
		if err != nil {
			return err
		}
	}
	if epoch.ResolvedConfigPath != resolved || epoch.ResolvedConfigIdentity != resolved || epoch.DestinationScope != destinationMaintenanceScope(task, resolved) || epoch.FirstRemote != firstRotationRemote(task) || epoch.RemoteDir != models.CanonicalDestinationPath(task.RemoteDir) {
		return ErrManualMergeConflict
	}
	claimed, err := d.claimDedupeEpoch(task, resolved, d.now(), epoch.ID)
	if errors.Is(err, quota.ErrActiveBatch) {
		closeErr := d.DB.Transaction(func(tx *gorm.DB) error {
			return closeManualMaintenanceTx(tx, epoch, []string{models.DedupeStatePending, ""}, false, models.DedupeStateFailed, models.DedupeStateFailed, 1, "manual merge blocked by account-wide active work", d.now(), d.ConfigResolver)
		})
		return errors.Join(ErrManualMergeConflict, closeErr)
	}
	if err != nil {
		closeErr := d.DB.Transaction(func(tx *gorm.DB) error {
			return closeManualMaintenanceTx(tx, epoch, []string{models.DedupeStatePending, ""}, false, models.DedupeStateFailed, models.DedupeStateFailed, 1, err.Error(), d.now(), d.ConfigResolver)
		})
		return errors.Join(err, closeErr, d.recordManualMaintenanceError(epoch, err))
	}
	if err == nil {
		runner, ok := d.Executor.(dedupeExecutor)
		if !ok {
			err = errors.New("dedupe runner is unavailable")
			closeErr := d.DB.Transaction(func(tx *gorm.DB) error {
				return closeManualMaintenanceTx(tx, claimed, []string{models.DedupeStateClaimed}, false, models.DedupeStateFailed, models.DedupeStateFailed, 1, err.Error(), d.now(), d.ConfigResolver)
			})
			err = errors.Join(err, closeErr)
		} else {
			err = runner.RunDedupe(ctx, claimed)
		}
	}
	if err != nil {
		return errors.Join(err, d.recordManualMaintenanceError(epoch, err))
	}
	return nil
}

func (d *Dispatcher) recordManualMaintenanceError(epoch models.DestinationScopeMaintenance, original error) error {
	if original == nil {
		return nil
	}
	var current models.DestinationScopeMaintenance
	if err := d.DB.First(&current, epoch.ID).Error; err == nil {
		epoch = current
	}
	message := redactMaintenanceError(original.Error(), epoch)
	var errs []error
	if err := d.DB.Model(&models.DestinationScopeMaintenance{}).Where("id = ?", epoch.ID).Update("last_error", message).Error; err != nil {
		errs = append(errs, err)
	}
	if err := d.DB.Model(&models.Task{}).Where("id = ?", epoch.OwnerTaskID).Update("last_error", message).Error; err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (d *Dispatcher) CloseUnknownMaintenance(epochID uint, reason string, expectedRevision int64) error {
	var epoch models.DestinationScopeMaintenance
	if err := d.DB.First(&epoch, epochID).Error; err != nil {
		return err
	}
	if epoch.Reason != reason || (reason != models.MaintenanceReasonManualMerge && reason != models.MaintenanceReasonQuotaExhaustion) || epoch.DedupeState != models.DedupeStateUnknown || epoch.State != models.MaintenanceStateExhausted || epoch.Revision != expectedRevision {
		return ErrUnknownMaintenance
	}
	if d.Inspector == nil || epoch.ProcessID <= 0 || epoch.ProcessStartToken == "" {
		return ErrUnknownMaintenance
	}
	status, err := d.Inspector.Inspect(epoch.ProcessID, epoch.ProcessStartToken)
	if err != nil || !status.Confirmed || status.Alive {
		return ErrUnknownMaintenance
	}
	if reason == models.MaintenanceReasonQuotaExhaustion {
		return d.closeUnknownLegacyQuota(epoch, expectedRevision)
	}
	now := d.now()
	return d.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND reason = ? AND state = ? AND dedupe_state = ? AND revision = ?", epochID, reason, models.MaintenanceStateExhausted, models.DedupeStateUnknown, expectedRevision).Updates(map[string]interface{}{"state": models.MaintenanceStateClosed, "dedupe_state": models.DedupeStateFailed, "result": models.DedupeStateFailed, "finished_at": now, "revision": gorm.Expr("revision + 1"), "last_error": "manual merge closed after verified dead process"})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrUnknownMaintenance
		}
		var tasks []models.Task
		if err := tx.Where("enabled = ? AND task_type = ? AND rotation_strategy = ?", true, "rotation", "proactive_quota").Find(&tasks).Error; err != nil {
			return err
		}
		for _, task := range tasks {
			taskScope, scopeErr := taskDestinationScope(task, d.ConfigResolver)
			if scopeErr == nil && taskScope == epoch.DestinationScope {
				if err := tx.Model(&models.Task{}).Where("id = ?", task.ID).Updates(map[string]interface{}{"rotation_rescan_pending": true, "rotation_quota_wake_at": now}).Error; err != nil {
					return err
				}
			}
		}
		var coordinator models.DestinationScopeCoordinator
		if err := tx.Where("destination_scope = ?", epoch.DestinationScope).First(&coordinator).Error; err == nil {
			if err := tx.Model(&coordinator).Where("maintenance_epoch_id = ?", epoch.ID).Updates(map[string]interface{}{"maintenance_epoch_id": 0, "revision": gorm.Expr("revision + 1")}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (d *Dispatcher) closeUnknownLegacyQuota(epoch models.DestinationScopeMaintenance, expectedRevision int64) error {
	now := d.now()
	return d.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&models.DestinationScopeMaintenance{}).
			Where("id = ? AND reason = ? AND state = ? AND dedupe_state = ? AND revision = ?", epoch.ID, models.MaintenanceReasonQuotaExhaustion, models.MaintenanceStateExhausted, models.DedupeStateUnknown, expectedRevision).
			Updates(map[string]interface{}{"state": models.MaintenanceStateClosed, "dedupe_state": models.DedupeStateFailed, "result": models.DedupeStateFailed, "finished_at": now, "lease_token": "", "lease_until": nil, "revision": gorm.Expr("revision + 1"), "last_error": redactMaintenanceError("legacy quota exhaustion closed after verified process stop", epoch)})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrUnknownMaintenance
		}
		if err := d.wakeResolvedScopeTasks(tx, epoch.DestinationScope, now); err != nil {
			return err
		}
		var coordinator models.DestinationScopeCoordinator
		if err := tx.Where("destination_scope = ?", epoch.DestinationScope).First(&coordinator).Error; err == nil {
			if err := tx.Model(&coordinator).Where("maintenance_epoch_id = ?", epoch.ID).Updates(map[string]interface{}{"maintenance_epoch_id": 0, "revision": gorm.Expr("revision + 1")}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (d *Dispatcher) wakeResolvedScopeTasks(tx *gorm.DB, scope string, now time.Time) error {
	var tasks []models.Task
	if err := tx.Where("enabled = ? AND task_type = ? AND rotation_strategy = ?", true, "rotation", "proactive_quota").Find(&tasks).Error; err != nil {
		return err
	}
	for _, task := range tasks {
		taskScope, err := taskDestinationScope(task, d.ConfigResolver)
		if err != nil || taskScope != scope {
			continue
		}
		if err := tx.Model(&models.Task{}).Where("id = ?", task.ID).Updates(map[string]interface{}{"rotation_rescan_pending": true, "rotation_quota_wake_at": now}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (d *Dispatcher) claimDedupe(task models.Task, resolvedConfig string, now time.Time) (models.DestinationScopeMaintenance, error) {
	return d.claimDedupeEpoch(task, resolvedConfig, now, 0)
}

func (d *Dispatcher) claimDedupeEpoch(task models.Task, resolvedConfig string, now time.Time, epochID uint) (models.DestinationScopeMaintenance, error) {
	scope := destinationMaintenanceScope(task, resolvedConfig)
	var claimed models.DestinationScopeMaintenance
	err := d.DB.Transaction(func(tx *gorm.DB) error {
		var epoch models.DestinationScopeMaintenance
		query := tx.Where("destination_scope = ? AND reason = ? AND state = ? AND dedupe_state IN ?", scope, models.MaintenanceReasonManualMerge, models.MaintenanceStateExhausted, []string{models.DedupeStatePending, ""})
		if epochID != 0 {
			query = query.Where("id = ? AND owner_task_id = ?", epochID, task.ID)
		} else {
			query = query.Order("epoch DESC")
		}
		if err := query.First(&epoch).Error; err != nil {
			return err
		}
		var ownerTask models.Task
		if err := tx.First(&ownerTask, epoch.OwnerTaskID).Error; err != nil {
			return err
		}
		keys, err := completeQuotaKeys(ownerTask, epoch.ResolvedConfigPath)
		if err != nil {
			return err
		}
		var accounts []models.QuotaAccount
		if err := tx.Where("quota_key IN ?", mapValues(keys)).Find(&accounts).Error; err != nil {
			return err
		}
		accountIDs := make([]uint, 0, len(accounts))
		for _, account := range accounts {
			accountIDs = append(accountIDs, account.ID)
		}
		var active int64
		if err := tx.Model(&models.RotationQuotaBatch{}).Where("destination_scope = ? AND state IN ?", scope, []string{models.BatchStateReserved, models.BatchStatePlanned, models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Count(&active).Error; err != nil {
			return err
		}
		if len(accountIDs) > 0 {
			if err := tx.Model(&models.RotationQuotaBatch{}).Where("quota_account_id IN ? AND state IN ?", accountIDs, []string{models.BatchStateReserved, models.BatchStatePlanned, models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Count(&active).Error; err != nil {
				return err
			}
			var reservations int64
			if err := tx.Model(&models.QuotaReservation{}).Where("quota_account_id IN ? AND state IN ?", accountIDs, []string{models.ReservationStateHeld, models.ReservationStateActive, models.ReservationStateUnknown}).Count(&reservations).Error; err != nil {
				return err
			}
			if reservations > 0 {
				return quota.ErrActiveBatch
			}
		}
		if active != 0 {
			return quota.ErrActiveBatch
		}
		token := randomToken()
		leaseUntil := now.Add(2 * time.Minute)
		result := tx.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND state = ? AND dedupe_state IN ?", epoch.ID, models.MaintenanceStateExhausted, []string{models.DedupeStatePending, ""}).Updates(map[string]interface{}{"dedupe_state": models.DedupeStateClaimed, "lease_token": token, "lease_until": leaseUntil, "revision": gorm.Expr("revision + 1")})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLeaseConflict
		}
		epoch.LeaseToken = token
		epoch.LeaseUntil = &leaseUntil
		claimed = epoch
		return nil
	})
	return claimed, err
}

func (d *Dispatcher) finishDedupe(epoch models.DestinationScopeMaintenance, result ProcessResult, waitErr error) error {
	state := models.DedupeStateSucceeded
	if waitErr != nil || result.ExitCode != 0 {
		state = models.DedupeStateFailed
	}
	errText := result.Stderr
	if errText == "" {
		errText = result.Stdout
	}
	if waitErr != nil {
		errText = fmt.Sprintf("%s: %v", errText, waitErr)
	}
	return d.DB.Model(&models.DestinationScopeMaintenance{}).Where("id = ? AND state = ? AND dedupe_state IN ? AND lease_token = ?", epoch.ID, models.MaintenanceStateExhausted, []string{models.DedupeStateClaimed, models.DedupeStateRunning}, epoch.LeaseToken).Updates(map[string]interface{}{"dedupe_state": state, "result": state, "exit_code": result.ExitCode, "process_id": result.PID, "process_start_token": result.ProcessStartToken, "finished_at": d.now(), "last_error": redactMaintenanceError(errText, epoch)}).Error
}

func redactMaintenanceError(value string, epoch models.DestinationScopeMaintenance) string {
	for _, secret := range []string{epoch.ResolvedConfigPath, epoch.ResolvedConfigIdentity, epoch.LeaseToken, epoch.ProcessStartToken} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	value = maintenanceTokenPattern.ReplaceAllString(value, "[redacted-token]")
	if len(value) > 2048 {
		return value[:2048]
	}
	return value
}

func maintenanceEpochStatus(db *gorm.DB, scopes []string) ([]models.DestinationScopeMaintenance, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	var rows []models.DestinationScopeMaintenance
	err := db.Where("destination_scope IN ?", scopes).Order("destination_scope, epoch DESC").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].DestinationScope < rows[j].DestinationScope })
	return rows, nil
}
