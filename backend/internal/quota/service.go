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
		maxRetries = 3
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
		if err := cleanExpiredHeld(tx, accountIDs(accounts), transactionNow); err != nil {
			return err
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
		selected, pending, err := packSnapshots(req.Snapshots, remotes, quotaKeys, accounts, usage, req.Task.RotationQuotaLimitBytes, transactionNow)
		if err != nil {
			return err
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
				TaskID:                  req.Task.ID,
				QuotaAccountID:          account.ID,
				DestinationScope:        models.DestinationScope(resolvedConfig, destinationPath),
				SourceRoot:              req.SourceRoot,
				SourceRootDevice:        req.Snapshots[0].RootDevice,
				SourceRootInode:         req.Snapshots[0].RootInode,
				DestinationRemote:       remote,
				TransferMode:            req.Task.TransferMode,
				DestinationScopeVersion: 1,
				RcloneConfigPath:        resolvedConfig,
				RequestKey:              requestKey,
				RequestFingerprint:      fingerprint,
				DestinationPath:         destinationPath,
				State:                   models.BatchStateReserved,
				OwnerToken:              ownerToken,
				LeaseToken:              s.generateToken(),
				ProcessStartToken:       s.generateToken(),
				ReservedBytes:           reservedBytes,
			}
			if err := tx.Create(&batch).Error; err != nil {
				return err
			}
			expires := transactionNow.Add(time.Duration(max(1, account.WindowSeconds)) * time.Second)
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
					ReservedAt: &transactionNow, ExpiresAt: &expires,
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
			if account.ProviderBlockedUntil != nil && account.ProviderBlockedUntil.After(now) {
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
				if account.Enabled && (account.ProviderBlockedUntil == nil || !account.ProviderBlockedUntil.After(now)) && snapshot.SizeBytes <= account.BudgetBytes && snapshot.SizeBytes <= taskLimit {
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
		addWake(batch.LeaseUntil)
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
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var batch models.RotationQuotaBatch
		if err := tx.First(&batch, batchID).Error; err != nil {
			return err
		}
		if batch.StartedAt != nil || batch.ProcessID != 0 || (batch.State != models.BatchStateReserved && batch.State != models.BatchStatePlanned) {
			return fmt.Errorf("batch %d is not an unstarted held batch", batchID)
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
		if err := tx.First(&account, batch.QuotaAccountID).Error; err != nil {
			return err
		}
		if err := validateAccounts([]models.QuotaAccount{account}); err != nil {
			return err
		}
		var reservations []models.QuotaReservation
		if err := tx.Where("batch_id = ?", batchID).Find(&reservations).Error; err != nil {
			return err
		}
		if len(reservations) == 0 {
			return fmt.Errorf("batch %d has no reservations", batchID)
		}
		for _, reservation := range reservations {
			if reservation.State != models.ReservationStateHeld || reservation.QuotaAccountID != batch.QuotaAccountID {
				return fmt.Errorf("batch %d has a non-held reservation", batchID)
			}
		}
		transactionNow := now()
		result = tx.Model(&models.QuotaReservation{}).Where("batch_id = ? AND state = ?", batchID, models.ReservationStateHeld).Updates(map[string]interface{}{
			"state": models.ReservationStateReleased, "released_at": transactionNow,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != int64(len(reservations)) {
			return fmt.Errorf("batch %d reservations changed while releasing", batchID)
		}
		result = tx.Model(&models.RotationQuotaBatch{}).Where("id = ? AND state IN ? AND started_at IS NULL AND process_id = 0", batchID, []string{models.BatchStatePlanned, models.BatchStateReserved}).Updates(map[string]interface{}{
			"state": models.BatchStateCanceled, "finished_at": transactionNow,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("batch %d changed while releasing", batchID)
		}
		if err := ReconcileAccountWindowAnchor(tx, batch.QuotaAccountID, transactionNow); err != nil {
			return err
		}
		return nil
	})
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

func cleanExpiredHeld(tx *gorm.DB, accountIDs []uint, now time.Time) error {
	var reservations []models.QuotaReservation
	if err := tx.Where("quota_account_id IN ? AND state = ?", accountIDs, models.ReservationStateHeld).Order("batch_id, id").Find(&reservations).Error; err != nil {
		return err
	}
	byBatch := make(map[uint][]models.QuotaReservation)
	for _, reservation := range reservations {
		byBatch[reservation.BatchID] = append(byBatch[reservation.BatchID], reservation)
	}
	batchIDs := make([]uint, 0, len(byBatch))
	for batchID := range byBatch {
		batchIDs = append(batchIDs, batchID)
	}
	sort.Slice(batchIDs, func(i, j int) bool { return batchIDs[i] < batchIDs[j] })
	for _, batchID := range batchIDs {
		var batch models.RotationQuotaBatch
		if err := tx.First(&batch, batchID).Error; err != nil {
			return err
		}
		if batch.StartedAt != nil || batch.ProcessID != 0 || (batch.State != models.BatchStatePlanned && batch.State != models.BatchStateReserved) {
			continue
		}
		var allReservations []models.QuotaReservation
		if err := tx.Where("batch_id = ?", batchID).Order("id").Find(&allReservations).Error; err != nil {
			return err
		}
		allExpired := len(allReservations) > 0
		for _, reservation := range allReservations {
			if reservation.State != models.ReservationStateHeld || reservation.QuotaAccountID != batch.QuotaAccountID {
				allExpired = false
				break
			}
			if reservation.ExpiresAt == nil || reservation.ExpiresAt.After(now) {
				allExpired = false
				break
			}
		}
		if !allExpired {
			continue
		}
		result := tx.Model(&models.QuotaReservation{}).Where("batch_id = ? AND state = ?", batchID, models.ReservationStateHeld).Updates(map[string]interface{}{"state": models.ReservationStateExpired, "released_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != int64(len(allReservations)) {
			return fmt.Errorf("batch %d reservations changed during expiry cleanup", batchID)
		}
		result = tx.Model(&models.RotationQuotaBatch{}).Where("id = ? AND state IN ? AND started_at IS NULL AND process_id = 0", batchID, []string{models.BatchStatePlanned, models.BatchStateReserved}).Updates(map[string]interface{}{"state": models.BatchStateExpired, "finished_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("batch %d changed during expiry cleanup", batchID)
		}
		if err := ReconcileAccountWindowAnchor(tx, batch.QuotaAccountID, now); err != nil {
			return err
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
			if reservation.ExpiresAt != nil && !reservation.ExpiresAt.After(now) {
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

func packSnapshots(snapshots []LocalSnapshot, remotes []string, keys map[string]string, accounts []models.QuotaAccount, usage map[uint]int64, taskLimit int64, now time.Time) (map[string][]LocalSnapshot, []LocalSnapshot, error) {
	if taskLimit < 0 {
		return nil, nil, fmt.Errorf("rotation quota limit cannot be negative")
	}
	ordered := append([]LocalSnapshot(nil), snapshots...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].RelativePath < ordered[j].RelativePath })
	byKey := accountsByKey(accounts)
	selected := make(map[string][]LocalSnapshot)
	used := make(map[uint]int64, len(usage))
	for id, bytes := range usage {
		used[id] = bytes
	}
	pending := make([]LocalSnapshot, 0)
	for _, snapshot := range ordered {
		placed := false
		for _, remote := range remotes {
			account := byKey[keys[remote]]
			if !account.Enabled || (account.ProviderBlockedUntil != nil && account.ProviderBlockedUntil.After(now)) {
				continue
			}
			limit := account.BudgetBytes
			if taskLimit < limit {
				limit = taskLimit
			}
			if used[account.ID] <= limit && snapshot.SizeBytes <= limit-used[account.ID] {
				selected[remote] = append(selected[remote], snapshot)
				updated, err := safeAdd(used[account.ID], snapshot.SizeBytes)
				if err != nil {
					return nil, nil, fmt.Errorf("quota packing overflows account %d", account.ID)
				}
				used[account.ID] = updated
				placed = true
				break
			}
		}
		if !placed {
			pending = append(pending, snapshot)
		}
	}
	return selected, pending, nil
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

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func isBusyError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database table is locked") || strings.Contains(message, "busy")
}

// ReconcileAccountWindowAnchor keeps QuotaAccount.WindowStartedAt aligned
// with the current reservation usage. Call this from every
// reservation-mutating transaction so the "first exhaustion" anchor is
// set the moment an account hits zero and cleared again on refill.
//   - usage > 0 -> clear WindowStartedAt (a refill happened; the next 24h
//     window restarts when the account is fully used again).
//   - usage == 0 and WindowStartedAt == nil -> set WindowStartedAt to now
//     (first moment the account reached zero).
//   - usage == 0 and WindowStartedAt != nil -> keep (still awaiting reset).
//
// The "next reset" timestamp is computed in the dispatcher as
// WindowStartedAt + WindowSeconds.
func ReconcileAccountWindowAnchor(tx *gorm.DB, accountID uint, now time.Time) error {
	var account models.QuotaAccount
	if err := tx.First(&account, accountID).Error; err != nil {
		return err
	}
	usage, err := accountUsage(tx, []uint{accountID}, now)
	if err != nil {
		return err
	}
	current := usage[accountID]
	switch {
	case current > 0:
		if account.WindowStartedAt == nil {
			return nil
		}
		return tx.Model(&models.QuotaAccount{}).
			Where("id = ?", accountID).
			Update("window_started_at", nil).Error
	case current == 0:
		if account.WindowStartedAt != nil {
			return nil
		}
		anchor := now
		return tx.Model(&models.QuotaAccount{}).
			Where("id = ?", accountID).
			Update("window_started_at", anchor).Error
	default:
		return nil
	}
}
