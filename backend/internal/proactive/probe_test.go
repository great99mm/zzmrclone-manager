package proactive

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
)

type fakeProbeProcess struct {
	result  ProcessResult
	waitErr error
}

func (p *fakeProbeProcess) Wait() (ProcessResult, error) { return p.result, p.waitErr }
func (p *fakeProbeProcess) Stop() error                  { return nil }
func (p *fakeProbeProcess) PID() int                     { return p.result.PID }
func (p *fakeProbeProcess) StartToken() string           { return p.result.ProcessStartToken }

type fakeProbeRunner struct {
	mu           sync.Mutex
	uploads      int
	verifies     int
	deletes      int
	uploadExit   int
	verifyExact  bool
	deleteGone   bool
	verifyErrors int
	verifyErr    error
}

func (r *fakeProbeRunner) StartProbeUpload(context.Context, ProbeUploadSpec) (ProcessHandle, error) {
	r.mu.Lock()
	r.uploads++
	exit := r.uploadExit
	r.mu.Unlock()
	return &fakeProbeProcess{result: ProcessResult{PID: 100 + exit, ProcessStartToken: fmt.Sprintf("probe:%d", exit), ExitCode: exit}}, nil
}

func (r *fakeProbeRunner) VerifyProbeObject(context.Context, string, string, string, int64) (ProbeObjectResult, error) {
	r.mu.Lock()
	r.verifies++
	exact := r.verifyExact
	if r.verifyErrors > 0 {
		r.verifyErrors--
		err := r.verifyErr
		r.mu.Unlock()
		if err == nil {
			err = errors.New("temporary stat failure")
		}
		return ProbeObjectResult{Evidence: err.Error()}, err
	}
	r.mu.Unlock()
	return ProbeObjectResult{Exists: exact, Exact: exact, ObjectCount: 1, Evidence: "exact probe object"}, nil
}

func (r *fakeProbeRunner) DeleteProbeObject(context.Context, string, string, string) (ProbeCleanupResult, error) {
	r.mu.Lock()
	r.deletes++
	gone := r.deleteGone
	r.mu.Unlock()
	return ProbeCleanupResult{Absent: gone, Evidence: "probe object deleted"}, nil
}

func (r *fakeProbeRunner) counts() (int, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.uploads, r.verifies, r.deletes
}

func newProbeDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "probe.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(2)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.Task{}, &models.QuotaAccount{}, &models.QuotaProbeAttempt{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &models.QuotaReservation{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func createProbeAccount(t *testing.T, db *gorm.DB, key string, now time.Time) models.QuotaAccount {
	t.Helper()
	next := now
	account := models.QuotaAccount{QuotaKey: key, RemoteName: "remote", ConfigIdentity: "/tmp/rclone.conf", BudgetBytes: models.DefaultRotationQuotaLimitBytes, WindowSeconds: 86400, Enabled: true, RecoveryState: models.QuotaRecoveryStateExhausted, RecoveryGeneration: 4, NextProbeAt: &next}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	return account
}

func createProbeTask(t *testing.T, db *gorm.DB, id uint, key string) models.Task {
	t.Helper()
	task := models.Task{ID: id, Name: fmt.Sprintf("probe-task-%d", id), Enabled: true, Status: "idle", TaskType: "rotation", RotationStrategy: "proactive_quota", RotationRemotes: `["remote"]`, RotationQuotaKeys: fmt.Sprintf(`{"remote":%q}`, key)}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	return task
}

func probeService(db *gorm.DB, runner *fakeProbeRunner, clock *time.Time) *ProbeService {
	return &ProbeService{DB: db, Runner: runner, Now: func() time.Time { return *clock }, ConfigResolver: func(string) (string, error) { return "/tmp/rclone.conf", nil }}
}

func TestProbeClaimConcurrencyCreatesOneAttempt(t *testing.T) {
	db := newProbeDB(t)
	now := time.Unix(100, 0)
	account := createProbeAccount(t, db, "claim-key", now)
	createProbeTask(t, db, 1, account.QuotaKey)
	s1 := probeService(db, &fakeProbeRunner{}, &now)
	s2 := probeService(db, &fakeProbeRunner{}, &now)
	type result struct {
		claimed bool
		err     error
	}
	results := make(chan result, 2)
	go func() { _, _, claimed, err := s1.claim(account.ID, now); results <- result{claimed: claimed, err: err} }()
	go func() { _, _, claimed, err := s2.claim(account.ID, now); results <- result{claimed: claimed, err: err} }()
	claimed := 0
	for i := 0; i < 2; i++ {
		value := <-results
		if value.err != nil && !strings.Contains(value.err.Error(), "locked") {
			t.Fatal(value.err)
		}
		if value.claimed {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("claimed=%d, want exactly one", claimed)
	}
	var count int64
	if err := db.Model(&models.QuotaProbeAttempt{}).Where("quota_account_id = ?", account.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("attempt count=%d, want one", count)
	}
}

func TestProbeClaimRejectsActiveBatch(t *testing.T) {
	db := newProbeDB(t)
	now := time.Unix(100, 0)
	account := createProbeAccount(t, db, "blocked-key", now)
	createProbeTask(t, db, 1, account.QuotaKey)
	if err := db.Create(&models.RotationQuotaBatch{TaskID: 1, QuotaAccountID: account.ID, State: models.BatchStateRunning, RequestKey: "active", OwnerToken: testOwner}).Error; err != nil {
		t.Fatal(err)
	}
	service := probeService(db, &fakeProbeRunner{}, &now)
	_, _, claimed, err := service.claim(account.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("probe claimed while active batch existed")
	}
}

func TestProbeReleasesSafeHeldWorkAcrossTasksBeforeClaim(t *testing.T) {
	db := newProbeDB(t)
	now := time.Unix(100, 0)
	account := createProbeAccount(t, db, "shared-held-key", now)
	createProbeTask(t, db, 1, account.QuotaKey)
	expires := now.Add(time.Hour)
	batch := models.RotationQuotaBatch{TaskID: 2, QuotaAccountID: account.ID, State: models.BatchStateReserved, RequestKey: "dormant-other-task", OwnerToken: strings.Repeat("e", 48), DestinationRemote: "remote"}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	file := models.RotationQuotaBatchFile{BatchID: batch.ID, RelativePath: "dormant", SnapshotKey: "dormant", State: models.BatchFileStateHeld}
	if err := db.Create(&file).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.QuotaReservation{QuotaAccountID: account.ID, BatchID: batch.ID, BatchFileID: file.ID, Bytes: 1, State: models.ReservationStateHeld, ExpiresAt: &expires, IdempotencyKey: "dormant-other-task"}).Error; err != nil {
		t.Fatal(err)
	}
	runner := &fakeProbeRunner{verifyExact: true, deleteGone: true}
	service := probeService(db, runner, &now)
	service.Poll(now)
	var storedBatch models.RotationQuotaBatch
	if err := db.First(&storedBatch, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedBatch.State != models.BatchStateCanceled {
		t.Fatalf("safe held batch state=%s", storedBatch.State)
	}
	var storedFile models.RotationQuotaBatchFile
	if err := db.First(&storedFile, file.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedFile.State != models.BatchFileStateFailed {
		t.Fatalf("safe held file state=%s", storedFile.State)
	}
	var reservation models.QuotaReservation
	if err := db.First(&reservation, "batch_id = ?", batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reservation.State != models.ReservationStateReleased || reservation.ReleasedAt == nil {
		t.Fatalf("safe held reservation=%#v", reservation)
	}
	var storedAccount models.QuotaAccount
	if err := db.First(&storedAccount, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedAccount.RecoveryState != models.QuotaRecoveryStateAvailable {
		t.Fatalf("probe did not proceed after safe release: %#v", storedAccount)
	}
}

func TestProbeKeepsUnknownAccountWorkBlocked(t *testing.T) {
	for _, state := range []string{models.BatchStateRunning, models.BatchStateUnknown} {
		t.Run(state, func(t *testing.T) {
			db := newProbeDB(t)
			now := time.Unix(100, 0)
			account := createProbeAccount(t, db, "blocked-"+state, now)
			createProbeTask(t, db, 1, account.QuotaKey)
			batch := models.RotationQuotaBatch{TaskID: 2, QuotaAccountID: account.ID, State: state, RequestKey: "blocked-" + state, OwnerToken: strings.Repeat("f", 48), DestinationRemote: "remote"}
			if err := db.Create(&batch).Error; err != nil {
				t.Fatal(err)
			}
			service := probeService(db, &fakeProbeRunner{}, &now)
			_, _, claimed, err := service.claim(account.ID, now)
			if err != nil {
				t.Fatal(err)
			}
			if claimed {
				t.Fatal("probe claimed with active or unknown account work")
			}
			var stored models.RotationQuotaBatch
			if err := db.First(&stored, batch.ID).Error; err != nil {
				t.Fatal(err)
			}
			if stored.State != state {
				t.Fatalf("blocked batch changed from %s to %s", state, stored.State)
			}
		})
	}
}

func TestProbeSuccessResetsLedgerAndWakesTasks(t *testing.T) {
	db := newProbeDB(t)
	now := time.Unix(100, 0)
	account := createProbeAccount(t, db, "success-key", now)
	if err := db.Model(&models.QuotaAccount{}).Where("id = ?", account.ID).Update("budget_bytes", 1).Error; err != nil {
		t.Fatal(err)
	}
	createProbeTask(t, db, 1, account.QuotaKey)
	createProbeTask(t, db, 2, account.QuotaKey)
	expires := now.Add(time.Hour)
	if err := db.Create(&models.QuotaReservation{QuotaAccountID: account.ID, Bytes: 123, State: models.ReservationStateCommitted, ExpiresAt: &expires, IdempotencyKey: "old-usage"}).Error; err != nil {
		t.Fatal(err)
	}
	runner := &fakeProbeRunner{verifyExact: true, deleteGone: true}
	service := probeService(db, runner, &now)
	service.Poll(now)
	uploads, verifies, deletes := runner.counts()
	if uploads != 1 || verifies != 1 || deletes != 1 {
		t.Fatalf("probe calls upload=%d verify=%d delete=%d", uploads, verifies, deletes)
	}
	var stored models.QuotaAccount
	if err := db.First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RecoveryState != models.QuotaRecoveryStateAvailable || stored.NextProbeAt != nil || stored.ProviderBlockedUntil != nil || stored.BudgetBytes != models.DefaultRotationQuotaLimitBytes || stored.WindowStartedAt == nil || !stored.WindowStartedAt.Equal(now) {
		t.Fatalf("account was not restored: %#v", stored)
	}
	var reservation models.QuotaReservation
	if err := db.Where("idempotency_key = ?", "old-usage").First(&reservation).Error; err != nil {
		t.Fatal(err)
	}
	if reservation.ExpiresAt == nil || !reservation.ExpiresAt.Equal(now) || reservation.Bytes != 123 || reservation.State != models.ReservationStateCommitted {
		t.Fatalf("ledger reset changed reservation history: %#v", reservation)
	}
	var tasks []models.Task
	if err := db.Order("id").Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if !task.RotationRescanPending || task.RotationRescanGeneration != 1 || task.RotationQuotaWakeAt == nil || !task.RotationQuotaWakeAt.Equal(now) {
			t.Fatalf("task %d was not durably woken: %#v", task.ID, task)
		}
	}
}

func TestProbeFinalSuccessFailsClosedWhenAttemptCannotBeLoaded(t *testing.T) {
	db := newProbeDB(t)
	now := time.Unix(100, 0)
	account := createProbeAccount(t, db, "missing-final-attempt", now)
	token := strings.Repeat("c", 48)
	if err := db.Model(&models.QuotaAccount{}).Where("id = ?", account.ID).Update("probe_claim_token", token).Error; err != nil {
		t.Fatal(err)
	}
	err := (&ProbeService{DB: db, Now: func() time.Time { return now }}).finishSuccess(models.QuotaProbeAttempt{ID: 999, QuotaAccountID: account.ID, RecoveryGeneration: account.RecoveryGeneration}, token, "untrusted evidence")
	if err == nil {
		t.Fatal("final success ignored a missing attempt")
	}
	var stored models.QuotaAccount
	if err := db.First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RecoveryState != models.QuotaRecoveryStateExhausted || stored.ProbeClaimToken != token {
		t.Fatalf("missing attempt reset account: %#v", stored)
	}
}

func TestProbeCleanupPendingOrUnknownVerificationNeverUnblocks(t *testing.T) {
	for _, verificationState := range []string{models.ProbeVerificationStatePending, models.ProbeVerificationStateUnknown} {
		t.Run(verificationState, func(t *testing.T) {
			db := newProbeDB(t)
			now := time.Unix(100, 0)
			account := createProbeAccount(t, db, "cleanup-"+verificationState, now)
			createProbeTask(t, db, 1, account.QuotaKey)
			token := strings.Repeat("d", 48)
			leaseUntil := now.Add(time.Hour)
			attempt := models.QuotaProbeAttempt{
				QuotaAccountID: account.ID, RecoveryGeneration: account.RecoveryGeneration, ScheduledSlot: 1,
				AttemptKey: models.QuotaProbeAttemptKey(account.ID, account.RecoveryGeneration, 1), ContractVersion: models.ProbeContractVersion,
				Phase: models.ProbePhaseCleanup, ObjectPath: ".cleanup-" + verificationState, ExpectedBytes: models.ProbeExpectedBytes,
				QuotaKey: account.QuotaKey, ConfigIdentity: "/tmp/rclone.conf", RemoteName: "remote", State: models.ProbeAttemptStateRunning,
				DueAt: now, LeaseToken: token, LeaseUntil: &leaseUntil, VerificationState: verificationState, CleanupState: models.ProbeCleanupStatePending,
			}
			if err := db.Create(&attempt).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&models.QuotaAccount{}).Where("id = ?", account.ID).Update("probe_claim_token", token).Error; err != nil {
				t.Fatal(err)
			}
			service := probeService(db, &fakeProbeRunner{deleteGone: true}, &now)
			if err := service.runCleanup(context.Background(), attempt, token); err != nil {
				t.Fatal(err)
			}
			var stored models.QuotaAccount
			if err := db.First(&stored, account.ID).Error; err != nil {
				t.Fatal(err)
			}
			if stored.RecoveryState != models.QuotaRecoveryStateExhausted || stored.NextProbeAt == nil || !stored.NextProbeAt.Equal(now.Add(models.DefaultQuotaRecoveryProbeDelay)) {
				t.Fatalf("verification %s reset account: %#v", verificationState, stored)
			}
		})
	}
}

func TestProbeNonzeroUploadUsesRemoteVerification(t *testing.T) {
	db := newProbeDB(t)
	now := time.Unix(100, 0)
	account := createProbeAccount(t, db, "nonzero-upload-key", now)
	createProbeTask(t, db, 1, account.QuotaKey)
	runner := &fakeProbeRunner{uploadExit: 1, verifyExact: true, deleteGone: true}
	clock := now
	service := probeService(db, runner, &clock)
	service.Poll(clock)
	var stored models.QuotaAccount
	if err := db.First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RecoveryState != models.QuotaRecoveryStateAvailable || stored.NextProbeAt != nil {
		t.Fatalf("remote evidence did not restore account: %#v", stored)
	}
	var first models.QuotaProbeAttempt
	if err := db.First(&first).Error; err != nil {
		t.Fatal(err)
	}
	if first.State != models.ProbeAttemptStateSucceeded || first.Phase != models.ProbePhaseFinished {
		t.Fatalf("nonzero upload attempt = %#v", first)
	}
	if uploads, verifies, deletes := runner.counts(); uploads != 1 || verifies != 1 || deletes != 1 {
		t.Fatalf("probe calls upload=%d verify=%d delete=%d", uploads, verifies, deletes)
	}
}

func TestProbeConfirmedMismatchCleansSameAttemptBeforeFailure(t *testing.T) {
	db := newProbeDB(t)
	now := time.Unix(100, 0)
	account := createProbeAccount(t, db, "mismatch-key", now)
	createProbeTask(t, db, 1, account.QuotaKey)
	runner := &fakeProbeRunner{verifyExact: false, deleteGone: true}
	service := probeService(db, runner, &now)
	service.Poll(now)
	var attempt models.QuotaProbeAttempt
	if err := db.First(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if attempt.State != models.ProbeAttemptStateFailed || attempt.Phase != models.ProbePhaseFinished || attempt.VerificationState != models.ProbeVerificationStateFailed || attempt.CleanupState != models.ProbeCleanupStateSucceeded {
		t.Fatalf("mismatch attempt = %#v", attempt)
	}
	if uploads, verifies, deletes := runner.counts(); uploads != 1 || verifies != 1 || deletes != 1 {
		t.Fatalf("probe calls upload=%d verify=%d delete=%d", uploads, verifies, deletes)
	}
	var stored models.QuotaAccount
	if err := db.First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RecoveryState != models.QuotaRecoveryStateExhausted || stored.NextProbeAt == nil || !stored.NextProbeAt.Equal(now.Add(models.DefaultQuotaRecoveryProbeDelay)) {
		t.Fatalf("mismatch did not retain exhaustion: %#v", stored)
	}
}

func TestProbeTransientVerificationRetriesWithoutSecondUpload(t *testing.T) {
	db := newProbeDB(t)
	now := time.Unix(100, 0)
	account := createProbeAccount(t, db, "transient-verify-key", now)
	createProbeTask(t, db, 1, account.QuotaKey)
	runner := &fakeProbeRunner{verifyExact: true, deleteGone: true, verifyErrors: 1}
	clock := now
	service := probeService(db, runner, &clock)
	service.Poll(clock)
	var pending models.QuotaProbeAttempt
	if err := db.First(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if pending.State != models.ProbeAttemptStateUnknown || pending.Phase != models.ProbePhaseVerify {
		t.Fatalf("transient verification state = %#v", pending)
	}
	var stored models.QuotaAccount
	if err := db.First(&stored, account.ID).Error; err != nil {
		t.Fatal(err)
	}
	clock = *stored.NextProbeAt
	service.Poll(clock)
	var finished models.QuotaProbeAttempt
	if err := db.First(&finished, pending.ID).Error; err != nil {
		t.Fatal(err)
	}
	if finished.State != models.ProbeAttemptStateSucceeded {
		t.Fatalf("retry attempt = %#v", finished)
	}
	if uploads, verifies, deletes := runner.counts(); uploads != 1 || verifies != 2 || deletes != 1 {
		t.Fatalf("probe calls upload=%d verify=%d delete=%d", uploads, verifies, deletes)
	}
}

func TestProbeVerifyAndCleanupRecoveryDoNotUploadAgain(t *testing.T) {
	for _, phase := range []string{models.ProbePhaseVerify, models.ProbePhaseCleanup} {
		t.Run(phase, func(t *testing.T) {
			db := newProbeDB(t)
			now := time.Unix(100, 0)
			account := createProbeAccount(t, db, "recovery-"+phase, now)
			createProbeTask(t, db, 1, account.QuotaKey)
			verifiedAt := now.Add(-time.Minute)
			attempt := models.QuotaProbeAttempt{QuotaAccountID: account.ID, RecoveryGeneration: account.RecoveryGeneration, ScheduledSlot: 1, AttemptKey: models.QuotaProbeAttemptKey(account.ID, account.RecoveryGeneration, 1), ContractVersion: models.ProbeContractVersion, Phase: phase, ObjectPath: ".recovery-object-" + phase, ExpectedBytes: models.ProbeExpectedBytes, QuotaKey: account.QuotaKey, ConfigIdentity: "/tmp/rclone.conf", RemoteName: "remote", State: models.ProbeAttemptStateUnknown, DueAt: now, VerificationState: models.ProbeVerificationStatePending, CleanupState: models.ProbeCleanupStatePending}
			if phase == models.ProbePhaseCleanup {
				attempt.VerificationState = models.ProbeVerificationStateSucceeded
				attempt.VerifiedAt = &verifiedAt
			}
			if err := db.Create(&attempt).Error; err != nil {
				t.Fatal(err)
			}
			runner := &fakeProbeRunner{verifyExact: true, deleteGone: true}
			service := probeService(db, runner, &now)
			service.Poll(now)
			uploads, verifies, deletes := runner.counts()
			if uploads != 0 || deletes != 1 || (phase == models.ProbePhaseVerify && verifies != 1) {
				t.Fatalf("phase %s calls upload=%d verify=%d delete=%d", phase, uploads, verifies, deletes)
			}
			var stored models.QuotaAccount
			if err := db.First(&stored, account.ID).Error; err != nil {
				t.Fatal(err)
			}
			if stored.RecoveryState != models.QuotaRecoveryStateAvailable {
				t.Fatalf("phase %s did not finish recovery: %#v", phase, stored)
			}
		})
	}
}

func TestProbeSelectsDefaultQuotaKeyAfterResolvingConfig(t *testing.T) {
	db := newProbeDB(t)
	now := time.Unix(100, 0)
	config := "/tmp/rclone.conf"
	account := createProbeAccount(t, db, models.DefaultRotationQuotaKey(config, "remote"), now)
	task := models.Task{ID: 1, Name: "default-probe-task", Enabled: true, Status: "idle", TaskType: "rotation", RotationStrategy: "proactive_quota", RcloneConfig: config, RotationRemotes: `["remote"]`, RotationQuotaKeys: `{}`}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	runner := &fakeProbeRunner{verifyExact: true, deleteGone: true}
	service := probeService(db, runner, &now)
	service.Poll(now)
	var attempt models.QuotaProbeAttempt
	if err := db.First(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if attempt.ConfigIdentity != config || attempt.RemoteName != "remote" || attempt.QuotaKey != account.QuotaKey {
		t.Fatalf("default tuple = %#v", attempt)
	}
}

func TestProbeUploadRecoveryWithMissingProcessTokenNeverUploadsAgain(t *testing.T) {
	db := newProbeDB(t)
	now := time.Unix(100, 0)
	account := createProbeAccount(t, db, "missing-token-key", now)
	createProbeTask(t, db, 1, account.QuotaKey)
	attempt := models.QuotaProbeAttempt{
		QuotaAccountID: account.ID, RecoveryGeneration: account.RecoveryGeneration, ScheduledSlot: 1,
		AttemptKey: models.QuotaProbeAttemptKey(account.ID, account.RecoveryGeneration, 1), ContractVersion: models.ProbeContractVersion,
		Phase: models.ProbePhaseUpload, ObjectPath: ".restart-object", ExpectedBytes: models.ProbeExpectedBytes,
		QuotaKey: account.QuotaKey, ConfigIdentity: "/tmp/rclone.conf", RemoteName: "remote", State: models.ProbeAttemptStateUnknown,
		DueAt: now, VerificationState: models.ProbeVerificationStatePending, CleanupState: models.ProbeCleanupStatePending,
		ProcessID: 9999,
	}
	if err := db.Create(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	runner := &fakeProbeRunner{verifyExact: true, deleteGone: true}
	service := probeService(db, runner, &now)
	service.Poll(now)
	if uploads, verifies, deletes := runner.counts(); uploads != 0 || verifies != 1 || deletes != 1 {
		t.Fatalf("probe calls upload=%d verify=%d delete=%d", uploads, verifies, deletes)
	}
}

type blockingProbeRunner struct {
	started  chan struct{}
	canceled chan struct{}
}

func (r *blockingProbeRunner) StartProbeUpload(ctx context.Context, _ ProbeUploadSpec) (ProcessHandle, error) {
	close(r.started)
	<-ctx.Done()
	close(r.canceled)
	return nil, ctx.Err()
}

func (r *blockingProbeRunner) VerifyProbeObject(context.Context, string, string, string, int64) (ProbeObjectResult, error) {
	return ProbeObjectResult{}, errors.New("verification should not run")
}

func (r *blockingProbeRunner) DeleteProbeObject(context.Context, string, string, string) (ProbeCleanupResult, error) {
	return ProbeCleanupResult{}, errors.New("cleanup should not run")
}

func TestProbeServiceStopCancelsBlockingRunner(t *testing.T) {
	db := newProbeDB(t)
	now := time.Unix(100, 0)
	account := createProbeAccount(t, db, "blocking-key", now)
	createProbeTask(t, db, 1, account.QuotaKey)
	runner := &blockingProbeRunner{started: make(chan struct{}), canceled: make(chan struct{})}
	service := probeService(db, nil, &now)
	service.Runner = runner
	service.Every = time.Hour
	service.Start()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("blocking probe did not start")
	}
	done := make(chan struct{})
	go func() {
		service.Stop()
		close(done)
	}()
	select {
	case <-runner.canceled:
	case <-time.After(time.Second):
		t.Fatal("runner was not canceled")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("service stop did not return")
	}
}

func TestProbeServiceLifecycleStartStop(t *testing.T) {
	service := &ProbeService{Every: time.Hour}
	service.Start()
	service.Stop()
	service.Stop()
}
