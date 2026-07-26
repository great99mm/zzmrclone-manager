package quota

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
)

var (
	ErrActiveBatch            = errors.New("an active quota batch already exists for the task or destination scope")
	ErrIdempotencyConflict    = errors.New("request idempotency key was reused with a different fingerprint")
	ErrDestinationScopePaused = errors.New("destination scope is paused for maintenance")
	ErrCoordinatorConflict    = errors.New("destination scope coordinator is owned")
	ErrHeldBatchNotSafe       = errors.New("quota batch is not fully held and unstarted")
)

type ConfigResolver func(raw string) (string, error)

type Service struct {
	DB                          *gorm.DB
	Now                         func() time.Time
	SettleInterval              time.Duration
	RetrySleep                  func(time.Duration)
	TokenGenerator              func() string
	ConfigResolver              ConfigResolver
	MaxRetries                  int
	RetryDelay                  time.Duration
	MoveEnabled                 func() bool
	BeforeFinalReservationCheck func(*gorm.DB)
}

func (s *Service) ResolveConfigPath(raw string) (string, error) { return s.resolveConfigPath(raw) }

type PackReserveRequest struct {
	Task                  models.Task
	Snapshots             []LocalSnapshot
	RequestIdempotencyKey string
	SourceRoot            string
	DestinationPath       string
	CoordinatorLeaseToken string
}

type PackReserveResult struct {
	Batches        []models.RotationQuotaBatch
	Pending        []LocalSnapshot
	Existing       bool
	Classification string
	RetryAt        *time.Time
}

func (s *Service) Reserve(req PackReserveRequest) (PackReserveResult, error) {
	if s == nil || s.DB == nil {
		return PackReserveResult{}, errors.New("quota service database is required")
	}
	if len(req.Snapshots) == 0 {
		return PackReserveResult{Classification: models.ReserveClassNoFiles}, nil
	}
	normalizedTask, err := s.normalizeQuotaTask(req.Task)
	if err != nil {
		return PackReserveResult{}, err
	}
	if !filepath.IsAbs(req.SourceRoot) {
		return PackReserveResult{}, fmt.Errorf("source root must be absolute: %q", req.SourceRoot)
	}
	sourceRoot := filepath.Clean(req.SourceRoot)
	resolvedConfig, err := s.resolveConfigPath(normalizedTask.RcloneConfig)
	if err != nil {
		return PackReserveResult{}, err
	}
	if err := validateSnapshots(req.Snapshots); err != nil {
		return PackReserveResult{}, err
	}
	req.Task = normalizedTask
	req.SourceRoot = sourceRoot
	requestKey := strings.TrimSpace(req.RequestIdempotencyKey)
	if requestKey == "" {
		requestKey = s.generateToken()
	}
	remotes := models.ParseRotationRemotes(req.Task.RotationRemotes)
	if len(remotes) == 0 {
		return PackReserveResult{}, errors.New("rotation remotes are required")
	}
	quotaKeys, err := models.ParseRotationQuotaKeys(req.Task.RotationQuotaKeys)
	if err != nil {
		return PackReserveResult{}, err
	}
	for _, remote := range remotes {
		if strings.TrimSpace(quotaKeys[remote]) == "" {
			return PackReserveResult{}, fmt.Errorf("quota key is missing for remote %q", remote)
		}
	}
	destinationPath := models.CanonicalDestinationPath(req.DestinationPath)
	fingerprint := requestFingerprint(req.Task, req.SourceRoot, resolvedConfig, destinationPath, remotes, quotaKeys, req.Snapshots)

	maxRetries := s.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 8
	}
	for attempt := 0; attempt < maxRetries; attempt++ {
		result, err := s.reserveAttempt(req, requestKey, fingerprint, resolvedConfig, destinationPath, remotes, quotaKeys)
		if err == nil {
			return result, nil
		}
		if !isBusyError(err) || attempt == maxRetries-1 {
			return PackReserveResult{}, err
		}
		sleep := s.RetrySleep
		if sleep == nil {
			sleep = time.Sleep
		}
		sleep(s.retryDelay(attempt))
	}
	return PackReserveResult{}, errors.New("quota reservation retries exhausted")
}

func (s *Service) retryDelay(attempt int) time.Duration {
	base := s.RetryDelay
	if base <= 0 {
		base = 10 * time.Millisecond
	}
	for i := 0; i < attempt; i++ {
		base *= 2
	}
	return base
}

func (s *Service) normalizeQuotaTask(task models.Task) (models.Task, error) {
	if strings.TrimSpace(task.SourceType) == "" {
		task.SourceType = "local"
	}
	if task.SourceType != "local" {
		return models.Task{}, fmt.Errorf("quota reservation requires a local source")
	}
	if strings.TrimSpace(task.DestType) == "" {
		task.DestType = "remote"
	}
	if task.DestType != "remote" {
		return models.Task{}, fmt.Errorf("quota reservation requires a remote destination")
	}
	if strings.TrimSpace(task.TransferMode) == "" {
		task.TransferMode = "move"
	}
	if task.TransferMode != models.TransferModeCopy && (task.TransferMode != models.TransferModeMove || s.MoveEnabled == nil || !s.MoveEnabled()) {
		return models.Task{}, fmt.Errorf("proactive quota reservation transfer mode %q is disabled or unsupported", task.TransferMode)
	}
	if task.RotationQuotaLimitBytes < 0 {
		return models.Task{}, fmt.Errorf("rotation quota limit cannot be negative")
	}
	if task.RotationQuotaLimitBytes > models.DefaultRotationQuotaLimitBytes {
		return models.Task{}, fmt.Errorf("rotation quota limit exceeds %d bytes", models.DefaultRotationQuotaLimitBytes)
	}
	return task, nil
}

func (s *Service) resolveConfigPath(raw string) (string, error) {
	if s.ConfigResolver != nil {
		resolved, err := s.ConfigResolver(raw)
		if err != nil {
			return "", err
		}
		return validateResolvedConfigPath(resolved)
	}
	return resolveRcloneConfigPath(raw)
}

func resolveRcloneConfigPath(raw string) (string, error) {
	configPath := strings.TrimSpace(raw)
	if configPath == "" {
		configPath = models.DefaultRcloneConfigPath
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return "", fmt.Errorf("resolve rclone config path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(abs))
	if err != nil {
		return "", fmt.Errorf("resolve rclone config path %q: %w", abs, err)
	}
	return validateResolvedConfigPath(resolved)
}

func validateResolvedConfigPath(raw string) (string, error) {
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("normalize resolved rclone config path: %w", err)
	}
	resolved := filepath.Clean(abs)
	if resolved != abs {
		return "", fmt.Errorf("resolved rclone config path is not clean: %q", raw)
	}
	actual, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", fmt.Errorf("validate resolved rclone config path: %w", err)
	}
	actual, err = filepath.Abs(filepath.Clean(actual))
	if err != nil || actual != resolved {
		return "", fmt.Errorf("resolved rclone config path is an unresolved alias: %q", raw)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat resolved rclone config path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("resolved rclone config is not a regular non-symlink file: %q", resolved)
	}
	return resolved, nil
}

func validateSnapshots(snapshots []LocalSnapshot) error {
	seenPaths := make(map[string]struct{}, len(snapshots))
	seenKeys := make(map[string]struct{}, len(snapshots))
	var total int64
	var rootDevice, rootInode int64
	for _, snapshot := range snapshots {
		if snapshot.RootDevice <= 0 || snapshot.RootInode <= 0 {
			return errors.New("snapshots are missing a valid source root identity")
		}
		if rootDevice == 0 {
			rootDevice, rootInode = snapshot.RootDevice, snapshot.RootInode
		} else if snapshot.RootDevice != rootDevice || snapshot.RootInode != rootInode {
			return errors.New("snapshots have inconsistent source root identity")
		}
		if snapshot.SizeBytes < 0 {
			return fmt.Errorf("snapshot %q has negative size", snapshot.RelativePath)
		}
		if strings.TrimSpace(snapshot.SnapshotKey) == "" {
			return fmt.Errorf("snapshot %q has an empty snapshot key", snapshot.RelativePath)
		}
		if err := validateRelativeSnapshotPath(snapshot.RelativePath); err != nil {
			return err
		}
		if _, ok := seenPaths[snapshot.RelativePath]; ok {
			return fmt.Errorf("duplicate snapshot relative path %q", snapshot.RelativePath)
		}
		if _, ok := seenKeys[snapshot.SnapshotKey]; ok {
			return fmt.Errorf("duplicate snapshot key %q", snapshot.SnapshotKey)
		}
		seenPaths[snapshot.RelativePath] = struct{}{}
		seenKeys[snapshot.SnapshotKey] = struct{}{}
		var err error
		total, err = safeAdd(total, snapshot.SizeBytes)
		if err != nil {
			return fmt.Errorf("snapshot byte total overflows int64")
		}
	}
	return nil
}

func validateRelativeSnapshotPath(relative string) error {
	if relative == "" || strings.ContainsAny(relative, "\r\n\x00") || filepath.IsAbs(relative) {
		return fmt.Errorf("invalid snapshot relative path %q", relative)
	}
	normalized := strings.ReplaceAll(relative, "\\", "/")
	clean := path.Clean(normalized)
	if clean != normalized {
		return fmt.Errorf("snapshot relative path is not canonical: %q", relative)
	}
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("snapshot relative path escapes source root: %q", relative)
	}
	for _, component := range strings.Split(clean, "/") {
		if component == "." || component == ".." {
			return fmt.Errorf("snapshot relative path contains traversal component: %q", relative)
		}
	}
	return nil
}

func safeAdd(left, right int64) (int64, error) {
	if right > 0 && left > int64(^uint64(0)>>1)-right {
		return 0, errors.New("int64 overflow")
	}
	return left + right, nil
}

func (s *Service) reserveAttempt(req PackReserveRequest, requestKey, fingerprint, resolvedConfig, destinationPath string, remotes []string, quotaKeys map[string]string) (PackReserveResult, error) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	returnResult := PackReserveResult{}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := proactiveCoordinatorReserve(tx, models.DestinationScope(resolvedConfig, destinationPath), req.CoordinatorLeaseToken, now()); err != nil {
			return err
		}
		if err := checkMaintenanceFence(tx, models.DestinationScope(resolvedConfig, destinationPath), now()); err != nil {
			return err
		}
		accounts, err := loadAccounts(tx, quotaKeys)
		if err != nil {
			return err
		}
		if err := validateAccounts(accounts); err != nil {
			return err
		}
		for _, account := range accounts {
			result := tx.Model(&models.QuotaAccount{}).Where("id = ?", account.ID).UpdateColumn("updated_at", gorm.Expr("updated_at"))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("quota account %d disappeared while acquiring writer lock", account.ID)
			}
		}
		if err := tx.Where("id IN ?", accountIDs(accounts)).Order("id, quota_key").Find(&accounts).Error; err != nil {
			return err
		}
		if err := validateAccounts(accounts); err != nil {
			return err
		}
		transactionNow := now()
		for i := range accounts {
			advanced, err := AdvanceAccountWindowTx(tx, accounts[i].ID, transactionNow)
			if err != nil {
				return err
			}
			accounts[i] = advanced
		}
		var existing []models.RotationQuotaBatch
		if err := tx.Where("task_id = ? AND request_key = ?", req.Task.ID, requestKey).Order("id").Find(&existing).Error; err != nil {
			return err
		}
		if len(existing) > 0 {
			for _, batch := range existing {
				if batch.RequestFingerprint != fingerprint {
					return ErrIdempotencyConflict
				}
			}
			pending, err := pendingForExistingBatches(tx, req.Snapshots, existing)
			if err != nil {
				return err
			}
			returnResult.Batches = existing
			returnResult.Pending = pending
			returnResult.Existing = true
			returnResult.Classification = models.ReserveClassReserved
			return nil
		}
		if err := rejectActiveBatches(tx, req.Task, resolvedConfig, destinationPath); err != nil {
			return err
		}
		blocked, blockerWake, err := accountWideBlocker(tx, accountIDs(accounts), models.DestinationScope(resolvedConfig, destinationPath), transactionNow)
		if err != nil {
			return err
		}
		if blocked {
			returnResult.Pending = append([]LocalSnapshot(nil), req.Snapshots...)
			returnResult.Classification = models.ReserveClassAccountBlocked
			returnResult.RetryAt = &blockerWake
			return nil
		}

		usage, err := accountUsage(tx, accountIDs(accounts), transactionNow)
		if err != nil {
			return err
		}
		forecast3h, err := accountForecasts(tx, accountIDs(accounts), transactionNow)
		if err != nil {
			return err
		}
		var persistedAssignments []models.RotationQuotaDirectoryAssignment
		if err := tx.Where("task_id = ?", req.Task.ID).Find(&persistedAssignments).Error; err != nil {
			return err
		}
		directoryAccounts := make(map[string]uint, len(persistedAssignments))
		for _, assignment := range persistedAssignments {
			directoryAccounts[assignment.Directory] = assignment.QuotaAccountID
		}
		selected, pending, newAssignments, err := packSnapshotsWithForecast(req.Snapshots, remotes, quotaKeys, accounts, usage, forecast3h, req.Task.RotationQuotaLimitBytes, req.Task.RotationBatchFiles, directoryAccounts, transactionNow)
		if err != nil {
			return err
		}
		for directory, accountID := range newAssignments {
			if err := tx.Create(&models.RotationQuotaDirectoryAssignment{TaskID: req.Task.ID, Directory: directory, QuotaAccountID: accountID}).Error; err != nil {
				return err
			}
		}
		returnResult.Pending = pending
		returnResult.Classification = classifyReserveOutcome(pending, selected, accounts, usage, req.Task.RotationQuotaLimitBytes, transactionNow)
		if s.BeforeFinalReservationCheck != nil {
			s.BeforeFinalReservationCheck(tx)
		}
		if err := proactiveCoordinatorReserve(tx, models.DestinationScope(resolvedConfig, destinationPath), req.CoordinatorLeaseToken, transactionNow); err != nil {
			return err
		}
		touchedAccounts := make(map[uint]struct{})
		for _, remote := range remotes {
			files := selected[remote]
			if len(files) == 0 {
				continue
			}
			account := accountsByKey(accounts)[quotaKeys[remote]]
			ownerToken := s.generateToken()
			reservedBytes, err := totalBytes(files)
			if err != nil {
				return err
			}
			batch := models.RotationQuotaBatch{
				TaskID:                    req.Task.ID,
				QuotaAccountID:            account.ID,
				DestinationScope:          models.DestinationScope(resolvedConfig, destinationPath),
				SourceRoot:                req.SourceRoot,
				SourceRootDevice:          req.Snapshots[0].RootDevice,
				SourceRootInode:           req.Snapshots[0].RootInode,
				DestinationRemote:         remote,
				TransferMode:              req.Task.TransferMode,
				RcloneTransfers:           req.Task.Transfers,
				RotationConcurrentBatches: req.Task.RotationConcurrentBatches,
				DestinationScopeVersion:   1,
				RcloneConfigPath:          resolvedConfig,
				RequestKey:                requestKey,
				RequestFingerprint:        fingerprint,
				DestinationPath:           destinationPath,
				State:                     models.BatchStateReserved,
				OwnerToken:                ownerToken,
				LeaseToken:                s.generateToken(),
				ProcessStartToken:         s.generateToken(),
				ReservedBytes:             reservedBytes,
			}
			if err := tx.Create(&batch).Error; err != nil {
				return err
			}
			for _, snapshot := range files {
				batchFile := models.RotationQuotaBatchFile{
					BatchID: batch.ID, RelativePath: snapshot.RelativePath, SnapshotKey: snapshot.SnapshotKey,
					SizeBytes: snapshot.SizeBytes, MtimeNS: snapshot.MtimeNS, Device: snapshot.Device, Inode: snapshot.Inode,
					State: models.BatchFileStateHeld,
				}
				if err := tx.Create(&batchFile).Error; err != nil {
					return err
				}
				reservation := models.QuotaReservation{
					QuotaAccountID: account.ID, BatchID: batch.ID, BatchFileID: batchFile.ID,
					Bytes: snapshot.SizeBytes, State: models.ReservationStateHeld, IdempotencyKey: ownerToken + ":" + snapshot.SnapshotKey,
					ReservedAt: &transactionNow,
				}
				if err := tx.Create(&reservation).Error; err != nil {
					return err
				}
			}
			returnResult.Batches = append(returnResult.Batches, batch)
			touchedAccounts[account.ID] = struct{}{}
		}
		for accountID := range touchedAccounts {
			if err := ReconcileAccountWindowAnchor(tx, accountID, transactionNow); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && errors.Is(err, ErrActiveBatch) {
		return PackReserveResult{Classification: models.ReserveClassActive}, err
	}
	return returnResult, err
}

func checkMaintenanceFence(tx *gorm.DB, scope string, _ time.Time) error {
	var epoch models.DestinationScopeMaintenance
	result := tx.Where("destination_scope = ? AND ((reason = ? AND state = ?) OR (reason = ? AND dedupe_state IN ?))", scope, models.MaintenanceReasonManualMerge, models.MaintenanceStateExhausted, models.MaintenanceReasonQuotaExhaustion, []string{models.DedupeStateClaimed, models.DedupeStateRunning, models.DedupeStateUnknown}).Order("epoch DESC").First(&epoch)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return nil
	}
	if result.Error != nil {
		if strings.Contains(strings.ToLower(result.Error.Error()), "no such table") {
			return nil
		}
		return result.Error
	}
	return ErrDestinationScopePaused
}

func proactiveCoordinatorReserve(tx *gorm.DB, scope, scannerToken string, now time.Time) error {
	if !tx.Migrator().HasTable(&models.DestinationScopeCoordinator{}) {
		return nil
	}
	var coordinator models.DestinationScopeCoordinator
	result := tx.Where("destination_scope = ?", scope).First(&coordinator)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return nil
	}
	if result.Error != nil && strings.Contains(strings.ToLower(result.Error.Error()), "no such table") {
		return nil
	}
	if result.Error != nil {
		return result.Error
	}
	if err := tx.Model(&coordinator).UpdateColumn("updated_at", gorm.Expr("updated_at")).Error; err != nil {
		return err
	}
	if scannerToken != "" {
		if coordinator.ScannerLeaseToken != scannerToken || coordinator.ScannerLeaseUntil == nil || !coordinator.ScannerLeaseUntil.After(now) {
			return ErrCoordinatorConflict
		}
	} else if coordinator.ScannerLeaseToken != "" && coordinator.ScannerLeaseUntil != nil && coordinator.ScannerLeaseUntil.After(now) {
		return ErrCoordinatorConflict
	}
	return nil
}

func classifyReserveOutcome(pending []LocalSnapshot, selected map[string][]LocalSnapshot, accounts []models.QuotaAccount, usage map[uint]int64, taskLimit int64, now time.Time) string {
	if len(pending) == 0 {
		return models.ReserveClassReserved
	}
	if len(selected) == 0 {
		enabled := 0
		blocked := 0
		allBudgetExhausted := true
		for _, account := range accounts {
			if !account.Enabled {
				continue
			}
			enabled++
			if models.IsUnavailableForProactiveTransfers(account, now) {
				blocked++
				continue
			}
			if usage[account.ID] < account.BudgetBytes {
				allBudgetExhausted = false
			}
		}
		if enabled == 0 {
			if blocked > 0 {
				return models.ReserveClassProviderBlocked
			}
			return models.ReserveClassDisabled
		}
		if blocked == enabled {
			return models.ReserveClassProviderBlocked
		}
		if blocked == 0 && allBudgetExhausted {
			return models.ReserveClassBudgetExhausted
		}
		for _, snapshot := range pending {
			fits := false
			for _, account := range accounts {
				if !models.IsUnavailableForProactiveTransfers(account, now) && snapshot.SizeBytes <= account.BudgetBytes && snapshot.SizeBytes <= taskLimit {
					fits = true
				}
			}
			if !fits {
				return models.ReserveClassOversize
			}
		}
		return models.ReserveClassNoFit
	}
	if taskLimit >= 0 {
		return models.ReserveClassTaskCeiling
	}
	return models.ReserveClassNoFit
}

func accountWideBlocker(tx *gorm.DB, accountIDs []uint, scope string, now time.Time) (bool, time.Time, error) {
	var wake time.Time
	blocked := false
	addWake := func(candidate *time.Time) {
		if candidate == nil {
			return
		}
		if !candidate.After(now) {
			wake = now
			return
		}
		if wake.IsZero() || candidate.Before(wake) {
			wake = *candidate
		}
	}
	var accounts []models.QuotaAccount
	if err := tx.Where("id IN ?", accountIDs).Find(&accounts).Error; err != nil {
		return false, time.Time{}, err
	}
	for _, account := range accounts {
		candidate, err := nextAccountRecoveryTx(tx, account, now)
		if err != nil {
			return false, time.Time{}, err
		}
		if candidate != nil && candidate.After(now) && (wake.IsZero() || candidate.Before(wake)) {
			wake = *candidate
		}
	}
	var batches []models.RotationQuotaBatch
	if err := tx.Where("quota_account_id IN ? AND destination_scope <> ? AND state IN ?", accountIDs, scope, []string{models.BatchStatePlanned, models.BatchStateReserved, models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Find(&batches).Error; err != nil {
		return false, time.Time{}, err
	}
	for _, batch := range batches {
		blocked = true
		if batch.LeaseUntil != nil {
			addWake(batch.LeaseUntil)
		} else if batch.State == models.BatchStateReserved || batch.State == models.BatchStatePlanned {
			reconcileAt := now.Add(time.Minute)
			addWake(&reconcileAt)
		}
	}
	var reservations []models.QuotaReservation
	if err := tx.Model(&models.QuotaReservation{}).
		Joins("JOIN rotation_quota_batches ON rotation_quota_batches.id = quota_reservations.batch_id").
		Where("quota_reservations.quota_account_id IN ? AND rotation_quota_batches.destination_scope <> ? AND quota_reservations.state IN ?", accountIDs, scope, []string{models.ReservationStateHeld, models.ReservationStateActive, models.ReservationStateUnknown}).
		Find(&reservations).Error; err != nil {
		return false, time.Time{}, err
	}
	for _, reservation := range reservations {
		blocked = true
		addWake(reservation.ExpiresAt)
	}
	if wake.IsZero() {
		wake = now.Add(time.Minute)
	}
	return blocked, wake, nil
}

// AccountWideBlocker is the shared conflict rule used by reservation and
// manual maintenance claims. It deliberately includes unknown ledger state.
func AccountWideBlocker(tx *gorm.DB, accountIDs []uint, scope string, now time.Time) (bool, time.Time, error) {
	return accountWideBlocker(tx, accountIDs, scope, now)
}

func pendingForExistingBatches(tx *gorm.DB, snapshots []LocalSnapshot, batches []models.RotationQuotaBatch) ([]LocalSnapshot, error) {
	ids := make([]uint, len(batches))
	for i := range batches {
		ids[i] = batches[i].ID
	}
	var keys []string
	if err := tx.Model(&models.RotationQuotaBatchFile{}).Where("batch_id IN ?", ids).Pluck("snapshot_key", &keys).Error; err != nil {
		return nil, err
	}
	claimed := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		claimed[key] = struct{}{}
	}
	ordered := append([]LocalSnapshot(nil), snapshots...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].RelativePath < ordered[j].RelativePath })
	pending := make([]LocalSnapshot, 0, len(ordered))
	for _, snapshot := range ordered {
		if _, ok := claimed[snapshot.SnapshotKey]; !ok {
			pending = append(pending, snapshot)
		}
	}
	return pending, nil
}

func (s *Service) ReleaseHeldBatch(batchID uint) error {
	if s == nil || s.DB == nil {
		return errors.New("quota service database is required")
	}
	maxRetries := s.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	for attempt := 0; attempt < maxRetries; attempt++ {
		err := s.releaseHeldAttempt(batchID)
		if err == nil {
			return nil
		}
		if !isBusyError(err) || attempt == maxRetries-1 {
			return err
		}
		sleep := s.RetrySleep
		if sleep == nil {
			sleep = time.Sleep
		}
		sleep(s.retryDelay(attempt))
	}
	return errors.New("quota release retries exhausted")
}

func (s *Service) releaseHeldAttempt(batchID uint) error {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		return releaseHeldBatchTx(tx, batchID, now)
	})
}

// ReleaseHeldBatchesForAccount releases only batches that are provably safe
// to discard before a recovery probe. It intentionally leaves started,
// unknown, or inconsistent work untouched so the caller can keep the account
// blocked after its final recheck.
func ReleaseHeldBatchesForAccount(tx *gorm.DB, accountID uint, now time.Time) error {
	if tx == nil {
		return errors.New("quota transaction is required")
	}
	var batches []models.RotationQuotaBatch
	if err := tx.Where("quota_account_id = ? AND state IN ?", accountID, []string{models.BatchStateReserved, models.BatchStatePlanned}).Order("id ASC").Find(&batches).Error; err != nil {
		return err
	}
	for _, batch := range batches {
		err := releaseHeldBatchTx(tx, batch.ID, now)
		if errors.Is(err, ErrHeldBatchNotSafe) {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func releaseHeldBatchTx(tx *gorm.DB, batchID uint, now time.Time) error {
	var batch models.RotationQuotaBatch
	if err := tx.First(&batch, batchID).Error; err != nil {
		return err
	}
	if batch.StartedAt != nil || batch.ProcessID != 0 || (batch.State != models.BatchStateReserved && batch.State != models.BatchStatePlanned) {
		return fmt.Errorf("batch %d: %w", batchID, ErrHeldBatchNotSafe)
	}
	var account models.QuotaAccount
	if err := tx.First(&account, batch.QuotaAccountID).Error; err != nil {
		return err
	}
	if err := validateAccounts([]models.QuotaAccount{account}); err != nil {
		return err
	}
	result := tx.Model(&models.QuotaAccount{}).Where("id = ?", account.ID).UpdateColumn("updated_at", gorm.Expr("updated_at"))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("quota account %d disappeared while acquiring writer lock", account.ID)
	}
	var files []models.RotationQuotaBatchFile
	if err := tx.Where("batch_id = ?", batchID).Find(&files).Error; err != nil {
		return err
	}
	var reservations []models.QuotaReservation
	if err := tx.Where("batch_id = ?", batchID).Find(&reservations).Error; err != nil {
		return err
	}
	if len(files) == 0 || len(reservations) == 0 || len(files) != len(reservations) {
		return fmt.Errorf("batch %d: %w", batchID, ErrHeldBatchNotSafe)
	}
	fileIDs := make(map[uint]struct{}, len(files))
	for _, file := range files {
		if file.State != models.BatchFileStateHeld {
			return fmt.Errorf("batch %d: %w", batchID, ErrHeldBatchNotSafe)
		}
		fileIDs[file.ID] = struct{}{}
	}
	reservationFileIDs := make(map[uint]struct{}, len(reservations))
	for _, reservation := range reservations {
		if reservation.State != models.ReservationStateHeld || reservation.QuotaAccountID != batch.QuotaAccountID {
			return fmt.Errorf("batch %d: %w", batchID, ErrHeldBatchNotSafe)
		}
		if _, ok := fileIDs[reservation.BatchFileID]; !ok {
			return fmt.Errorf("batch %d: %w", batchID, ErrHeldBatchNotSafe)
		}
		if _, duplicate := reservationFileIDs[reservation.BatchFileID]; duplicate {
			return fmt.Errorf("batch %d: %w", batchID, ErrHeldBatchNotSafe)
		}
		reservationFileIDs[reservation.BatchFileID] = struct{}{}
	}
	result = tx.Model(&models.QuotaReservation{}).Where("batch_id = ? AND state = ?", batchID, models.ReservationStateHeld).Updates(map[string]interface{}{
		"state": models.ReservationStateReleased, "released_at": now,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != int64(len(reservations)) {
		return fmt.Errorf("batch %d reservations changed while releasing", batchID)
	}
	result = tx.Model(&models.RotationQuotaBatchFile{}).Where("batch_id = ? AND state = ?", batchID, models.BatchFileStateHeld).Updates(map[string]interface{}{
		"state": models.BatchFileStateFailed, "last_error": "released before start by quota recovery probe",
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != int64(len(files)) {
		return fmt.Errorf("batch %d files changed while releasing", batchID)
	}
	result = tx.Model(&models.RotationQuotaBatch{}).Where("id = ? AND state IN ? AND started_at IS NULL AND process_id = 0", batchID, []string{models.BatchStatePlanned, models.BatchStateReserved}).Updates(map[string]interface{}{
		"state": models.BatchStateCanceled, "finished_at": now,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("batch %d changed while releasing", batchID)
	}
	return ReconcileAccountWindowAnchor(tx, batch.QuotaAccountID, now)
}

func (s *Service) generateToken() string {
	if s.TokenGenerator != nil {
		return s.TokenGenerator()
	}
	return randomToken()
}

func randomToken() string {
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		panic(fmt.Sprintf("failed to generate quota identity token: %v", err))
	}
	return hex.EncodeToString(bytes)
}

func stableToken(values ...interface{}) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = fmt.Fprintf(hash, "%v\x00", value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func requestFingerprint(task models.Task, sourceRoot, resolvedConfig, destinationPath string, remotes []string, quotaKeys map[string]string, snapshots []LocalSnapshot) string {
	ordered := append([]LocalSnapshot(nil), snapshots...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].RelativePath == ordered[j].RelativePath {
			return ordered[i].SnapshotKey < ordered[j].SnapshotKey
		}
		return ordered[i].RelativePath < ordered[j].RelativePath
	})
	type binding struct {
		Remote   string `json:"remote"`
		QuotaKey string `json:"quota_key"`
	}
	type snapshotPayload struct {
		RelativePath string `json:"relative_path"`
		SizeBytes    int64  `json:"size_bytes"`
		MtimeNS      int64  `json:"mtime_ns"`
		Device       int64  `json:"device"`
		Inode        int64  `json:"inode"`
		SnapshotKey  string `json:"snapshot_key"`
	}
	payload := struct {
		SchemaVersion    string            `json:"schema_version"`
		TaskID           uint              `json:"task_id"`
		QuotaLimitBytes  int64             `json:"quota_limit_bytes"`
		TransferMode     string            `json:"transfer_mode"`
		SourceRoot       string            `json:"source_root"`
		SourceRootDevice int64             `json:"source_root_device"`
		SourceRootInode  int64             `json:"source_root_inode"`
		RcloneConfigPath string            `json:"rclone_config_path"`
		DestinationPath  string            `json:"destination_path"`
		Bindings         []binding         `json:"bindings"`
		Snapshots        []snapshotPayload `json:"snapshots"`
	}{
		SchemaVersion: "phase2-v2", TaskID: task.ID, QuotaLimitBytes: task.RotationQuotaLimitBytes,
		TransferMode: task.TransferMode, SourceRoot: filepath.Clean(sourceRoot),
		RcloneConfigPath: resolvedConfig, DestinationPath: models.CanonicalDestinationPath(destinationPath),
		Bindings: make([]binding, 0, len(remotes)), Snapshots: make([]snapshotPayload, 0, len(ordered)),
	}
	if len(ordered) > 0 {
		payload.SourceRootDevice = ordered[0].RootDevice
		payload.SourceRootInode = ordered[0].RootInode
	}
	for _, remote := range remotes {
		payload.Bindings = append(payload.Bindings, binding{Remote: remote, QuotaKey: quotaKeys[remote]})
	}
	for _, snapshot := range ordered {
		payload.Snapshots = append(payload.Snapshots, snapshotPayload{RelativePath: snapshot.RelativePath, SizeBytes: snapshot.SizeBytes, MtimeNS: snapshot.MtimeNS, Device: snapshot.Device, Inode: snapshot.Inode, SnapshotKey: snapshot.SnapshotKey})
	}
	data, _ := json.Marshal(payload)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func loadAccounts(tx *gorm.DB, keys map[string]string) ([]models.QuotaAccount, error) {
	seen := make(map[string]struct{}, len(keys))
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		values = append(values, key)
	}
	sort.Strings(values)
	var accounts []models.QuotaAccount
	if err := tx.Where("quota_key IN ?", values).Order("id, quota_key").Find(&accounts).Error; err != nil {
		return nil, err
	}
	if len(accounts) != len(values) {
		return nil, errors.New("one or more quota accounts are missing")
	}
	return accounts, nil
}

func validateAccounts(accounts []models.QuotaAccount) error {
	maxWindowSeconds := int64((time.Duration(1<<63 - 1)) / time.Second)
	for _, account := range accounts {
		if account.BudgetBytes < 0 {
			return fmt.Errorf("quota account %d has a negative budget", account.ID)
		}
		if account.WindowSeconds <= 0 || int64(account.WindowSeconds) > maxWindowSeconds {
			return fmt.Errorf("quota account %d has an invalid window", account.ID)
		}
	}
	return nil
}

func accountIDs(accounts []models.QuotaAccount) []uint {
	ids := make([]uint, len(accounts))
	for i := range accounts {
		ids[i] = accounts[i].ID
	}
	return ids
}

func accountsByKey(accounts []models.QuotaAccount) map[string]models.QuotaAccount {
	result := make(map[string]models.QuotaAccount, len(accounts))
	for _, account := range accounts {
		result[account.QuotaKey] = account
	}
	return result
}

func rejectActiveBatches(tx *gorm.DB, task models.Task, resolvedConfig, destinationPath string) error {
	if err := checkBatchScopeStates(tx, "task_id = ?", task.ID); err != nil {
		return err
	}
	scope := models.DestinationScope(resolvedConfig, destinationPath)
	if err := checkBatchScopeStates(tx, "destination_scope = ?", scope); err != nil {
		return err
	}
	return nil
}

func checkBatchScopeStates(tx *gorm.DB, condition string, value interface{}) error {
	var batches []models.RotationQuotaBatch
	if err := tx.Select("id, state").Where(condition, value).Find(&batches).Error; err != nil {
		return err
	}
	for _, batch := range batches {
		if !models.IsKnownBatchState(batch.State) {
			return fmt.Errorf("quota ledger corruption: batch %d has unknown state %q", batch.ID, batch.State)
		}
		if models.IsActiveBatchState(batch.State) {
			return ErrActiveBatch
		}
		if !models.IsTerminalBatchState(batch.State) {
			return fmt.Errorf("quota ledger corruption: batch %d has non-terminal unrecognized state %q", batch.ID, batch.State)
		}
	}
	return nil
}

func accountUsage(tx *gorm.DB, ids []uint, now time.Time) (map[uint]int64, error) {
	var reservations []models.QuotaReservation
	if err := tx.Where("quota_account_id IN ?", ids).Find(&reservations).Error; err != nil {
		return nil, err
	}
	usage := make(map[uint]int64, len(ids))
	for _, reservation := range reservations {
		if reservation.Bytes < 0 {
			return nil, fmt.Errorf("quota ledger corruption: reservation %d has negative bytes", reservation.ID)
		}
		switch reservation.State {
		case models.ReservationStateReleased, models.ReservationStateExpired:
			continue
		case models.ReservationStateCommitted:
			// A missing timestamp is historical uncertainty and remains charged
			// until an idempotent migration supplies a conservative timestamp.
			if reservation.CommittedAt != nil && !reservation.CommittedAt.After(now.Add(-24*time.Hour)) {
				continue
			}
		case models.ReservationStateHeld, models.ReservationStateActive, models.ReservationStateUnknown:
		default:
			return nil, fmt.Errorf("quota ledger corruption: reservation %d has invalid state %q", reservation.ID, reservation.State)
		}
		updated, err := safeAdd(usage[reservation.QuotaAccountID], reservation.Bytes)
		if err != nil {
			return nil, fmt.Errorf("quota ledger corruption: account %d usage overflows int64", reservation.QuotaAccountID)
		}
		usage[reservation.QuotaAccountID] = updated
	}
	return usage, nil
}

const defaultBatchFiles = 5

func packSnapshots(snapshots []LocalSnapshot, remotes []string, keys map[string]string, accounts []models.QuotaAccount, usage map[uint]int64, taskLimit int64, batchFiles int, directoryAccounts map[string]uint, now time.Time) (map[string][]LocalSnapshot, []LocalSnapshot, map[string]uint, error) {
	return packSnapshotsWithForecast(snapshots, remotes, keys, accounts, usage, nil, taskLimit, batchFiles, directoryAccounts, now)
}

func packSnapshotsWithForecast(snapshots []LocalSnapshot, remotes []string, keys map[string]string, accounts []models.QuotaAccount, usage map[uint]int64, forecast3h map[uint]int64, taskLimit int64, batchFiles int, directoryAccounts map[string]uint, now time.Time) (map[string][]LocalSnapshot, []LocalSnapshot, map[string]uint, error) {
	if taskLimit < 0 {
		return nil, nil, nil, fmt.Errorf("rotation quota limit cannot be negative")
	}
	if batchFiles <= 0 {
		batchFiles = defaultBatchFiles
	}
	if directoryAccounts == nil {
		directoryAccounts = make(map[string]uint)
	}
	ordered := append([]LocalSnapshot(nil), snapshots...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].RelativePath < ordered[j].RelativePath })
	byKey := accountsByKey(accounts)
	remoteByAccount := make(map[uint]string, len(remotes))
	for _, remote := range remotes {
		account, ok := byKey[keys[remote]]
		if ok {
			if _, exists := remoteByAccount[account.ID]; !exists {
				remoteByAccount[account.ID] = remote
			}
		}
	}
	selected := make(map[string][]LocalSnapshot)
	newAssignments := make(map[string]uint)
	used := make(map[uint]int64, len(usage))
	for id, bytes := range usage {
		used[id] = bytes
	}
	counts := make(map[string]int)
	pending := make([]LocalSnapshot, 0)
	for _, snapshot := range ordered {
		placed := false
		directory := path.Dir(snapshot.RelativePath)
		assignedAccountID := directoryAccounts[directory]
		candidateRemotes := remotes
		if assignedAccountID != 0 {
			remote, ok := remoteByAccount[assignedAccountID]
			if !ok {
				pending = append(pending, snapshot)
				continue
			}
			candidateRemotes = []string{remote}
		}
		preferred := -1
		for i, remote := range candidateRemotes {
			account := byKey[keys[remote]]
			if models.IsUnavailableForProactiveTransfers(account, now) {
				continue
			}
			limit := account.BudgetBytes
			if limit > models.DefaultRotationQuotaLimitBytes {
				limit = models.DefaultRotationQuotaLimitBytes
			}
			if taskLimit < limit {
				limit = taskLimit
			}
			if used[account.ID] <= limit && snapshot.SizeBytes <= limit-used[account.ID] {
				preferred = i
				break
			}
		}
		if preferred >= 0 {
			remote := candidateRemotes[preferred]
			account := byKey[keys[remote]]
			spill := false
			if forecast3h != nil {
				limit := account.BudgetBytes
				if limit > models.DefaultRotationQuotaLimitBytes {
					limit = models.DefaultRotationQuotaLimitBytes
				}
				if taskLimit < limit {
					limit = taskLimit
				}
				remaining := limit - used[account.ID]
				spill = forecast3h[account.ID] >= remaining
			}
			if preferred == 0 && spill && len(candidateRemotes) > 1 {
				for i := 1; i < len(candidateRemotes); i++ {
					candidate := candidateRemotes[i]
					candidateAccount := byKey[keys[candidate]]
					if models.IsUnavailableForProactiveTransfers(candidateAccount, now) || counts[candidate] >= batchFiles {
						continue
					}
					limit := candidateAccount.BudgetBytes
					if limit > models.DefaultRotationQuotaLimitBytes {
						limit = models.DefaultRotationQuotaLimitBytes
					}
					if taskLimit < limit {
						limit = taskLimit
					}
					if used[candidateAccount.ID] <= limit && snapshot.SizeBytes <= limit-used[candidateAccount.ID] {
						remote, account = candidate, candidateAccount
						preferred = i
						break
					}
				}
			}
			if preferred == 0 && counts[remote] >= batchFiles {
				if assignedAccountID != 0 {
					preferred = -1
				} else {
					for i := 1; i < len(candidateRemotes); i++ {
						candidate := candidateRemotes[i]
						candidateAccount := byKey[keys[candidate]]
						if models.IsUnavailableForProactiveTransfers(candidateAccount, now) || counts[candidate] >= batchFiles {
							continue
						}
						limit := candidateAccount.BudgetBytes
						if limit > models.DefaultRotationQuotaLimitBytes {
							limit = models.DefaultRotationQuotaLimitBytes
						}
						if taskLimit < limit {
							limit = taskLimit
						}
						if used[candidateAccount.ID] <= limit && snapshot.SizeBytes <= limit-used[candidateAccount.ID] {
							remote, account = candidate, candidateAccount
							preferred = i
							break
						}
					}
				}
			}
			if preferred >= 0 && counts[remote] < batchFiles {
				selected[remote] = append(selected[remote], snapshot)
				counts[remote]++
				updated, err := safeAdd(used[account.ID], snapshot.SizeBytes)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("quota packing overflows account %d", account.ID)
				}
				used[account.ID] = updated
				if assignedAccountID == 0 {
					directoryAccounts[directory] = account.ID
					newAssignments[directory] = account.ID
				}
				placed = true
			}
		}
		if !placed {
			pending = append(pending, snapshot)
		}
	}
	return selected, pending, newAssignments, nil
}

func totalBytes(snapshots []LocalSnapshot) (int64, error) {
	var total int64
	for _, snapshot := range snapshots {
		var err error
		total, err = safeAdd(total, snapshot.SizeBytes)
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func isBusyError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database table is locked") || strings.Contains(message, "busy")
}

var ErrQuotaWindowOverdrawn = errors.New("quota account has more than 700,000,000,000 bytes of effective usage")
var ErrQuotaWindowUninitialized = errors.New("quota account rolling state is not initialized")
var ErrQuotaWindowInvalid = errors.New("quota account rolling state is invalid")

func ValidateAccountWindow(account models.QuotaAccount, now time.Time) error {
	if account.FixedWindowMigrationVersion > models.FixedWindowMigrationVersion || account.WindowSeconds <= 0 || account.BudgetBytes < 0 {
		return fmt.Errorf("quota account %d: %w", account.ID, ErrQuotaWindowInvalid)
	}
	if account.QuotaPolicyVersion > models.RollingQuotaPolicyVersion {
		return fmt.Errorf("quota account %d: %w", account.ID, ErrQuotaWindowInvalid)
	}
	_ = now
	return nil
}

func AdvanceAccountWindow(database *gorm.DB, accountID uint, now time.Time) (models.QuotaAccount, error) {
	var account models.QuotaAccount
	if database == nil {
		return account, errors.New("quota database is required")
	}
	err := database.Transaction(func(tx *gorm.DB) error {
		var err error
		account, err = AdvanceAccountWindowTx(tx, accountID, now)
		return err
	})
	return account, err
}

func AdvanceAccountWindows(database *gorm.DB, accountIDs []uint, now time.Time) ([]models.QuotaAccount, error) {
	accounts := make([]models.QuotaAccount, 0, len(accountIDs))
	if database == nil {
		return accounts, errors.New("quota database is required")
	}
	err := database.Transaction(func(tx *gorm.DB) error {
		for _, accountID := range accountIDs {
			account, err := AdvanceAccountWindowTx(tx, accountID, now)
			if err != nil {
				return err
			}
			accounts = append(accounts, account)
		}
		return nil
	})
	return accounts, err
}

// InitializeAccountWindows is the startup policy pass. Account advancement
// owns the account lock and performs the durable fixed-v1-to-rolling migration
// before normal ledger cleanup, so concurrent startup callers cannot duplicate it.
func InitializeAccountWindows(database *gorm.DB, now time.Time) error {
	if database == nil {
		return errors.New("quota database is required")
	}
	return database.Transaction(func(tx *gorm.DB) error {
		var accountIDs []uint
		if err := tx.Model(&models.QuotaAccount{}).Order("id ASC").Pluck("id", &accountIDs).Error; err != nil {
			return err
		}
		for _, accountID := range accountIDs {
			if _, err := AdvanceAccountWindowTx(tx, accountID, now); err != nil {
				return err
			}
		}
		return retireQuotaProbeAttemptsTx(tx, now)
	})
}

// AdvanceAccountWindowTx must run after the caller has entered its writer
// transaction. It locks one account and applies rolling reservation expiry.
func AdvanceAccountWindowTx(tx *gorm.DB, accountID uint, now time.Time) (models.QuotaAccount, error) {
	return advanceRollingAccountTx(tx, accountID, now)
}

// advanceRollingAccountTx is the only runtime quota rollover. The legacy
// WindowStartedAt and ExpiresAt columns are deliberately ignored as clocks.
func advanceRollingAccountTx(tx *gorm.DB, accountID uint, now time.Time) (models.QuotaAccount, error) {
	var account models.QuotaAccount
	if tx == nil {
		return account, errors.New("quota transaction is required")
	}
	lock := tx.Model(&models.QuotaAccount{}).Where("id = ?", accountID).UpdateColumn("updated_at", gorm.Expr("updated_at"))
	if lock.Error != nil {
		return account, lock.Error
	}
	if lock.RowsAffected != 1 {
		return account, fmt.Errorf("quota account %d disappeared while advancing window", accountID)
	}
	if err := tx.First(&account, accountID).Error; err != nil {
		return account, err
	}
	if account.FixedWindowMigrationVersion > models.FixedWindowMigrationVersion || account.QuotaPolicyVersion > models.RollingQuotaPolicyVersion {
		return account, fmt.Errorf("quota account %d: %w", account.ID, ErrQuotaWindowInvalid)
	}
	if account.QuotaPolicyVersion < models.RollingQuotaPolicyVersion {
		if err := migrateAccountToRollingTx(tx, account, now); err != nil {
			return account, err
		}
		if err := tx.First(&account, account.ID).Error; err != nil {
			return account, err
		}
	}
	if account.BudgetBytes < 0 {
		return account, fmt.Errorf("quota account %d has a negative budget", accountID)
	}
	if account.BudgetBytes == 700*1024*1024*1024 {
		if err := tx.Model(&models.QuotaAccount{}).Where("id = ?", account.ID).Update("budget_bytes", models.DefaultRotationQuotaLimitBytes).Error; err != nil {
			return account, err
		}
		account.BudgetBytes = models.DefaultRotationQuotaLimitBytes
	}
	if account.WindowSeconds != models.DefaultQuotaWindowSeconds || account.RecoveryState == "" {
		updates := map[string]interface{}{
			"window_seconds": models.DefaultQuotaWindowSeconds,
		}
		if account.RecoveryState == "" {
			updates["recovery_state"] = models.QuotaRecoveryStateAvailable
		}
		if err := tx.Model(&models.QuotaAccount{}).Where("id = ?", account.ID).Updates(updates).Error; err != nil {
			return account, err
		}
		if err := tx.First(&account, account.ID).Error; err != nil {
			return account, err
		}
	}

	if err := expireRollingReservationsTx(tx, account.ID, now); err != nil {
		return account, err
	}
	var unresolved []models.QuotaReservation
	if err := tx.Where("quota_account_id = ? AND state IN ? AND expires_at IS NOT NULL AND expires_at > ?", account.ID, []string{models.ReservationStateHeld, models.ReservationStateActive, models.ReservationStateUnknown}, now).Find(&unresolved).Error; err != nil {
		return account, err
	}
	for _, reservation := range unresolved {
		if reservation.State == models.ReservationStateUnknown {
			if err := preserveUnknownReservationRetryHintTx(tx, reservation, *reservation.ExpiresAt); err != nil {
				return account, err
			}
		}
	}
	usage, err := accountUsage(tx, []uint{account.ID}, now)
	if err != nil {
		return account, err
	}
	if usage[account.ID] > models.DefaultRotationQuotaLimitBytes {
		return account, fmt.Errorf("account %d: %w", account.ID, ErrQuotaWindowOverdrawn)
	}
	accountUpdates := map[string]interface{}{"window_seconds": models.DefaultQuotaWindowSeconds, "fixed_window_migration_version": models.FixedWindowMigrationVersion, "quota_policy_version": models.RollingQuotaPolicyVersion, "next_probe_at": nil, "probe_claim_token": "", "probe_claim_until": nil}
	if account.RecoveryState == "" || (account.RecoveryState == models.QuotaRecoveryStateExhausted && account.CampaignCooldownUntil == nil && (account.ProviderBlockedUntil == nil || !account.ProviderBlockedUntil.After(now))) {
		accountUpdates["recovery_state"] = models.QuotaRecoveryStateAvailable
	}
	if account.CampaignCooldownUntil != nil && !account.CampaignCooldownUntil.After(now) {
		accountUpdates["cooldown_until"] = nil
	}
	if account.ProviderBlockedUntil != nil && !account.ProviderBlockedUntil.After(now) {
		accountUpdates["provider_blocked_until"] = nil
	}
	if err := tx.Model(&models.QuotaAccount{}).Where("id = ?", account.ID).Updates(accountUpdates).Error; err != nil {
		return account, err
	}
	var refreshed models.QuotaAccount
	if err := tx.First(&refreshed, account.ID).Error; err != nil {
		return account, err
	}
	return refreshed, nil
}

func migrateAccountToRollingTx(tx *gorm.DB, account models.QuotaAccount, now time.Time) error {
	var reservations []models.QuotaReservation
	if err := tx.Where("quota_account_id = ?", account.ID).Order("id ASC").Find(&reservations).Error; err != nil {
		return err
	}
	cutoff := now.Add(-24 * time.Hour)
	for _, reservation := range reservations {
		if reservation.Bytes < 0 {
			return fmt.Errorf("quota ledger corruption: reservation %d has negative bytes", reservation.ID)
		}
		if reservation.State != models.ReservationStateCommitted {
			continue
		}
		var evidence *time.Time
		var err error
		if reservation.CommittedAt != nil {
			value := reservation.CommittedAt.UTC()
			evidence = &value
		} else {
			evidence, err = legacyReservationCompletionEvidenceTx(tx, reservation)
		}
		if err != nil {
			return err
		}
		committedAt, expired := legacyRollingCommittedAt(account, reservation, evidence, now)
		updates := map[string]interface{}{"committed_at": committedAt}
		if expired || !committedAt.After(cutoff) {
			updates["state"] = models.ReservationStateExpired
			updates["released_at"] = now
		}
		if err := tx.Model(&models.QuotaReservation{}).Where("id = ? AND state = ?", reservation.ID, models.ReservationStateCommitted).Updates(updates).Error; err != nil {
			return err
		}
	}
	updates := map[string]interface{}{
		"window_seconds":                 models.DefaultQuotaWindowSeconds,
		"fixed_window_migration_version": models.FixedWindowMigrationVersion,
		"quota_policy_version":           models.RollingQuotaPolicyVersion,
		"next_probe_at":                  nil,
		"probe_claim_token":              "",
		"probe_claim_until":              nil,
	}
	// Fixed-window exhaustion never becomes a new rolling campaign cooldown.
	// Only the legacy provider boundary itself may remain as an outage hold.
	updates["cooldown_until"] = nil
	if account.ProviderBlockedUntil != nil && account.ProviderBlockedUntil.After(now) {
		updates["provider_blocked_until"] = account.ProviderBlockedUntil
	} else {
		updates["provider_blocked_until"] = nil
	}
	updates["recovery_state"] = models.QuotaRecoveryStateAvailable
	updates["first_exhausted_at"] = nil
	return tx.Model(&models.QuotaAccount{}).Where("id = ? AND quota_policy_version < ?", account.ID, models.RollingQuotaPolicyVersion).Updates(updates).Error
}

func legacyReservationCompletionEvidenceTx(tx *gorm.DB, reservation models.QuotaReservation) (*time.Time, error) {
	if tx.Migrator().HasTable(&models.RotationQuotaBatchFile{}) && reservation.BatchFileID != 0 {
		var file models.RotationQuotaBatchFile
		result := tx.Where("id = ?", reservation.BatchFileID).Limit(1).Find(&file)
		if result.Error != nil {
			return nil, result.Error
		}
		if file.ID != 0 && file.VerifiedAt != nil {
			value := file.VerifiedAt.UTC()
			return &value, nil
		}
	}
	if tx.Migrator().HasTable(&models.RotationQuotaBatch{}) && reservation.BatchID != 0 {
		var batch models.RotationQuotaBatch
		result := tx.Where("id = ?", reservation.BatchID).Limit(1).Find(&batch)
		if result.Error != nil {
			return nil, result.Error
		}
		if batch.ID != 0 && batch.FinishedAt != nil {
			value := batch.FinishedAt.UTC()
			return &value, nil
		}
	}
	return nil, nil
}

func legacyRollingCommittedAt(account models.QuotaAccount, reservation models.QuotaReservation, evidence *time.Time, now time.Time) (time.Time, bool) {
	const rollingWindow = 24 * time.Hour
	deadline, hasDeadline := legacyReservationDeadline(account, reservation, now)
	if hasDeadline {
		candidate := deadline.Add(-rollingWindow)
		validEvidence := evidence != nil && !evidence.After(now)
		if validEvidence && evidence.Before(candidate) {
			candidate = evidence.UTC()
		}
		if candidate.After(now) {
			// A future v1 deadline with no completed work evidence cannot be
			// turned into a new charge beginning at migration time.
			return now.Add(-rollingWindow).UTC(), true
		}
		return candidate, !candidate.After(now.Add(-rollingWindow))
	}
	if evidence != nil && !evidence.After(now) {
		candidate := evidence.UTC()
		return candidate, !candidate.After(now.Add(-rollingWindow))
	}
	// There is no trustworthy legacy clock. Do not translate that uncertainty
	// into a fresh 24h charge from migration time; expire the row at the exact
	// rolling cutoff instead.
	return now.Add(-rollingWindow).UTC(), true
}

func legacyReservationDeadline(account models.QuotaAccount, reservation models.QuotaReservation, now time.Time) (time.Time, bool) {
	var deadline *time.Time
	add := func(candidate time.Time) {
		candidate = candidate.UTC()
		if candidate.IsZero() || (deadline != nil && !candidate.Before(*deadline)) {
			return
		}
		value := candidate
		deadline = &value
	}
	if reservation.ExpiresAt != nil {
		add(*reservation.ExpiresAt)
	}
	if account.WindowStartedAt != nil && !account.WindowStartedAt.After(now) {
		window := time.Duration(account.WindowSeconds) * time.Second
		if window <= 0 {
			window = 24 * time.Hour
		}
		add(account.WindowStartedAt.Add(window))
	}
	if account.ProviderBlockedUntil != nil && account.ProviderBlockedUntil.After(now) {
		add(*account.ProviderBlockedUntil)
	}
	if deadline == nil {
		return time.Time{}, false
	}
	return *deadline, true
}

func expireRollingReservationsTx(tx *gorm.DB, accountID uint, now time.Time) error {
	cutoff := now.Add(-24 * time.Hour)
	return tx.Model(&models.QuotaReservation{}).
		Where("quota_account_id = ? AND state = ? AND committed_at IS NOT NULL AND committed_at <= ?", accountID, models.ReservationStateCommitted, cutoff).
		Updates(map[string]interface{}{"state": models.ReservationStateExpired, "released_at": now}).Error
}

func nextAccountRecoveryTx(tx *gorm.DB, account models.QuotaAccount, now time.Time) (*time.Time, error) {
	var next *time.Time
	add := func(candidate time.Time) {
		if !candidate.After(now) || (next != nil && !candidate.Before(*next)) {
			return
		}
		value := candidate
		next = &value
	}
	var hardHoldUntil *time.Time
	addHardHold := func(candidate *time.Time) {
		if candidate == nil || !candidate.After(now) || (hardHoldUntil != nil && !candidate.After(*hardHoldUntil)) {
			return
		}
		value := candidate.UTC()
		hardHoldUntil = &value
	}
	addHardHold(account.CampaignCooldownUntil)
	addHardHold(account.ProviderBlockedUntil)
	if hardHoldUntil != nil {
		return hardHoldUntil, nil
	}
	var reservations []models.QuotaReservation
	if err := tx.Where("quota_account_id = ? AND state IN ?", account.ID, []string{models.ReservationStateHeld, models.ReservationStateActive, models.ReservationStateUnknown, models.ReservationStateCommitted}).Find(&reservations).Error; err != nil {
		return nil, err
	}
	for _, reservation := range reservations {
		switch reservation.State {
		case models.ReservationStateCommitted:
			if reservation.CommittedAt == nil {
				add(now.Add(24 * time.Hour))
			} else {
				add(reservation.CommittedAt.Add(24 * time.Hour))
			}
		case models.ReservationStateHeld, models.ReservationStateActive, models.ReservationStateUnknown:
			if reservation.ExpiresAt != nil {
				add(*reservation.ExpiresAt)
			}
		}
	}
	return next, nil
}

// NextAccountRecovery returns the earliest durable recovery point for an
// account. It does not mutate the ledger and is safe for status projections.
func NextAccountRecovery(tx *gorm.DB, accountID uint, now time.Time) (*time.Time, error) {
	var account models.QuotaAccount
	if err := tx.First(&account, accountID).Error; err != nil {
		return nil, err
	}
	return nextAccountRecoveryTx(tx, account, now)
}

func accountForecasts(tx *gorm.DB, ids []uint, now time.Time) (map[uint]int64, error) {
	result := make(map[uint]int64, len(ids))
	for _, id := range ids {
		forecast, err := Forecast3HBytes(tx, id, now)
		if err != nil {
			return nil, err
		}
		result[id] = forecast
	}
	return result, nil
}

// Forecast3HBytes derives a conservative forecast from durable completed
// reservations. The observed interval begins with the oldest recent commit.
func Forecast3HBytes(tx *gorm.DB, accountID uint, now time.Time) (int64, error) {
	cutoff := now.Add(-24 * time.Hour)
	var reservations []models.QuotaReservation
	if err := tx.Where("quota_account_id = ? AND state = ? AND committed_at IS NOT NULL AND committed_at > ?", accountID, models.ReservationStateCommitted, cutoff).Find(&reservations).Error; err != nil {
		return 0, err
	}
	if len(reservations) == 0 {
		return 0, nil
	}
	var total int64
	oldest := now
	for _, reservation := range reservations {
		var batch models.RotationQuotaBatch
		if reservation.BatchID != 0 && tx.First(&batch, reservation.BatchID).Error != nil {
			continue
		}
		if err := safeBytesAdd(&total, reservation.Bytes); err != nil {
			return 0, err
		}
		if reservation.CommittedAt.Before(oldest) {
			oldest = *reservation.CommittedAt
		}
	}
	span := now.Sub(oldest)
	if span < time.Second {
		span = time.Second
	}
	forecast := float64(total) * float64(3*time.Hour) / float64(span)
	if forecast >= float64(int64(^uint64(0)>>1)) {
		return int64(^uint64(0) >> 1), nil
	}
	return int64(forecast), nil
}

func safeBytesAdd(target *int64, value int64) error {
	updated, err := safeAdd(*target, value)
	if err != nil {
		return fmt.Errorf("quota forecast overflows int64")
	}
	*target = updated
	return nil
}

// ApplyCommittedQuotaTx updates the local cooldown after a successful commit.
// The caller must already be in the completion transaction.
func ApplyCommittedQuotaTx(tx *gorm.DB, accountID uint, committedAt time.Time) error {
	var account models.QuotaAccount
	if err := tx.First(&account, accountID).Error; err != nil {
		return err
	}
	usage, err := rollingCommittedUsage(tx, accountID, committedAt)
	if err != nil {
		return err
	}
	limit := account.BudgetBytes
	if limit > models.DefaultRotationQuotaLimitBytes || limit <= 0 {
		limit = models.DefaultRotationQuotaLimitBytes
	}
	if usage < limit {
		return nil
	}
	until := committedAt.Add(24 * time.Hour)
	if account.CampaignCooldownUntil != nil && account.CampaignCooldownUntil.After(until) {
		until = *account.CampaignCooldownUntil
	}
	return tx.Model(&models.QuotaAccount{}).Where("id = ?", accountID).Updates(map[string]interface{}{"cooldown_until": until, "recovery_state": models.QuotaRecoveryStateAvailable}).Error
}

func rollingCommittedUsage(tx *gorm.DB, accountID uint, now time.Time) (int64, error) {
	var reservations []models.QuotaReservation
	if err := tx.Where("quota_account_id = ? AND state = ?", accountID, models.ReservationStateCommitted).Find(&reservations).Error; err != nil {
		return 0, err
	}
	cutoff := now.Add(-24 * time.Hour)
	var total int64
	for _, reservation := range reservations {
		if reservation.CommittedAt != nil && !reservation.CommittedAt.After(cutoff) {
			continue
		}
		if err := safeBytesAdd(&total, reservation.Bytes); err != nil {
			return 0, err
		}
	}
	return total, nil
}

// RollingCommittedBytes returns committed rolling usage across every account.
// It is used only for telemetry comparison and never gates scheduling.
func RollingCommittedBytes(tx *gorm.DB, now time.Time) (int64, error) {
	var reservations []models.QuotaReservation
	if err := tx.Where("state = ?", models.ReservationStateCommitted).Find(&reservations).Error; err != nil {
		return 0, err
	}
	cutoff := now.Add(-24 * time.Hour)
	var total int64
	for _, reservation := range reservations {
		if reservation.CommittedAt != nil && !reservation.CommittedAt.After(cutoff) {
			continue
		}
		if err := safeBytesAdd(&total, reservation.Bytes); err != nil {
			return 0, err
		}
	}
	return total, nil
}

type ManualResetRequest struct {
	TaskID        uint
	AccountID     uint
	ActorIdentity string
	ActorType     string
	RequestedAt   time.Time
	RequestCutoff time.Time
}

type ManualResetResult struct {
	RequestCutoff time.Time
	EffectiveAt   time.Time
	ExpiredRows   int64
}

// ManualReset expires only committed historical quota. The cutoff is captured
// before the transaction starts so a completion committed after the request
// remains durable even if this reset obtains the account lock later.
func ManualReset(database *gorm.DB, accountID uint, now time.Time) error {
	_, err := manualReset(database, ManualResetRequest{AccountID: accountID, RequestedAt: now, RequestCutoff: now}, false)
	return err
}

// ManualResetWithAudit performs the ordinary administrator reset and appends
// its durable event in the same transaction as the quota mutation.
func ManualResetWithAudit(database *gorm.DB, request ManualResetRequest) (ManualResetResult, error) {
	return manualReset(database, request, true)
}

func manualReset(database *gorm.DB, request ManualResetRequest, audit bool) (ManualResetResult, error) {
	result := ManualResetResult{}
	if database == nil {
		return result, errors.New("quota database is required")
	}
	requestedAt := request.RequestedAt.UTC()
	if requestedAt.IsZero() {
		requestedAt = time.Now().UTC()
	}
	cutoff := request.RequestCutoff.UTC()
	if cutoff.IsZero() {
		cutoff = requestedAt
	}
	// These values are deliberately finalized before opening the transaction.
	result.RequestCutoff = cutoff
	err := database.Transaction(func(tx *gorm.DB) error {
		var account models.QuotaAccount
		lock := tx.Model(&models.QuotaAccount{}).Where("id = ?", request.AccountID).UpdateColumn("updated_at", gorm.Expr("updated_at"))
		if lock.Error != nil {
			return lock.Error
		}
		if lock.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		if err := tx.First(&account, request.AccountID).Error; err != nil {
			return err
		}
		effectiveAt := requestedAt
		updated := tx.Model(&models.QuotaReservation{}).
			Where("quota_account_id = ? AND state = ? AND committed_at IS NOT NULL AND committed_at <= ?", request.AccountID, models.ReservationStateCommitted, cutoff).
			Updates(map[string]interface{}{"state": models.ReservationStateExpired, "released_at": effectiveAt})
		if updated.Error != nil {
			return updated.Error
		}
		result.ExpiredRows = updated.RowsAffected
		if err := tx.Model(&models.QuotaAccount{}).Where("id = ?", request.AccountID).Updates(map[string]interface{}{
			// This is the intentional campaign cooldown only. Provider block and
			// recovery evidence are independent hard safety state and survive.
			"cooldown_until":       nil,
			"last_manual_reset_at": effectiveAt,
		}).Error; err != nil {
			return err
		}
		result.EffectiveAt = effectiveAt
		if audit {
			event := models.QuotaManualResetEvent{
				TaskID: request.TaskID, QuotaAccountID: request.AccountID,
				ActorIdentity: request.ActorIdentity, ActorType: request.ActorType,
				RequestedAt: requestedAt, RequestCutoff: cutoff, EffectiveAt: &effectiveAt,
				Outcome: models.QuotaManualResetOutcomeSucceeded,
				Details: fmt.Sprintf("expired %d committed reservations and cleared campaign cooldown", result.ExpiredRows),
			}
			if err := tx.Create(&event).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func preserveUnknownReservationRetryHintTx(tx *gorm.DB, reservation models.QuotaReservation, retryAt time.Time) error {
	if reservation.BatchID == 0 {
		return nil
	}
	var batch models.RotationQuotaBatch
	err := tx.Where("id = ? AND state IN ?", reservation.BatchID, []string{models.BatchStatePlanned, models.BatchStateReserved, models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).First(&batch).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if batch.LeaseUntil == nil || batch.LeaseUntil.After(retryAt) {
		return tx.Model(&models.RotationQuotaBatch{}).Where("id = ?", batch.ID).Update("lease_until", retryAt).Error
	}
	return nil
}

func retireQuotaProbeAttemptsTx(tx *gorm.DB, now time.Time, accountIDs ...uint) error {
	if !tx.Migrator().HasTable(&models.QuotaProbeAttempt{}) {
		return nil
	}
	query := tx.Model(&models.QuotaProbeAttempt{}).Where("state IN ?", []string{models.ProbeAttemptStatePending, models.ProbeAttemptStateClaimed, models.ProbeAttemptStateRunning, models.ProbeAttemptStateUnknown})
	if len(accountIDs) > 0 {
		query = query.Where("quota_account_id IN ?", accountIDs)
	}
	return query.Updates(map[string]interface{}{"state": models.ProbeAttemptStateCanceled, "phase": models.ProbePhaseFinished, "finished_at": now, "lease_token": "", "lease_until": nil, "process_id": 0, "process_start_token": "", "last_error": "quota recovery probes retired by fixed-window policy"}).Error
}

// ReconcileAccountWindowAnchor is retained for callers from older quota
// mutations; fixed-window policy never clears an initialized anchor.
func ReconcileAccountWindowAnchor(tx *gorm.DB, accountID uint, now time.Time) error {
	_, err := AdvanceAccountWindowTx(tx, accountID, now)
	return err
}

// EffectiveAccountUsage returns current ledger usage for one account. It is
// exported for durable recovery transactions that must prove a logical reset
// left no active quota usage before reopening an account.
func EffectiveAccountUsage(tx *gorm.DB, accountID uint, now time.Time) (int64, error) {
	usage, err := accountUsage(tx, []uint{accountID}, now)
	if err != nil {
		return 0, err
	}
	return usage[accountID], nil
}
