package manualtransfer

import (
	"context"
	"errors"
	"sync"
	"testing"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
)

type gate2SerialFence struct {
	database            *gorm.DB
	mu                  sync.Mutex
	blockMu             sync.Mutex
	block               bool
	configuredOperation gate2FenceOperation
	entered             chan struct{}
	release             chan struct{}
}

type gate2FenceOperation string

type gate2FenceOperationKey struct{}

const (
	gate2AllocateOperation           gate2FenceOperation = "allocate"
	gate2AccountUpdateOperation      gate2FenceOperation = "account-update"
	gate2ReplacementAnalyzeOperation gate2FenceOperation = "replacement-analyze"
)

func gate2TaggedContext(ctx context.Context, operation gate2FenceOperation) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, gate2FenceOperationKey{}, operation)
}

func gate2OperationFromContext(ctx context.Context) gate2FenceOperation {
	if ctx == nil {
		return ""
	}
	operation, _ := ctx.Value(gate2FenceOperationKey{}).(gate2FenceOperation)
	return operation
}

func (f *gate2SerialFence) blockOperation(operation gate2FenceOperation, entered, release chan struct{}) {
	f.blockMu.Lock()
	f.block = true
	f.configuredOperation = operation
	f.entered = entered
	f.release = release
	f.blockMu.Unlock()
}

func (f *gate2SerialFence) takeBlock(operation gate2FenceOperation) (chan struct{}, chan struct{}, bool) {
	f.blockMu.Lock()
	defer f.blockMu.Unlock()
	if !f.block || (f.configuredOperation != "" && f.configuredOperation != operation) {
		return nil, nil, false
	}
	f.block = false
	return f.entered, f.release, true
}

func (f *gate2SerialFence) WithTaskExclusive(ctx context.Context, taskID uint, fn func(*models.Task) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if entered, release, blocked := f.takeBlock(gate2OperationFromContext(ctx)); blocked {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var task models.Task
	if err := f.database.First(&task, taskID).Error; err != nil {
		return err
	}
	return fn(&task)
}

func TestGate2RejectsNonManualTaskAcrossServiceBoundary(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	task := models.Task{Name: "legacy", SourceType: "local", SourceDir: root, DestType: "remote", RemoteDir: "/dest", TransferMode: models.TransferModeCopy, TaskType: "normal", Enabled: true}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(database)
	if _, err := service.GetTask(task.ID); !errors.Is(err, ErrNotManualTask) {
		t.Fatalf("GetTask error = %v", err)
	}
	if _, err := service.ListTaskAccounts(task.ID); !errors.Is(err, ErrNotManualTask) {
		t.Fatalf("ListTaskAccounts error = %v", err)
	}
	if _, err := service.CreateAnalyze(AnalyzeRequest{TaskID: task.ID, SourcePath: root, DestinationPath: "/dest", TransferMode: models.TransferModeCopy, IdempotencyKey: "legacy"}); !errors.Is(err, ErrNotManualTask) {
		t.Fatalf("CreateAnalyze error = %v", err)
	}
}

func TestGate2AccountSaveRequiresExplicitCASAndIdempotency(t *testing.T) {
	database := manualTestDB(t)
	task := manualTask(t, database, t.TempDir())
	service := NewService(database)
	if _, err := service.UpdateTaskAccounts(nil, UpdateTaskAccountsRequest{TaskID: task.ID, ExpectedRevision: 1, AccountIDs: []uint{1}}); err == nil {
		t.Fatal("account save synthesized an idempotency key")
	}
	if _, err := service.UpdateTaskAccounts(nil, UpdateTaskAccountsRequest{TaskID: task.ID, IdempotencyKey: "missing-revision", AccountIDs: []uint{1}}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("missing account revision error = %v", err)
	}
}

func TestGate2AllocateRequiresRunAndConfigCAS(t *testing.T) {
	database := manualTestDB(t)
	run := seedAllocationRun(t, database, 1, []allocationTestFile{{"file", 1}})
	service := NewService(database)
	if _, err := service.CreateAllocate(AllocateRequest{RunID: run.ID, ExpectedRevision: run.Revision, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: "missing-run"}); err == nil {
		t.Fatal("allocate accepted missing expected_run_id")
	}
	if _, err := service.CreateAllocate(AllocateRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision, IdempotencyKey: "missing-config"}); err == nil {
		t.Fatal("allocate accepted missing expected_config_revision")
	}
}

func TestGate2ReplacementAnalysisCannotRacePastAllocateFence(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	task := manualTask(t, database, root)
	service := NewService(database)
	service.Start()
	defer service.Stop()
	initial := manualRequest(task.ID, "race-initial")
	initial.SourcePath = root
	created, err := service.CreateAnalyze(initial)
	if err != nil {
		t.Fatal(err)
	}
	oldRun := waitManualRun(t, service, created.Run.ID, ManualRunStateAnalyzed)
	fence := &gate2SerialFence{database: database}
	service.TaskFence = fence
	replacement := initial
	replacement.IdempotencyKey = "race-replacement"
	replacement.ExpectedRunID = &oldRun.ID
	replacement.ExpectedRevision = &oldRun.Revision
	allocateBlocked := make(chan struct{})
	allocateRelease := make(chan struct{})
	fence.blockOperation(gate2AllocateOperation, allocateBlocked, allocateRelease)
	allocateDone := make(chan error, 1)
	go func() {
		_, allocateErr := service.CreateAllocateContext(gate2TaggedContext(context.Background(), gate2AllocateOperation), AllocateRequest{RunID: oldRun.ID, ExpectedRunID: &oldRun.ID, ExpectedRevision: oldRun.Revision, ExpectedConfigRevision: oldRun.ManualConfigRevision, IdempotencyKey: "race-allocate"})
		allocateDone <- allocateErr
	}()
	<-allocateBlocked
	replacementResult, err := service.CreateAnalyzeContext(gate2TaggedContext(context.Background(), gate2ReplacementAnalyzeOperation), replacement)
	if err != nil {
		t.Fatal(err)
	}
	replacementRun := waitManualRun(t, service, replacementResult.Run.ID, ManualRunStateAnalyzed)
	if replacementRun.ID == oldRun.ID {
		t.Fatal("replacement analyze did not commit a newer run before allocation")
	}
	close(allocateRelease)
	if err := <-allocateDone; !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("older allocation race error = %v", err)
	}
}

func TestGate2AccountUpdateCannotRacePastAllocateFence(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	task := manualTask(t, database, root)
	service := NewService(database)
	service.Start()
	defer service.Stop()
	request := manualRequest(task.ID, "account-race")
	request.SourcePath = root
	created, err := service.CreateAnalyze(request)
	if err != nil {
		t.Fatal(err)
	}
	run := waitManualRun(t, service, created.Run.ID, ManualRunStateAnalyzed)
	fence := &gate2SerialFence{database: database}
	service.TaskFence = fence
	allocateBlocked := make(chan struct{})
	allocateRelease := make(chan struct{})
	fence.blockOperation(gate2AllocateOperation, allocateBlocked, allocateRelease)
	allocateDone := make(chan error, 1)
	go func() {
		_, allocateErr := service.CreateAllocateContext(gate2TaggedContext(context.Background(), gate2AllocateOperation), AllocateRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: "account-race-allocate"})
		allocateDone <- allocateErr
	}()
	<-allocateBlocked
	updated, err := service.UpdateTaskAccounts(gate2TaggedContext(context.Background(), gate2AccountUpdateOperation), UpdateTaskAccountsRequest{TaskID: task.ID, AccountIDs: []uint{2, 1}, ExpectedRevision: 1, IdempotencyKey: "account-race-update"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Page.Revision != 2 {
		t.Fatalf("account update revision = %d, want 2", updated.Page.Revision)
	}
	close(allocateRelease)
	if err := <-allocateDone; !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("allocation after account CAS race error = %v", err)
	}
}

func TestGate2TaskAndAccountMutationInvalidateAnalyze(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	task := manualTask(t, database, root)
	service := NewService(database)
	service.Start()
	defer service.Stop()
	request := manualRequest(task.ID, "fence")
	request.SourcePath = root
	created, err := service.CreateAnalyze(request)
	if err != nil {
		t.Fatal(err)
	}
	run := waitManualRun(t, service, created.Run.ID, ManualRunStateAnalyzed)
	if err := database.Model(&models.Task{}).Where("id = ?", task.ID).Updates(map[string]interface{}{"source_dir": t.TempDir(), "manual_input_revision": run.ManualInputRevision + 1}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateAllocate(AllocateRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: "fence-allocate"}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("source mutation allocation error = %v", err)
	}
}

func TestGate2AccountIdentityMutationInvalidatesAnalyze(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	task := manualTask(t, database, root)
	service := NewService(database)
	service.Start()
	defer service.Stop()
	request := manualRequest(task.ID, "account-fence")
	request.SourcePath = root
	created, err := service.CreateAnalyze(request)
	if err != nil {
		t.Fatal(err)
	}
	run := waitManualRun(t, service, created.Run.ID, ManualRunStateAnalyzed)
	if err := database.Model(&models.QuotaAccount{}).Where("id = ?", 1).Update("remote_name", "changed-remote").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateAllocate(AllocateRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: "account-fence-allocate"}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("account mutation allocation error = %v", err)
	}
}

func TestGate2ConfigMutationInvalidatesAnalyze(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	task := manualTask(t, database, root)
	service := NewService(database)
	service.Start()
	defer service.Stop()
	request := manualRequest(task.ID, "config-fence")
	request.SourcePath = root
	created, err := service.CreateAnalyze(request)
	if err != nil {
		t.Fatal(err)
	}
	run := waitManualRun(t, service, created.Run.ID, ManualRunStateAnalyzed)
	if err := database.Model(&models.Task{}).Where("id = ?", task.ID).Update("rclone_config", "/tmp/changed-rclone.conf").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateAllocate(AllocateRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: "config-fence-allocate"}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("config mutation allocation error = %v", err)
	}
}

func TestGate2NewerRunBlocksOlderAllocation(t *testing.T) {
	database := manualTestDB(t)
	root := t.TempDir()
	task := manualTask(t, database, root)
	service := NewService(database)
	service.Start()
	defer service.Stop()
	firstRequest := manualRequest(task.ID, "first")
	firstRequest.SourcePath = root
	first, err := service.CreateAnalyze(firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	firstRun := waitManualRun(t, service, first.Run.ID, ManualRunStateAnalyzed)
	secondRequest := firstRequest
	secondRequest.IdempotencyKey = "second"
	secondRequest.ExpectedRunID = &firstRun.ID
	secondRequest.ExpectedRevision = &firstRun.Revision
	second, err := service.CreateAnalyze(secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	waitManualRun(t, service, second.Run.ID, ManualRunStateAnalyzed)
	if _, err := service.CreateAllocate(AllocateRequest{RunID: firstRun.ID, ExpectedRunID: &firstRun.ID, ExpectedRevision: firstRun.Revision, ExpectedConfigRevision: firstRun.ManualConfigRevision, IdempotencyKey: "old"}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("older allocation error = %v", err)
	}
}

func TestGate2ReasonBreakdownAndFilterCursorFence(t *testing.T) {
	database := manualTestDB(t)
	files := []allocationTestFile{
		{"a", 600_000_000_000}, {"b", 600_000_000_000}, {"c", 600_000_000_000}, {"d", 600_000_000_000},
		{"e", 600_000_000_000}, {"oversize", PerAccountBudgetBytes + 1},
	}
	run := seedAllocationRun(t, database, 4, files)
	service := NewService(database)
	service.Start()
	defer service.Stop()
	result, err := service.CreateAllocate(AllocateRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: "reasons"})
	if err != nil {
		t.Fatal(err)
	}
	allocated := waitManualRun(t, service, result.Run.ID, ManualRunStateAllocated)
	if allocated.OversizeCount != 1 || allocated.OversizeBytes != PerAccountBudgetBytes+1 || allocated.AggregateCapacityCount != 1 || allocated.AggregateCapacityBytes != 600_000_000_000 {
		t.Fatalf("reason breakdown = %#v", allocated)
	}
	if allocated.AccountCapacityCount != 0 {
		t.Fatalf("unexpected account-capacity count = %d", allocated.AccountCapacityCount)
	}
	summary, err := service.GetAllocationSummary(run.ID)
	if err != nil || summary.OversizeCount != 1 || summary.AggregateCapacityCount != 1 {
		t.Fatalf("summary = %#v, err=%v", summary, err)
	}
	page, err := service.ListAllocationFilesFiltered(run.ID, "", 1, "assigned", "", "")
	if err != nil || !page.HasMore {
		t.Fatalf("assigned page = %#v, err=%v", page, err)
	}
	for _, tc := range []struct {
		assignment string
		reason     string
		accountID  string
	}{
		{assignment: "unassigned"},
		{assignment: "assigned", reason: "oversize"},
		{assignment: "", reason: "aggregate_capacity"},
		{assignment: "", accountID: "2"},
	} {
		if _, err := service.ListAllocationFilesFiltered(run.ID, page.NextCursor, 1, tc.assignment, tc.reason, tc.accountID); err == nil {
			t.Fatalf("cursor filter mismatch accepted: %#v", tc)
		}
	}
	for _, reason := range []string{"unassigned", "no_fit", "Oversize"} {
		if _, err := service.ListAllocationFilesFiltered(run.ID, "", 1, "", reason, ""); err == nil {
			t.Fatalf("non-exact reason accepted: %q", reason)
		}
	}
}

func TestAllocationFiltersRemainBoundAfterWorkerRunStateChanges(t *testing.T) {
	database := manualTestDB(t)
	run := seedAllocationRun(t, database, 2, []allocationTestFile{{"a", 1}, {"b", 1}, {"c", PerAccountBudgetBytes + 1}})
	service := NewService(database)
	service.Start()
	defer service.Stop()
	result, err := service.CreateAllocate(AllocateRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: "filter-states"})
	if err != nil {
		t.Fatal(err)
	}
	allocated := waitManualRun(t, service, result.Run.ID, ManualRunStateAllocated)
	for _, state := range []string{ManualRunStateRunning, ManualRunStateSucceeded, ManualRunStateCancelled, ManualRunStateNeedsAttention} {
		if err := database.Model(&ManualTransferRun{}).Where("id = ?", allocated.ID).Update("state", state).Error; err != nil {
			t.Fatal(err)
		}
		assigned, err := service.ListFilesFiltered(allocated.ID, "", 1, "assigned", "", "")
		if err != nil || len(assigned.Files) == 0 {
			t.Fatalf("assigned filter state=%s page=%#v err=%v", state, assigned, err)
		}
		account, err := service.ListFilesFiltered(allocated.ID, "", 10, "", "", "1")
		if err != nil || len(account.Files) == 0 {
			t.Fatalf("account filter state=%s page=%#v err=%v", state, account, err)
		}
		for _, file := range account.Files {
			if file.AccountID != 1 {
				t.Fatalf("account filter state=%s returned account %d", state, file.AccountID)
			}
		}
		if assigned.HasMore {
			if _, err := service.ListFilesFiltered(allocated.ID, assigned.NextCursor, 1, "assigned", "", ""); err != nil {
				t.Fatalf("same cursor rejected after state=%s: %v", state, err)
			}
		}
	}
}

func TestGate2LargeAllocationUsesDurableBatches(t *testing.T) {
	database := manualTestDB(t)
	files := make([]allocationTestFile, 1025)
	for index := range files {
		files[index] = allocationTestFile{path: formatTestNumber(index), size: 1}
	}
	run := seedAllocationRun(t, database, 2, files)
	service := NewService(database)
	service.Start()
	defer service.Stop()
	created, err := service.CreateAllocate(AllocateRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: "large-allocation"})
	if err != nil {
		t.Fatal(err)
	}
	allocated := waitManualRun(t, service, created.Run.ID, ManualRunStateAllocated)
	if allocated.AssignedCount != int64(len(files)) {
		t.Fatalf("assigned count = %d, want %d", allocated.AssignedCount, len(files))
	}
	var rows int64
	if err := database.Model(&ManualRunAllocation{}).Where("run_id = ? AND generation = ? AND activated_at IS NOT NULL", run.ID, allocated.AllocationGeneration).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != int64(len(files)) {
		t.Fatalf("durable allocation rows = %d, want %d", rows, len(files))
	}
}
