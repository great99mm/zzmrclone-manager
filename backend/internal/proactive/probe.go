package proactive

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/quota"
)

var (
	ErrProbeActiveWork = errors.New("quota probe is blocked by active quota work")
	ErrProbeLeaseLost  = errors.New("quota probe lease was lost")
)

type ProbeService struct {
	DB             *gorm.DB
	Runner         ProbeRunner
	Inspector      ProcessInspector
	ConfigResolver quota.ConfigResolver
	Now            func() time.Time
	Every          time.Duration
	LeaseDuration  time.Duration
	HeartbeatEvery time.Duration

	stop     chan struct{}
	done     chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once
	stopOnce sync.Once
}

func (s *ProbeService) Start() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.stop = make(chan struct{})
		s.done = make(chan struct{})
		s.ctx, s.cancel = context.WithCancel(context.Background())
		go s.loop()
	})
}

func (s *ProbeService) Stop() {
	if s == nil || s.stop == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		close(s.stop)
		<-s.done
	})
}

func (s *ProbeService) loop() {
	defer close(s.done)
	interval := s.Every
	if interval <= 0 {
		interval = time.Minute
	}
	s.Poll(s.now())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.Poll(s.now())
		case <-s.stop:
			return
		}
	}
}

func (s *ProbeService) Poll(now time.Time) {
	if s == nil || s.DB == nil || s.Runner == nil {
		return
	}
	var accounts []models.QuotaAccount
	if err := s.DB.Where("recovery_state = ? AND next_probe_at IS NOT NULL AND next_probe_at <= ?", models.QuotaRecoveryStateExhausted, now).Order("id ASC").Find(&accounts).Error; err != nil {
		return
	}
	for _, account := range accounts {
		attempt, token, claimed, err := s.claim(account.ID, now)
		if err != nil || !claimed {
			continue
		}
		_ = s.runAttempt(s.pollContext(), attempt, token)
	}
}

func (s *ProbeService) claim(accountID uint, now time.Time) (models.QuotaProbeAttempt, string, bool, error) {
	token := randomToken()
	leaseUntil := now.Add(s.leaseDuration())
	var attempt models.QuotaProbeAttempt
	claimed := false
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var account models.QuotaAccount
		if err := tx.First(&account, accountID).Error; err != nil {
			return err
		}
		if account.RecoveryState != models.QuotaRecoveryStateExhausted || account.NextProbeAt == nil || account.NextProbeAt.After(now) {
			return nil
		}
		if account.ProbeClaimToken != "" && account.ProbeClaimUntil != nil && account.ProbeClaimUntil.After(now) {
			return nil
		}
		accountLock := tx.Model(&models.QuotaAccount{}).Where("id = ?", account.ID).UpdateColumn("updated_at", gorm.Expr("updated_at"))
		if accountLock.Error != nil {
			return accountLock.Error
		}
		if accountLock.RowsAffected != 1 {
			return errors.New("quota account disappeared while claiming recovery probe")
		}
		if err := tx.First(&account, account.ID).Error; err != nil {
			return err
		}
		if err := quota.ReleaseHeldBatchesForAccount(tx, account.ID, now); err != nil {
			return err
		}
		if blocked, err := probeAccountHasActiveWork(tx, account.ID); err != nil {
			return err
		} else if blocked {
			return nil
		}

		var existing models.QuotaProbeAttempt
		result := tx.Where("quota_account_id = ? AND recovery_generation = ? AND state IN ?", account.ID, account.RecoveryGeneration, []string{models.ProbeAttemptStatePending, models.ProbeAttemptStateClaimed, models.ProbeAttemptStateRunning, models.ProbeAttemptStateUnknown}).Order("scheduled_slot ASC, id ASC").First(&existing)
		if result.Error != nil && !errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return result.Error
		}
		if result.Error == nil && existing.LeaseToken != "" && existing.LeaseUntil != nil && existing.LeaseUntil.After(now) {
			return nil
		}

		if result.Error == nil {
			update := tx.Model(&models.QuotaProbeAttempt{}).Where("id = ? AND recovery_generation = ? AND (lease_token = '' OR lease_until IS NULL OR lease_until <= ?)", existing.ID, account.RecoveryGeneration, now).Updates(map[string]interface{}{"state": models.ProbeAttemptStateClaimed, "lease_token": token, "lease_until": leaseUntil})
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return nil
			}
			attempt = existing
			attempt.State = models.ProbeAttemptStateClaimed
			attempt.LeaseToken = token
			attempt.LeaseUntil = &leaseUntil
		} else {
			configPath, remote, err := s.selectProbeTuple(tx, account.QuotaKey)
			if err != nil {
				return nil
			}
			slot, err := s.nextScheduledSlot(tx, account.ID, account.RecoveryGeneration, now)
			if err != nil {
				return err
			}
			attempt = models.QuotaProbeAttempt{
				QuotaAccountID:     account.ID,
				RecoveryGeneration: account.RecoveryGeneration,
				ScheduledSlot:      slot,
				AttemptKey:         models.QuotaProbeAttemptKey(account.ID, account.RecoveryGeneration, slot),
				ContractVersion:    models.ProbeContractVersion,
				Phase:              models.ProbePhaseClaimed,
				ObjectPath:         fmt.Sprintf(".rclone-manager-probe-%d-%d-%d-%s", account.ID, account.RecoveryGeneration, slot, randomToken()),
				ExpectedBytes:      models.ProbeExpectedBytes,
				QuotaKey:           account.QuotaKey,
				ConfigIdentity:     configPath,
				RemoteName:         remote,
				State:              models.ProbeAttemptStateClaimed,
				DueAt:              now,
				LeaseToken:         token,
				LeaseUntil:         &leaseUntil,
				VerificationState:  models.ProbeVerificationStatePending,
				CleanupState:       models.ProbeCleanupStatePending,
			}
			if err := tx.Create(&attempt).Error; err != nil {
				return err
			}
		}

		accountUpdate := tx.Model(&models.QuotaAccount{}).Where("id = ? AND recovery_state = ? AND recovery_generation = ? AND (probe_claim_token = '' OR probe_claim_until IS NULL OR probe_claim_until <= ?)", account.ID, models.QuotaRecoveryStateExhausted, account.RecoveryGeneration, now).Updates(map[string]interface{}{"probe_claim_token": token, "probe_claim_until": leaseUntil})
		if accountUpdate.Error != nil {
			return accountUpdate.Error
		}
		if accountUpdate.RowsAffected != 1 {
			return nil
		}
		claimed = true
		return nil
	})
	return attempt, token, claimed, err
}

func (s *ProbeService) selectProbeTuple(tx *gorm.DB, quotaKey string) (string, string, error) {
	var tasks []models.Task
	if err := tx.Where("enabled = ? AND task_type = ? AND rotation_strategy = ? AND rotation_stop_requested = ?", true, "rotation", "proactive_quota", false).Order("id ASC").Find(&tasks).Error; err != nil {
		return "", "", err
	}
	resolver := s.ConfigResolver
	if resolver == nil {
		resolver = (&quota.Service{DB: tx}).ResolveConfigPath
	}
	for _, task := range tasks {
		if task.Status == "stopped" || task.Status == "canceled" {
			continue
		}
		configPath, err := resolver(task.RcloneConfig)
		if err != nil || strings.TrimSpace(configPath) == "" {
			continue
		}
		keys, err := models.ParseRotationQuotaKeys(task.RotationQuotaKeys)
		if err != nil {
			continue
		}
		for _, remote := range models.ParseRotationRemotes(task.RotationRemotes) {
			key := strings.TrimSpace(keys[remote])
			if key == "" {
				key = models.DefaultRotationQuotaKey(configPath, remote)
			}
			if key != quotaKey {
				continue
			}
			return configPath, remote, nil
		}
	}
	return "", "", errors.New("no valid proactive task tuple for quota account")
}

func (s *ProbeService) nextScheduledSlot(tx *gorm.DB, accountID uint, generation int64, now time.Time) (int64, error) {
	interval := int64(models.DefaultQuotaRecoveryProbeDelay / time.Second)
	if interval <= 0 {
		interval = 1
	}
	slot := now.Unix() / interval
	var latest models.QuotaProbeAttempt
	if err := tx.Where("quota_account_id = ? AND recovery_generation = ?", accountID, generation).Order("scheduled_slot DESC").First(&latest).Error; err == nil && latest.ScheduledSlot >= slot {
		slot = latest.ScheduledSlot + 1
	} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, err
	}
	return slot, nil
}

func (s *ProbeService) runAttempt(ctx context.Context, attempt models.QuotaProbeAttempt, token string) error {
	heartbeat := s.startHeartbeat(attempt, token)
	defer heartbeat.stop()
	if err := heartbeat.err(); err != nil {
		return err
	}
	if attempt.Phase == "" {
		attempt.Phase = models.ProbePhaseClaimed
	}
	startedFromClaimed := attempt.Phase == models.ProbePhaseClaimed && attempt.ProcessID == 0 && strings.TrimSpace(attempt.ProcessStartToken) == ""
	if attempt.Phase == models.ProbePhaseClaimed {
		if startedFromClaimed {
			if err := s.setPhase(attempt, token, models.ProbePhaseUpload); err != nil {
				return err
			}
			attempt.Phase = models.ProbePhaseUpload
			attempt.ProcessID = 0
			attempt.ProcessStartToken = ""
		} else {
			if err := s.setPhase(attempt, token, models.ProbePhaseVerify); err != nil {
				return err
			}
			attempt.Phase = models.ProbePhaseVerify
			attempt.ProcessID = 0
			attempt.ProcessStartToken = ""
		}
	}
	if attempt.Phase == models.ProbePhaseUpload {
		if !startedFromClaimed && (attempt.ProcessID <= 0 || strings.TrimSpace(attempt.ProcessStartToken) == "") {
			if err := s.setPhase(attempt, token, models.ProbePhaseVerify); err != nil {
				return err
			}
			attempt.Phase = models.ProbePhaseVerify
		} else if !startedFromClaimed {
			if s.Inspector == nil {
				return s.markUnknown(attempt, token, errors.New("probe process ownership cannot be verified"))
			}
			status, err := s.Inspector.Inspect(attempt.ProcessID, attempt.ProcessStartToken)
			if err != nil || (status.Confirmed && status.Alive) {
				return s.markUnknown(attempt, token, errors.New("probe upload process ownership is unresolved"))
			}
			if !status.Confirmed {
				return s.markUnknown(attempt, token, errors.New("probe upload process identity is not confirmed"))
			}
			if err := s.setPhase(attempt, token, models.ProbePhaseVerify); err != nil {
				return err
			}
			attempt.Phase = models.ProbePhaseVerify
		}
	}
	if attempt.Phase == models.ProbePhaseUpload {
		if err := s.runUpload(ctx, attempt, token); err != nil {
			return err
		}
		attempt.Phase = models.ProbePhaseVerify
	}
	if attempt.Phase == models.ProbePhaseVerify {
		if err := s.runVerify(ctx, attempt, token); err != nil {
			return err
		}
		attempt.Phase = models.ProbePhaseCleanup
	}
	if attempt.Phase == models.ProbePhaseCleanup {
		return s.runCleanup(ctx, attempt, token)
	}
	return nil
}

func (s *ProbeService) runUpload(ctx context.Context, attempt models.QuotaProbeAttempt, token string) error {
	process, err := s.Runner.StartProbeUpload(ctx, ProbeUploadSpec{ConfigPath: attempt.ConfigIdentity, Remote: attempt.RemoteName, ObjectPath: attempt.ObjectPath, ExpectedBytes: attempt.ExpectedBytes})
	if err != nil {
		var started *StartedProcessIdentityError
		if errors.As(err, &started) && started.Result.PID > 0 {
			_ = s.persistAmbiguousProcess(attempt, token, started.Result)
			return s.markUnknown(attempt, token, errors.New("probe upload started but process identity was unavailable"))
		}
		return s.failAttempt(attempt, token, err, "upload start failed")
	}
	if err := s.persistProcess(attempt, token, process); err != nil {
		stopErr := process.Stop()
		result, waitErr := process.Wait()
		if result.PID > 0 && (waitErr == nil || stopErr == nil) {
			_ = s.persistAmbiguousProcess(attempt, token, result)
			return s.markUnknown(attempt, token, errors.New("probe upload process persistence failed"))
		}
		return s.markUnknown(attempt, token, fmt.Errorf("probe upload process persistence failed: %w", err))
	}
	result, waitErr := process.Wait()
	if waitErr != nil || result.ExitCode != 0 {
		return s.setPhaseWithProcessResult(attempt, token, models.ProbePhaseVerify, result)
	}
	return s.setPhaseWithProcessResult(attempt, token, models.ProbePhaseVerify, result)
}

func (s *ProbeService) runVerify(ctx context.Context, attempt models.QuotaProbeAttempt, token string) error {
	if err := s.setPhase(attempt, token, models.ProbePhaseVerify); err != nil {
		return err
	}
	result, err := s.Runner.VerifyProbeObject(ctx, attempt.ConfigIdentity, attempt.RemoteName, attempt.ObjectPath, attempt.ExpectedBytes)
	if err != nil {
		return s.retryVerification(attempt, token, err, result.Evidence)
	}
	if !result.Exact {
		return s.persistVerificationFailure(attempt, token, errors.New("probe object was not an exact single expected object"), probeObjectEvidence(result))
	}
	verifiedAt := s.now()
	if err := s.persistVerification(attempt, token, verifiedAt, probeObjectEvidence(result)); err != nil {
		return err
	}
	return nil
}

func (s *ProbeService) runCleanup(ctx context.Context, attempt models.QuotaProbeAttempt, token string) error {
	if err := s.setPhase(attempt, token, models.ProbePhaseCleanup); err != nil {
		return err
	}
	result, err := s.Runner.DeleteProbeObject(ctx, attempt.ConfigIdentity, attempt.RemoteName, attempt.ObjectPath)
	if err != nil || !result.Absent {
		if err == nil {
			err = errors.New("probe object absence was not confirmed")
		}
		return s.retryCleanup(attempt, token, err, result.Evidence)
	}
	var current models.QuotaProbeAttempt
	if err := s.DB.Where("id = ? AND lease_token = ?", attempt.ID, token).First(&current).Error; err != nil {
		return s.retryCleanup(attempt, token, fmt.Errorf("load probe attempt after cleanup: %w", err), result.Evidence)
	}
	if current.VerificationState == models.ProbeVerificationStateFailed {
		return s.finishFailureAfterCleanup(attempt, token, result.Evidence)
	}
	if current.VerificationState != models.ProbeVerificationStateSucceeded || current.VerifiedAt == nil {
		return s.retryCleanup(attempt, token, errors.New("probe verification was not durably successful before cleanup completion"), result.Evidence)
	}
	return s.finishSuccess(attempt, token, result.Evidence)
}

func (s *ProbeService) setPhase(attempt models.QuotaProbeAttempt, token, phase string) error {
	result := s.DB.Model(&models.QuotaProbeAttempt{}).Where("id = ? AND lease_token = ? AND state IN ?", attempt.ID, token, []string{models.ProbeAttemptStateClaimed, models.ProbeAttemptStateRunning, models.ProbeAttemptStateUnknown}).Updates(map[string]interface{}{"phase": phase, "state": models.ProbeAttemptStateRunning, "started_at": gorm.Expr("COALESCE(started_at, ?)", s.now()), "process_id": 0, "process_start_token": ""})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrProbeLeaseLost
	}
	return nil
}

func (s *ProbeService) setPhaseWithProcessResult(attempt models.QuotaProbeAttempt, token, phase string, process ProcessResult) error {
	result := s.DB.Model(&models.QuotaProbeAttempt{}).Where("id = ? AND lease_token = ?", attempt.ID, token).Updates(map[string]interface{}{"phase": phase, "state": models.ProbeAttemptStateClaimed, "exit_code": process.ExitCode, "process_id": 0, "process_start_token": ""})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrProbeLeaseLost
	}
	return nil
}

func (s *ProbeService) persistProcess(attempt models.QuotaProbeAttempt, token string, process ProcessHandle) error {
	if process == nil || process.PID() <= 0 || process.StartToken() == "" {
		return errors.New("probe process identity is required")
	}
	result := s.DB.Model(&models.QuotaProbeAttempt{}).Where("id = ? AND lease_token = ? AND phase = ?", attempt.ID, token, models.ProbePhaseUpload).Updates(map[string]interface{}{"state": models.ProbeAttemptStateRunning, "process_id": process.PID(), "process_start_token": process.StartToken()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrProbeLeaseLost
	}
	return nil
}

func (s *ProbeService) persistAmbiguousProcess(attempt models.QuotaProbeAttempt, token string, result ProcessResult) error {
	return s.DB.Model(&models.QuotaProbeAttempt{}).Where("id = ? AND lease_token = ?", attempt.ID, token).Updates(map[string]interface{}{"state": models.ProbeAttemptStateUnknown, "process_id": result.PID, "process_start_token": result.ProcessStartToken, "exit_code": result.ExitCode}).Error
}

func (s *ProbeService) persistVerification(attempt models.QuotaProbeAttempt, token string, verifiedAt time.Time, evidence string) error {
	result := s.DB.Model(&models.QuotaProbeAttempt{}).Where("id = ? AND lease_token = ?", attempt.ID, token).Updates(map[string]interface{}{"phase": models.ProbePhaseCleanup, "state": models.ProbeAttemptStateRunning, "verification_state": models.ProbeVerificationStateSucceeded, "verification_evidence": redactProbeEvidence(evidence, attempt), "verified_at": verifiedAt, "process_id": 0, "process_start_token": ""})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrProbeLeaseLost
	}
	return nil
}

func (s *ProbeService) persistVerificationFailure(attempt models.QuotaProbeAttempt, token string, err error, evidence string) error {
	result := s.DB.Model(&models.QuotaProbeAttempt{}).Where("id = ? AND lease_token = ?", attempt.ID, token).Updates(map[string]interface{}{
		"phase":                 models.ProbePhaseCleanup,
		"state":                 models.ProbeAttemptStateRunning,
		"verification_state":    models.ProbeVerificationStateFailed,
		"verification_evidence": redactProbeEvidence(evidence, attempt),
		"cleanup_state":         models.ProbeCleanupStatePending,
		"last_error":            redactProbeEvidence(err.Error(), attempt),
		"process_id":            0,
		"process_start_token":   "",
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrProbeLeaseLost
	}
	return nil
}

func (s *ProbeService) retryVerification(attempt models.QuotaProbeAttempt, token string, err error, evidence string) error {
	now := s.now()
	updateErr := s.DB.Transaction(func(tx *gorm.DB) error {
		update := tx.Model(&models.QuotaProbeAttempt{}).Where("id = ? AND lease_token = ?", attempt.ID, token).Updates(map[string]interface{}{
			"state":               models.ProbeAttemptStateUnknown,
			"phase":               models.ProbePhaseVerify,
			"verification_state":  models.ProbeVerificationStateUnknown,
			"last_error":          redactProbeEvidence(err.Error(), attempt),
			"error_evidence":      redactProbeEvidence(evidence, attempt),
			"lease_token":         "",
			"lease_until":         nil,
			"process_id":          0,
			"process_start_token": "",
		})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrProbeLeaseLost
		}
		account := tx.Model(&models.QuotaAccount{}).Where("id = ? AND recovery_generation = ? AND probe_claim_token = ?", attempt.QuotaAccountID, attempt.RecoveryGeneration, token).Updates(map[string]interface{}{
			"next_probe_at":     now.Add(time.Minute),
			"probe_claim_token": "",
			"probe_claim_until": nil,
		})
		if account.Error != nil {
			return account.Error
		}
		if account.RowsAffected != 1 {
			return ErrProbeLeaseLost
		}
		return nil
	})
	if updateErr != nil {
		return updateErr
	}
	return err
}

func (s *ProbeService) failAttempt(attempt models.QuotaProbeAttempt, token string, err error, evidence string) error {
	now := s.now()
	return s.DB.Transaction(func(tx *gorm.DB) error {
		update := tx.Model(&models.QuotaProbeAttempt{}).Where("id = ? AND lease_token = ?", attempt.ID, token).Updates(map[string]interface{}{"state": models.ProbeAttemptStateFailed, "phase": models.ProbePhaseFinished, "finished_at": now, "last_error": redactProbeEvidence(err.Error(), attempt), "error_evidence": redactProbeEvidence(evidence, attempt), "lease_token": "", "lease_until": nil, "process_id": 0, "process_start_token": ""})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrProbeLeaseLost
		}
		account := tx.Model(&models.QuotaAccount{}).Where("id = ? AND recovery_generation = ? AND probe_claim_token = ?", attempt.QuotaAccountID, attempt.RecoveryGeneration, token).Updates(map[string]interface{}{"next_probe_at": now.Add(models.DefaultQuotaRecoveryProbeDelay), "probe_claim_token": "", "probe_claim_until": nil})
		if account.Error != nil {
			return account.Error
		}
		if account.RowsAffected != 1 {
			return ErrProbeLeaseLost
		}
		return nil
	})
}

func (s *ProbeService) retryCleanup(attempt models.QuotaProbeAttempt, token string, err error, evidence string) error {
	now := s.now()
	return s.DB.Transaction(func(tx *gorm.DB) error {
		update := tx.Model(&models.QuotaProbeAttempt{}).Where("id = ? AND lease_token = ?", attempt.ID, token).Updates(map[string]interface{}{"state": models.ProbeAttemptStateUnknown, "phase": models.ProbePhaseCleanup, "cleanup_state": models.ProbeCleanupStateUnknown, "last_error": redactProbeEvidence(err.Error(), attempt), "error_evidence": redactProbeEvidence(evidence, attempt), "lease_token": "", "lease_until": nil, "process_id": 0, "process_start_token": ""})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrProbeLeaseLost
		}
		account := tx.Model(&models.QuotaAccount{}).Where("id = ? AND recovery_generation = ? AND probe_claim_token = ?", attempt.QuotaAccountID, attempt.RecoveryGeneration, token).Updates(map[string]interface{}{"next_probe_at": now.Add(models.DefaultQuotaRecoveryProbeDelay), "probe_claim_token": "", "probe_claim_until": nil})
		if account.Error != nil {
			return account.Error
		}
		if account.RowsAffected != 1 {
			return ErrProbeLeaseLost
		}
		return nil
	})
}

func (s *ProbeService) finishFailureAfterCleanup(attempt models.QuotaProbeAttempt, token, evidence string) error {
	now := s.now()
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var current models.QuotaProbeAttempt
		if err := tx.Where("id = ? AND lease_token = ? AND phase = ? AND state IN ? AND verification_state = ? AND cleanup_state IN ? AND process_id = 0", attempt.ID, token, models.ProbePhaseCleanup, []string{models.ProbeAttemptStateClaimed, models.ProbeAttemptStateRunning, models.ProbeAttemptStateUnknown}, models.ProbeVerificationStateFailed, []string{models.ProbeCleanupStatePending, models.ProbeCleanupStateUnknown}).First(&current).Error; err != nil {
			return err
		}
		update := tx.Model(&models.QuotaProbeAttempt{}).Where("id = ? AND lease_token = ? AND phase = ? AND state IN ? AND verification_state = ? AND cleanup_state IN ? AND process_id = 0", current.ID, token, models.ProbePhaseCleanup, []string{models.ProbeAttemptStateClaimed, models.ProbeAttemptStateRunning, models.ProbeAttemptStateUnknown}, models.ProbeVerificationStateFailed, []string{models.ProbeCleanupStatePending, models.ProbeCleanupStateUnknown}).Updates(map[string]interface{}{
			"state":               models.ProbeAttemptStateFailed,
			"phase":               models.ProbePhaseFinished,
			"finished_at":         now,
			"cleanup_state":       models.ProbeCleanupStateSucceeded,
			"cleanup_evidence":    redactProbeEvidence(evidence, current),
			"cleaned_at":          now,
			"lease_token":         "",
			"lease_until":         nil,
			"process_id":          0,
			"process_start_token": "",
		})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrProbeLeaseLost
		}
		account := tx.Model(&models.QuotaAccount{}).Where("id = ? AND recovery_generation = ? AND probe_claim_token = ?", current.QuotaAccountID, current.RecoveryGeneration, token).Updates(map[string]interface{}{
			"next_probe_at":     now.Add(models.DefaultQuotaRecoveryProbeDelay),
			"probe_claim_token": "",
			"probe_claim_until": nil,
		})
		if account.Error != nil {
			return account.Error
		}
		if account.RowsAffected != 1 {
			return ErrProbeLeaseLost
		}
		return nil
	})
}

func (s *ProbeService) markUnknown(attempt models.QuotaProbeAttempt, token string, err error) error {
	now := s.now()
	return s.DB.Transaction(func(tx *gorm.DB) error {
		update := tx.Model(&models.QuotaProbeAttempt{}).Where("id = ? AND lease_token = ?", attempt.ID, token).Updates(map[string]interface{}{"state": models.ProbeAttemptStateUnknown, "last_error": redactProbeEvidence(err.Error(), attempt), "lease_token": "", "lease_until": nil})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrProbeLeaseLost
		}
		return tx.Model(&models.QuotaAccount{}).Where("id = ? AND recovery_generation = ? AND probe_claim_token = ?", attempt.QuotaAccountID, attempt.RecoveryGeneration, token).Updates(map[string]interface{}{"next_probe_at": now.Add(time.Minute), "probe_claim_token": "", "probe_claim_until": nil}).Error
	})
}

func (s *ProbeService) finishSuccess(attempt models.QuotaProbeAttempt, token, evidence string) error {
	now := s.now()
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var current models.QuotaProbeAttempt
		if err := tx.Where("id = ? AND lease_token = ? AND phase = ? AND state IN ? AND verification_state = ? AND verified_at IS NOT NULL AND cleanup_state IN ? AND process_id = 0", attempt.ID, token, models.ProbePhaseCleanup, []string{models.ProbeAttemptStateClaimed, models.ProbeAttemptStateRunning, models.ProbeAttemptStateUnknown}, models.ProbeVerificationStateSucceeded, []string{models.ProbeCleanupStatePending, models.ProbeCleanupStateUnknown}).First(&current).Error; err != nil {
			return err
		}
		verifiedAt := *current.VerifiedAt
		var account models.QuotaAccount
		if err := tx.First(&account, current.QuotaAccountID).Error; err != nil {
			return err
		}
		if account.RecoveryState != models.QuotaRecoveryStateExhausted || account.RecoveryGeneration != current.RecoveryGeneration || account.ProbeClaimToken != token {
			return ErrProbeLeaseLost
		}
		if blocked, err := probeAccountHasActiveWork(tx, account.ID); err != nil {
			return err
		} else if blocked {
			return ErrProbeActiveWork
		}
		if err := tx.Model(&models.QuotaReservation{}).Where("quota_account_id = ? AND state = ? AND (expires_at IS NULL OR expires_at > ?)", account.ID, models.ReservationStateCommitted, verifiedAt).Update("expires_at", verifiedAt).Error; err != nil {
			return err
		}
		usage, err := quota.EffectiveAccountUsage(tx, account.ID, verifiedAt)
		if err != nil {
			return err
		}
		if usage != 0 {
			return fmt.Errorf("quota probe reset left effective account usage at %d", usage)
		}
		attemptUpdate := tx.Model(&models.QuotaProbeAttempt{}).Where("id = ? AND lease_token = ? AND phase = ? AND state IN ? AND verification_state = ? AND verified_at IS NOT NULL AND cleanup_state IN ? AND process_id = 0", current.ID, token, models.ProbePhaseCleanup, []string{models.ProbeAttemptStateClaimed, models.ProbeAttemptStateRunning, models.ProbeAttemptStateUnknown}, models.ProbeVerificationStateSucceeded, []string{models.ProbeCleanupStatePending, models.ProbeCleanupStateUnknown}).Updates(map[string]interface{}{"state": models.ProbeAttemptStateSucceeded, "phase": models.ProbePhaseFinished, "finished_at": now, "cleanup_state": models.ProbeCleanupStateSucceeded, "cleanup_evidence": redactProbeEvidence(evidence, current), "cleaned_at": now, "lease_token": "", "lease_until": nil, "process_id": 0, "process_start_token": ""})
		if attemptUpdate.Error != nil {
			return attemptUpdate.Error
		}
		if attemptUpdate.RowsAffected != 1 {
			return ErrProbeLeaseLost
		}
		accountUpdate := tx.Model(&models.QuotaAccount{}).Where("id = ? AND recovery_state = ? AND recovery_generation = ? AND probe_claim_token = ?", account.ID, models.QuotaRecoveryStateExhausted, current.RecoveryGeneration, token).Updates(map[string]interface{}{"budget_bytes": models.DefaultRotationQuotaLimitBytes, "window_started_at": verifiedAt, "recovery_state": models.QuotaRecoveryStateAvailable, "provider_blocked_until": nil, "first_exhausted_at": nil, "next_probe_at": nil, "probe_claim_token": "", "probe_claim_until": nil})
		if accountUpdate.Error != nil {
			return accountUpdate.Error
		}
		if accountUpdate.RowsAffected != 1 {
			return ErrProbeLeaseLost
		}
		if err := tx.Model(&models.QuotaProbeAttempt{}).Where("quota_account_id = ? AND recovery_generation = ? AND id <> ? AND state IN ?", account.ID, current.RecoveryGeneration, current.ID, []string{models.ProbeAttemptStatePending, models.ProbeAttemptStateClaimed, models.ProbeAttemptStateRunning, models.ProbeAttemptStateUnknown}).Updates(map[string]interface{}{"state": models.ProbeAttemptStateCanceled, "phase": models.ProbePhaseFinished, "finished_at": now, "lease_token": "", "lease_until": nil, "last_error": "superseded by successful recovery probe"}).Error; err != nil {
			return err
		}
		return wakeAllProactiveTasks(tx, now)
	})
}

func probeAccountHasActiveWork(tx *gorm.DB, accountID uint) (bool, error) {
	var batches int64
	if err := tx.Model(&models.RotationQuotaBatch{}).Where("quota_account_id = ? AND state IN ?", accountID, []string{models.BatchStateReserved, models.BatchStatePlanned, models.BatchStateRunning, models.BatchStateReconciling, models.BatchStateUnknown}).Count(&batches).Error; err != nil {
		return false, err
	}
	if batches > 0 {
		return true, nil
	}
	var reservations int64
	if err := tx.Model(&models.QuotaReservation{}).Where("quota_account_id = ? AND state IN ?", accountID, []string{models.ReservationStateHeld, models.ReservationStateActive, models.ReservationStateUnknown}).Count(&reservations).Error; err != nil {
		return false, err
	}
	return reservations > 0, nil
}

func wakeAllProactiveTasks(tx *gorm.DB, now time.Time) error {
	return tx.Model(&models.Task{}).Where("enabled = ? AND task_type = ? AND rotation_strategy = ? AND rotation_stop_requested = ? AND status NOT IN ?", true, "rotation", "proactive_quota", false, []string{"stopped", "canceled"}).Updates(map[string]interface{}{"rotation_rescan_pending": true, "rotation_rescan_generation": gorm.Expr("rotation_rescan_generation + 1"), "rotation_quota_wake_at": now}).Error
}

func (s *ProbeService) startHeartbeat(attempt models.QuotaProbeAttempt, token string) *probeHeartbeat {
	interval := s.HeartbeatEvery
	if interval <= 0 {
		interval = s.leaseDuration() / 3
	}
	if interval <= 0 {
		interval = time.Minute
	}
	h := &probeHeartbeat{stopCh: make(chan struct{}), doneCh: make(chan struct{}), errCh: make(chan error, 1)}
	go func() {
		defer close(h.doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := s.renewHeartbeat(attempt, token); err != nil {
					select {
					case h.errCh <- err:
					default:
					}
					return
				}
			case <-h.stopCh:
				return
			}
		}
	}()
	return h
}

func (s *ProbeService) renewHeartbeat(attempt models.QuotaProbeAttempt, token string) error {
	until := s.now().Add(s.leaseDuration())
	result := s.DB.Model(&models.QuotaProbeAttempt{}).Where("id = ? AND lease_token = ? AND state IN ?", attempt.ID, token, []string{models.ProbeAttemptStateClaimed, models.ProbeAttemptStateRunning, models.ProbeAttemptStateUnknown}).Updates(map[string]interface{}{"lease_until": until})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrProbeLeaseLost
	}
	result = s.DB.Model(&models.QuotaAccount{}).Where("id = ? AND recovery_generation = ? AND probe_claim_token = ?", attempt.QuotaAccountID, attempt.RecoveryGeneration, token).Update("probe_claim_until", until)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrProbeLeaseLost
	}
	return nil
}

func (s *ProbeService) leaseDuration() time.Duration {
	if s.LeaseDuration > 0 {
		return s.LeaseDuration
	}
	return models.DefaultQuotaRecoveryClaimLease
}

func (s *ProbeService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *ProbeService) pollContext() context.Context {
	if s != nil && s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

type probeHeartbeat struct {
	stopCh chan struct{}
	doneCh chan struct{}
	errCh  chan error
	once   sync.Once
}

func (h *probeHeartbeat) stop() {
	if h == nil {
		return
	}
	h.once.Do(func() { close(h.stopCh); <-h.doneCh })
}

func (h *probeHeartbeat) err() error {
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

func redactProbeEvidence(value string, attempt models.QuotaProbeAttempt) string {
	for _, secret := range []string{attempt.ConfigIdentity, attempt.RemoteName, attempt.ObjectPath} {
		if strings.TrimSpace(secret) != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	if len(value) > 2048 {
		value = value[:2048]
	}
	return value
}

func stringEvidence(result ProcessResult) string {
	return result.Stdout + "\n" + result.Stderr
}

func (s *ProbeService) sortedDueAccounts(now time.Time) []models.QuotaAccount {
	var accounts []models.QuotaAccount
	if s.DB != nil {
		_ = s.DB.Where("recovery_state = ? AND next_probe_at IS NOT NULL AND next_probe_at <= ?", models.QuotaRecoveryStateExhausted, now).Find(&accounts).Error
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	return accounts
}
