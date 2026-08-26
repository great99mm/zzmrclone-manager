package manualtransfer

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
)

func waitSettlementState(t *testing.T, service *Service, runID uint, want string) ManualTransferRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := service.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if normalizedSettlementState(run.SettlementState) == want {
			return run
		}
		time.Sleep(5 * time.Millisecond)
	}
	run, _ := service.GetRun(runID)
	t.Fatalf("run %d settlement state = %q, want %q (error=%q)", runID, run.SettlementState, want, run.SettlementError)
	return run
}

func stopFixtureRun(t *testing.T, fixture manualWorkerFixture) ManualTransferRun {
	t.Helper()
	run, err := fixture.service.GetRun(fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.StopRun(context.Background(), SettlementRequest{RunID: run.ID, ExpectedRevision: run.Revision, IdempotencyKey: "stop-run", ActorIdentity: "operator", ActorType: "admin_session"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.SettlementState != ManualSettlementStateStopped {
		t.Fatalf("stop state = %q", result.Run.SettlementState)
	}
	return result.Run
}

func makeMixedWorkerOutcome(t *testing.T, fixture manualWorkerFixture, workerIDs []uint) uint {
	t.Helper()
	for _, workerID := range workerIDs {
		waitWorkerState(t, fixture.service, workerID, ManualWorkerStateSucceeded)
	}
	failedID := workerIDs[0]
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&ManualRunWorker{}).Where("id = ?", failedID).Update("state", ManualWorkerStateFailed).Error; err != nil {
			return err
		}
		if err := tx.Model(&ManualTransferRun{}).Where("id = ?", fixture.run.ID).Updates(map[string]interface{}{
			"settlement_state":          ManualSettlementStateActive,
			"settlement_checked_count":  0,
			"settlement_verified_count": 0,
			"settlement_verified_bytes": 0,
			"settlement_finished_at":    nil,
		}).Error; err != nil {
			return err
		}
		return fixture.service.deriveRunStateTx(tx, fixture.run.ID)
	}); err != nil {
		t.Fatal(err)
	}
	return failedID
}

func TestManualRunAllWorkersSucceededSkipsSettlement(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a", "b.txt": "bb", "c.txt": "ccc"})
	started := startFixture(t, fixture, "all-succeeded-start")
	for _, workerID := range started.WorkerIDs {
		waitWorkerState(t, fixture.service, workerID, ManualWorkerStateSucceeded)
	}
	run, err := fixture.service.GetRun(fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != ManualRunStateSucceeded || run.SettlementState != ManualSettlementStateFinished || run.SettlementFinishedAt == nil {
		t.Fatalf("successful run did not finish automatically: state=%q settlement=%q finished_at=%v", run.State, run.SettlementState, run.SettlementFinishedAt)
	}
	if run.SettlementCheckedCount != 3 || run.SettlementVerifiedCount != 3 || run.SettlementVerifiedBytes != 6 || run.SettlementReleasedCount != 0 {
		t.Fatalf("automatic settlement totals = checked %d verified %d/%d released %d", run.SettlementCheckedCount, run.SettlementVerifiedCount, run.SettlementVerifiedBytes, run.SettlementReleasedCount)
	}
	stop, err := fixture.service.StopRun(context.Background(), SettlementRequest{RunID: run.ID, ExpectedRevision: started.Run.Revision, IdempotencyKey: "unneeded-stop", ActorIdentity: "operator", ActorType: "admin_session"})
	if err != nil || !stop.Existing || stop.Run.SettlementState != ManualSettlementStateFinished {
		t.Fatalf("stop on successful run = %#v, err=%v", stop, err)
	}
	inputs := make([]AccountInput, 0, len(fixture.accounts))
	for _, account := range fixture.accounts {
		inputs = append(inputs, AccountInput{AccountID: account.AccountID})
	}
	result, err := fixture.service.CreateAnalyzeContext(context.Background(), AnalyzeRequest{TaskID: run.TaskID, SourcePath: run.SourcePath, DestinationPath: run.DestinationPath, TransferMode: run.TransferMode, Accounts: inputs, IdempotencyKey: "after-success", ExpectedRunID: &run.ID, ExpectedRevision: &run.Revision, ActorIdentity: "operator", ActorType: "admin_session"})
	if err != nil || result.Run.ID == run.ID {
		t.Fatalf("reanalyze after automatic finish = run %d, err=%v", result.Run.ID, err)
	}
}

func TestManualRunSettlementStopsReconcilesReleasesAndFinishes(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a", "b.txt": "bb", "c.txt": "ccc"})
	started := startFixture(t, fixture, "settlement-start")
	failedWorkerID := makeMixedWorkerOutcome(t, fixture, started.WorkerIDs)
	current, err := fixture.service.GetRun(fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	inputs := make([]AccountInput, 0, len(fixture.accounts))
	for _, account := range fixture.accounts {
		inputs = append(inputs, AccountInput{AccountID: account.AccountID})
	}
	if _, err := fixture.service.CreateAnalyzeContext(context.Background(), AnalyzeRequest{TaskID: current.TaskID, SourcePath: current.SourcePath, DestinationPath: current.DestinationPath, TransferMode: current.TransferMode, Accounts: inputs, IdempotencyKey: "before-finish", ExpectedRunID: &current.ID, ExpectedRevision: &current.Revision, ActorIdentity: "operator", ActorType: "admin_session"}); !errors.Is(err, ErrSettlementConflict) {
		t.Fatalf("reanalyze before finish error = %v, want settlement conflict", err)
	}
	stopped := stopFixtureRun(t, fixture)
	if _, err := fixture.service.RetryWorker(context.Background(), failedWorkerID, "operator", "admin_session"); !errors.Is(err, ErrSettlementConflict) {
		t.Fatalf("retry after stop error = %v, want settlement conflict", err)
	}
	fixture.runner.mu.Lock()
	fixture.runner.remote["a.txt"] = false
	fixture.runner.sizes["b.txt"]++
	fixture.runner.mu.Unlock()
	reconcile, err := fixture.service.ReconcileRun(context.Background(), SettlementRequest{RunID: stopped.ID, ExpectedRevision: stopped.Revision, IdempotencyKey: "reconcile-run", ActorIdentity: "operator", ActorType: "admin_session"})
	if err != nil || reconcile.Run.SettlementState != ManualSettlementStateReconciling {
		t.Fatalf("reconcile request = %#v, err=%v", reconcile, err)
	}
	reconciled := waitSettlementState(t, fixture.service, fixture.run.ID, ManualSettlementStateReconciled)
	if reconciled.SettlementVerifiedCount != 1 || reconciled.SettlementReleasedCount != 2 {
		t.Fatalf("settlement counts = verified %d released %d", reconciled.SettlementVerifiedCount, reconciled.SettlementReleasedCount)
	}
	var files []ManualWorkerFile
	if err := fixture.db.Where("run_id = ?", fixture.run.ID).Order("relative_path ASC").Find(&files).Error; err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	reasons := map[string]string{}
	for _, file := range files {
		states[file.RelativePath] = file.State
		reasons[file.RelativePath] = file.ReleaseReason
	}
	if states["a.txt"] != ManualWorkerFileStateReleased || reasons["a.txt"] != ManualWorkerFileReleaseRemoteMissing {
		t.Fatalf("missing file = state %q reason %q", states["a.txt"], reasons["a.txt"])
	}
	if states["b.txt"] != ManualWorkerFileStateReleased || reasons["b.txt"] != ManualWorkerFileReleaseSizeMismatch {
		t.Fatalf("mismatched file = state %q reason %q", states["b.txt"], reasons["b.txt"])
	}
	finished, err := fixture.service.FinishRun(context.Background(), SettlementRequest{RunID: reconciled.ID, ExpectedRevision: reconciled.Revision, IdempotencyKey: "finish-run", ActorIdentity: "operator", ActorType: "admin_session"})
	if err != nil || finished.Run.SettlementState != ManualSettlementStateFinished {
		t.Fatalf("finish = %#v, err=%v", finished, err)
	}
	replay, err := fixture.service.FinishRun(context.Background(), SettlementRequest{RunID: reconciled.ID, ExpectedRevision: reconciled.Revision, IdempotencyKey: "finish-run", ActorIdentity: "operator", ActorType: "admin_session"})
	if err != nil || !replay.Existing {
		t.Fatalf("finish replay = %#v, err=%v", replay, err)
	}
}

func TestManualRunSettlementTransientRemoteErrorRollsBackFileStates(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a", "b.txt": "bb"})
	started := startFixture(t, fixture, "rollback-start")
	makeMixedWorkerOutcome(t, fixture, started.WorkerIDs)
	stopped := stopFixtureRun(t, fixture)
	fixture.runner.mu.Lock()
	fixture.runner.remote["a.txt"] = false
	fixture.runner.statErrors = map[string]error{"b.txt": errors.New("temporary network failure")}
	fixture.runner.mu.Unlock()
	if _, err := fixture.service.ReconcileRun(context.Background(), SettlementRequest{RunID: stopped.ID, ExpectedRevision: stopped.Revision, IdempotencyKey: "rollback-reconcile", ActorIdentity: "operator", ActorType: "admin_session"}); err != nil {
		t.Fatal(err)
	}
	failed := waitSettlementState(t, fixture.service, fixture.run.ID, ManualSettlementStateStopped)
	if failed.SettlementError == "" {
		t.Fatal("transient failure did not persist settlement error")
	}
	var released int64
	if err := fixture.db.Model(&ManualWorkerFile{}).Where("run_id = ? AND state = ?", fixture.run.ID, ManualWorkerFileStateReleased).Count(&released).Error; err != nil {
		t.Fatal(err)
	}
	if released != 0 {
		t.Fatalf("released files after transient failure = %d, want 0", released)
	}
	if _, err := fixture.service.FinishRun(context.Background(), SettlementRequest{RunID: failed.ID, ExpectedRevision: failed.Revision, IdempotencyKey: "early-finish", ActorIdentity: "operator", ActorType: "admin_session"}); !errors.Is(err, ErrSettlementConflict) {
		t.Fatalf("finish after failed reconciliation error = %v", err)
	}
}

func TestRecoverSettlementsReturnsInterruptedComparisonToStopped(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a"})
	if err := fixture.db.Model(&ManualTransferRun{}).Where("id = ?", fixture.run.ID).Updates(map[string]interface{}{"settlement_state": ManualSettlementStateReconciling, "settlement_checked_count": 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.RecoverSettlements(); err != nil {
		t.Fatal(err)
	}
	run, err := fixture.service.GetRun(fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.SettlementState != ManualSettlementStateStopped || run.SettlementError == "" {
		t.Fatalf("recovered settlement = state %q error %q", run.SettlementState, run.SettlementError)
	}
}

func TestRecoverSettlementsCompletesInterruptedStopWhenWorkersAreTerminal(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a"})
	worker := ManualRunWorker{RunID: fixture.run.ID, AccountID: fixture.accounts[0].AccountID, AccountPosition: fixture.accounts[0].Position, AccountIdentity: fixture.accounts[0].AccountIdentity, RemoteName: fixture.accounts[0].RemoteName, ConfigIdentity: fixture.accounts[0].ConfigIdentity, State: ManualWorkerStateCancelled, AttemptNumber: 1, Revision: 1, CancelRequested: true, AssignedCount: 1, AssignedBytes: 1}
	if err := fixture.db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&ManualTransferRun{}).Where("id = ?", fixture.run.ID).Updates(map[string]interface{}{"state": ManualRunStateCancelled, "settlement_state": ManualSettlementStateStopping, "settlement_error": "waiting for workers"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.RecoverSettlements(); err != nil {
		t.Fatal(err)
	}
	run, err := fixture.service.GetRun(fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.SettlementState != ManualSettlementStateStopped || run.SettlementStoppedAt == nil || run.SettlementError != "" {
		t.Fatalf("recovered stop = state %q stopped_at=%v error=%q", run.SettlementState, run.SettlementStoppedAt, run.SettlementError)
	}
}

func TestRecoverSettlementsFinishesHistoricalSuccessfulRun(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a", "b.txt": "bb"})
	started := startFixture(t, fixture, "historical-success-start")
	for _, workerID := range started.WorkerIDs {
		waitWorkerState(t, fixture.service, workerID, ManualWorkerStateSucceeded)
	}
	if err := fixture.db.Model(&ManualTransferRun{}).Where("id = ?", fixture.run.ID).Updates(map[string]interface{}{
		"settlement_state":          ManualSettlementStateActive,
		"settlement_checked_count":  0,
		"settlement_verified_count": 0,
		"settlement_verified_bytes": 0,
		"settlement_finished_at":    nil,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.RecoverSettlements(); err != nil {
		t.Fatal(err)
	}
	run, err := fixture.service.GetRun(fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.SettlementState != ManualSettlementStateFinished || run.SettlementFinishedAt == nil || run.SettlementVerifiedCount != 2 {
		t.Fatalf("historical successful run recovery = state %q finished_at=%v verified=%d", run.SettlementState, run.SettlementFinishedAt, run.SettlementVerifiedCount)
	}
}
