package manualtransfer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/proactive"
	"rclone-manager/internal/quota"
)

var (
	ErrManualMoveUnsupported = errors.New("manual move retries require manual resolution")
	ErrWorkerNotFound        = errors.New("manual worker not found")
	ErrWorkerConflict        = errors.New("manual worker state conflict")
	ErrWorkerNoIncomplete    = errors.New("manual worker has no incomplete files")
	ErrWorkerUnavailable     = errors.New("manual worker execution is unavailable")
)

const (
	manualWorkerLogLimit      int64 = 64 << 10
	manualWorkerLeaseDuration       = 10 * time.Minute
	manualWorkerLeaseRenewal        = 2 * time.Minute
)

type StartRequest struct {
	RunID                  uint
	ExpectedRunID          *uint
	ExpectedRevision       int64
	ExpectedConfigRevision int64
	IdempotencyKey         string
	ActorIdentity          string
	ActorType              string
}

type StartResult struct {
	Run       ManualTransferRun
	Existing  bool
	WorkerIDs []uint
}

type WorkerDetail struct {
	Worker   ManualRunWorker       `json:"worker"`
	Attempts []ManualWorkerAttempt `json:"attempts"`
	Files    []ManualWorkerFile    `json:"files"`
}

type WorkerLogPage struct {
	WorkerID   uint   `json:"worker_id"`
	Offset     int64  `json:"offset"`
	Limit      int64  `json:"limit"`
	NextOffset int64  `json:"next_offset"`
	EOF        bool   `json:"eof"`
	Data       string `json:"data"`
}

type workerProcessInspector interface {
	Inspect(int, string) (proactive.ProcessStatus, error)
	StopVerified(int, string) error
}

type ownedWorkerProcess struct {
	attemptID uint
	lease     string
	process   proactive.ProcessHandle
	cancel    context.CancelFunc
}

type workerProgressState struct {
	mu          sync.Mutex
	value       proactive.ProcessProgress
	initialized bool
}

type workerStreamRedactors struct {
	stdout *workerLogRedactor
	stderr *workerLogRedactor
}

func newWorkerStreamRedactors() *workerStreamRedactors {
	return &workerStreamRedactors{stdout: newWorkerLogRedactor(), stderr: newWorkerLogRedactor()}
}

func (r *workerStreamRedactors) forStream(stream string) *workerLogRedactor {
	if r != nil && strings.EqualFold(stream, "stderr") {
		return r.stderr
	}
	if r != nil {
		return r.stdout
	}
	return newWorkerLogRedactor()
}

func newWorkerProgressState(worker ManualRunWorker) *workerProgressState {
	return &workerProgressState{value: proactive.ProcessProgress{RelativePath: worker.CurrentRelativePath, CompletedCount: worker.CompletedCount, CompletedBytes: worker.CompletedBytes, SpeedBytesPerSecond: worker.SpeedBytesPerSecond, ProgressPercent: worker.ProgressPercent}, initialized: true}
}

func (p *workerProgressState) merge(value proactive.ProcessProgress) proactive.ProcessProgress {
	p.mu.Lock()
	defer p.mu.Unlock()
	rich := value.CompletedCount != 0 || value.CompletedBytes != 0 || value.SpeedBytesPerSecond != 0 || value.RelativePath != ""
	if !p.initialized || rich {
		if rich || !p.initialized {
			p.value.CompletedCount = value.CompletedCount
			p.value.CompletedBytes = value.CompletedBytes
			p.value.SpeedBytesPerSecond = value.SpeedBytesPerSecond
			p.value.RelativePath = value.RelativePath
		}
	}
	if value.ProgressPercent != 0 || !p.initialized || rich {
		p.value.ProgressPercent = value.ProgressPercent
	}
	p.initialized = true
	return p.value
}

func (s *Service) StartRun(ctx context.Context, request StartRequest) (StartResult, error) {
	if s == nil || s.DB == nil {
		return StartResult{}, errors.New("manual transfer database is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateStartRequest(&request); err != nil {
		return StartResult{}, err
	}
	if s.workerCtx == nil {
		return StartResult{}, ErrWorkerUnavailable
	}
	var seed ManualTransferRun
	if err := s.DB.First(&seed, request.RunID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return StartResult{}, ErrRunNotFound
		}
		return StartResult{}, err
	}
	result := StartResult{}
	start := func(_ *models.Task) error {
		var err error
		result, err = s.startRunUnderFence(request)
		return err
	}
	var err error
	if s.TaskFence != nil {
		err = s.TaskFence.WithTaskExclusive(ctx, seed.TaskID, start)
	} else {
		err = start(nil)
	}
	if err != nil {
		return StartResult{}, err
	}
	for _, workerID := range result.WorkerIDs {
		s.launchWorker(workerID)
	}
	return result, nil
}

func (s *Service) startRunUnderFence(request StartRequest) (StartResult, error) {
	fingerprint := fingerprintBytes(fmt.Sprintf("%d\x00%d\x00%d\x00%d", request.RunID, request.ExpectedRevision, request.ExpectedConfigRevision, ManualAllocationVersion))
	result := StartResult{}
	err := retryManualSQLite(func() error {
		return s.DB.Transaction(func(tx *gorm.DB) error {
			result = StartResult{}
			var run ManualTransferRun
			if err := tx.First(&run, request.RunID).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrRunNotFound
				}
				return err
			}
			var task models.Task
			if err := tx.First(&task, run.TaskID).Error; err != nil {
				return err
			}
			if err := requireManualTask(&task); err != nil {
				return err
			}
			if err := validateRunFenceDB(tx, &task, run); err != nil {
				return err
			}
			if request.ExpectedRunID == nil || *request.ExpectedRunID != run.ID {
				return ErrRevisionConflict
			}
			if run.WorkerStartIdempotencyKey != "" {
				if run.WorkerStartIdempotencyKey != request.IdempotencyKey || run.WorkerStartFingerprint != fingerprint {
					return ErrIdempotencyConflict
				}
				result.Existing = true
				result.Run = run
				return tx.Model(&ManualRunWorker{}).Where("run_id = ?", run.ID).Order("account_position ASC, id ASC").Pluck("id", &result.WorkerIDs).Error
			}
			if request.ExpectedRevision != run.Revision || request.ExpectedConfigRevision != normalizedManualAccountRevision(run.ManualConfigRevision) {
				return ErrRevisionConflict
			}
			var latest ManualTransferRun
			if err := tx.Where("task_id = ?", run.TaskID).Order("id DESC").First(&latest).Error; err != nil {
				return err
			}
			if latest.ID != run.ID {
				return ErrRevisionConflict
			}
			if run.State != ManualRunStateAllocated {
				return ErrWorkerConflict
			}
			if run.AllocationGeneration <= 0 {
				return ErrAllocationUnavailable
			}

			var allocations []ManualRunAllocation
			if err := tx.Where("run_id = ? AND generation = ? AND activated_at IS NOT NULL AND unassigned_reason = ''", run.ID, run.AllocationGeneration).Order("account_position ASC, relative_path ASC, id ASC").Find(&allocations).Error; err != nil {
				return err
			}
			if len(allocations) == 0 {
				return ErrWorkerConflict
			}
			var snapshotFiles []ManualRunFile
			if err := tx.Where("run_id = ? AND generation = ? AND activated_at IS NOT NULL", run.ID, run.SnapshotGeneration).Find(&snapshotFiles).Error; err != nil {
				return err
			}
			bySnapshot := make(map[string]ManualRunFile, len(snapshotFiles))
			for _, file := range snapshotFiles {
				bySnapshot[file.SnapshotKey] = file
			}
			type group struct {
				position int
				account  ManualRunAllocation
				files    []ManualRunFile
			}
			groups := make(map[int]*group)
			for _, allocation := range allocations {
				if allocation.AccountPosition == nil || allocation.AccountID == 0 {
					return errors.New("allocated file is missing its account assignment")
				}
				snapshot, ok := bySnapshot[allocation.SnapshotKey]
				if !ok || snapshot.RelativePath != allocation.RelativePath || snapshot.SizeBytes != allocation.SizeBytes {
					return errors.New("allocated file is missing its immutable snapshot")
				}
				position := *allocation.AccountPosition
				current := groups[position]
				if current == nil {
					current = &group{position: position, account: allocation}
					groups[position] = current
				}
				current.files = append(current.files, snapshot)
			}
			positions := make([]int, 0, len(groups))
			for position := range groups {
				positions = append(positions, position)
			}
			sort.Ints(positions)
			for _, position := range positions {
				current := groups[position]
				worker := ManualRunWorker{RunID: run.ID, AccountID: current.account.AccountID, AccountPosition: position, AccountIdentity: current.account.AccountIdentity, ConfigIdentity: run.ConfigIdentity, State: ManualWorkerStatePending, AttemptNumber: 1, Revision: 1, AssignedCount: int64(len(current.files))}
				var account ManualRunAccount
				if err := tx.Where("run_id = ? AND position = ?", run.ID, position).First(&account).Error; err != nil {
					return err
				}
				if account.AccountID != current.account.AccountID || account.AccountIdentity != current.account.AccountIdentity {
					return ErrRevisionConflict
				}
				worker.AccountID = account.AccountID
				worker.AccountIdentity = account.AccountIdentity
				worker.RemoteName = account.RemoteName
				worker.ConfigIdentity = account.ConfigIdentity
				for _, file := range current.files {
					worker.AssignedBytes += file.SizeBytes
				}
				if err := tx.Create(&worker).Error; err != nil {
					return err
				}
				attempt := ManualWorkerAttempt{RunID: run.ID, WorkerID: worker.ID, AttemptNumber: 1, State: ManualWorkerStatePending, AssignedCount: worker.AssignedCount, AssignedBytes: worker.AssignedBytes}
				if err := tx.Create(&attempt).Error; err != nil {
					return err
				}
				if err := tx.Model(&worker).Update("current_attempt_id", attempt.ID).Error; err != nil {
					return err
				}
				for _, file := range current.files {
					if err := quota.ValidateRelativePath(file.RelativePath); err != nil {
						return err
					}
					if err := tx.Create(&ManualWorkerFile{RunID: run.ID, WorkerID: worker.ID, AttemptID: attempt.ID, RelativePath: file.RelativePath, SnapshotKey: file.SnapshotKey, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode, State: ManualWorkerFileStatePending}).Error; err != nil {
						return err
					}
				}
				logPath, err := s.workerLogPath(worker.ID)
				if err != nil {
					return err
				}
				if err := tx.Create(&ManualWorkerLog{RunID: run.ID, WorkerID: worker.ID, Path: logPath}).Error; err != nil {
					return err
				}
				result.WorkerIDs = append(result.WorkerIDs, worker.ID)
			}
			if len(result.WorkerIDs) == 0 {
				return ErrWorkerConflict
			}
			now := s.now()
			updated := tx.Model(&ManualTransferRun{}).Where("id = ? AND state = ? AND revision = ?", run.ID, ManualRunStateAllocated, request.ExpectedRevision).Updates(map[string]interface{}{
				"state":                        ManualRunStateRunning,
				"revision":                     gorm.Expr("revision + 1"),
				"worker_start_idempotency_key": request.IdempotencyKey,
				"worker_start_fingerprint":     fingerprint,
				"worker_started_at":            now,
			})
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected != 1 {
				return ErrRevisionConflict
			}
			if err := tx.First(&result.Run, run.ID).Error; err != nil {
				return err
			}
			return createEventAs(tx, result.Run, ManualRunEventWorkersStarted, ManualRunStateAllocated, ManualRunStateRunning, fmt.Sprintf("started %d %s workers", len(result.WorkerIDs), run.TransferMode), request.ActorIdentity, request.ActorType)
		})
	})
	if err != nil {
		return StartResult{}, err
	}
	return result, nil
}

func validateStartRequest(request *StartRequest) error {
	if request.RunID == 0 || request.ExpectedRunID == nil {
		return errors.New("expected_run_id is required")
	}
	if request.ExpectedRevision <= 0 || request.ExpectedConfigRevision <= 0 {
		return ErrRevisionConflict
	}
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 256 || strings.ContainsAny(request.IdempotencyKey, "\x00\r\n") {
		return errors.New("idempotency key is required")
	}
	if request.ActorIdentity == "" {
		request.ActorIdentity = "unknown-operator"
	}
	if request.ActorType == "" {
		request.ActorType = "admin_session"
	}
	return nil
}

// validateRunFenceDB is called while the task launch fence and the start
// transaction are both held. It validates the live task and durable account
// identities instead of trusting the analyzed run snapshot alone.
func validateRunFenceDB(tx *gorm.DB, task *models.Task, run ManualTransferRun) error {
	if task == nil || task.ID == 0 || run.TaskID != task.ID || !task.Enabled {
		return ErrRevisionConflict
	}
	if err := requireManualTask(task); err != nil {
		return err
	}
	if strings.TrimSpace(task.SourceType) != "local" || strings.TrimSpace(task.DestType) != "remote" {
		return ErrRevisionConflict
	}
	if normalizedManualInputRevision(task.ManualInputRevision) != normalizedManualInputRevision(run.ManualInputRevision) || normalizedManualAccountRevision(task.ManualAccountRevision) != normalizedManualAccountRevision(run.ManualConfigRevision) {
		return ErrRevisionConflict
	}
	configIdentity, err := canonicalTaskConfig(task)
	if err != nil || configIdentity != run.ConfigIdentity || run.SourcePath != task.SourceDir || run.TransferMode != task.TransferMode || !manualDestinationMatches(run.DestinationPath, task) {
		return ErrRevisionConflict
	}
	var runAccounts []ManualRunAccount
	if err := tx.Where("run_id = ?", run.ID).Order("position ASC, id ASC").Find(&runAccounts).Error; err != nil {
		return err
	}
	if len(runAccounts) == 0 {
		return ErrRevisionConflict
	}
	for _, frozen := range runAccounts {
		var account models.QuotaAccount
		if err := tx.First(&account, frozen.AccountID).Error; err != nil {
			return ErrRevisionConflict
		}
		if !account.Enabled || strings.TrimSpace(account.QuotaKey) != frozen.AccountIdentity || strings.TrimSpace(account.RemoteName) != frozen.RemoteName {
			return ErrRevisionConflict
		}
		accountConfig := strings.TrimSpace(account.ConfigIdentity)
		if accountConfig == "" {
			accountConfig = configIdentity
		}
		if accountConfig != frozen.ConfigIdentity {
			return ErrRevisionConflict
		}
	}
	if tx.Migrator().HasTable(&ManualTaskAccount{}) {
		var configured []ManualTaskAccount
		if err := tx.Where("task_id = ?", task.ID).Order("position ASC, id ASC").Find(&configured).Error; err != nil {
			return err
		}
		if len(configured) > 0 {
			if len(configured) != len(runAccounts) {
				return ErrRevisionConflict
			}
			for index, configuredAccount := range configured {
				frozen := runAccounts[index]
				if configuredAccount.Position != frozen.Position || configuredAccount.AccountID != frozen.AccountID || !configuredAccount.Enabled || configuredAccount.AccountIdentity != frozen.AccountIdentity || configuredAccount.RemoteName != frozen.RemoteName || configuredAccount.ConfigIdentity != frozen.ConfigIdentity {
					return ErrRevisionConflict
				}
			}
		}
	}
	if run.ManualConfigFingerprint != "" {
		accounts := make([]frozenAccount, 0, len(runAccounts))
		for _, account := range runAccounts {
			accounts = append(accounts, frozenAccount{Position: account.Position, AccountID: account.AccountID, AccountIdentity: account.AccountIdentity, RemoteName: account.RemoteName, ConfigIdentity: account.ConfigIdentity})
		}
		if manualConfigFingerprint(task, accounts) != run.ManualConfigFingerprint {
			return ErrRevisionConflict
		}
	}
	return nil
}

func (s *Service) GetRunWorkers(runID uint) ([]ManualRunWorker, error) {
	if _, err := s.GetRun(runID); err != nil {
		return nil, err
	}
	var workers []ManualRunWorker
	if err := s.DB.Where("run_id = ?", runID).Order("account_position ASC, id ASC").Find(&workers).Error; err != nil {
		return nil, err
	}
	for index := range workers {
		if err := s.decorateWorkerActionability(&workers[index]); err != nil {
			return nil, err
		}
	}
	return workers, nil
}

func (s *Service) GetWorker(workerID uint) (WorkerDetail, error) {
	var worker ManualRunWorker
	if err := s.DB.First(&worker, workerID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return WorkerDetail{}, ErrWorkerNotFound
		}
		return WorkerDetail{}, err
	}
	if _, err := s.GetRun(worker.RunID); err != nil {
		return WorkerDetail{}, err
	}
	var attempts []ManualWorkerAttempt
	if err := s.DB.Where("worker_id = ?", worker.ID).Order("attempt_number ASC").Find(&attempts).Error; err != nil {
		return WorkerDetail{}, err
	}
	var files []ManualWorkerFile
	if err := s.DB.Where("worker_id = ?", worker.ID).Order("relative_path ASC, id ASC").Find(&files).Error; err != nil {
		return WorkerDetail{}, err
	}
	if err := s.decorateWorkerActionability(&worker); err != nil {
		return WorkerDetail{}, err
	}
	return WorkerDetail{Worker: worker, Attempts: attempts, Files: files}, nil
}

func (s *Service) GetWorkerLogs(workerID uint, offset, limit int64) (WorkerLogPage, error) {
	if offset < 0 || limit < 1 || limit > manualWorkerLogLimit {
		return WorkerLogPage{}, errors.New("offset and limit are out of bounds")
	}
	var worker ManualRunWorker
	if err := s.DB.First(&worker, workerID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return WorkerLogPage{}, ErrWorkerNotFound
		}
		return WorkerLogPage{}, err
	}
	if _, err := s.GetRun(worker.RunID); err != nil {
		return WorkerLogPage{}, err
	}
	var record ManualWorkerLog
	if err := s.DB.Where("worker_id = ? AND run_id = ?", workerID, worker.RunID).First(&record).Error; err != nil {
		return WorkerLogPage{}, err
	}
	if err := s.validateWorkerLogPath(record.Path, workerID); err != nil {
		return WorkerLogPage{}, err
	}
	file, err := os.Open(record.Path)
	if errors.Is(err, os.ErrNotExist) {
		return WorkerLogPage{WorkerID: workerID, Offset: offset, Limit: limit, NextOffset: offset, EOF: workerLogTerminal(worker.State) && offset == 0}, nil
	}
	if err != nil {
		return WorkerLogPage{}, err
	}
	defer file.Close()
	if err := verifyOwnedWorkerLogFile(file, record.Path); err != nil {
		return WorkerLogPage{}, err
	}
	info, err := file.Stat()
	if err != nil {
		return WorkerLogPage{}, err
	}
	if offset > info.Size() {
		return WorkerLogPage{}, errors.New("offset is beyond the end of the worker log")
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return WorkerLogPage{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return WorkerLogPage{}, err
	}
	next := offset + int64(len(data))
	return WorkerLogPage{WorkerID: workerID, Offset: offset, Limit: limit, NextOffset: next, EOF: workerLogTerminal(worker.State) && next >= info.Size(), Data: string(data)}, nil
}

func workerLogTerminal(state string) bool {
	return state == ManualWorkerStateSucceeded || state == ManualWorkerStateFailed || state == ManualWorkerStateCancelled || state == ManualWorkerStateUnknown || state == ManualWorkerStateNeedsAttention
}

func workerLogPersistenceError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("worker log persistence failed: %w", err)
}

func appendWorkerResultLogs(s *Service, workerID uint, result proactive.ProcessResult) error {
	var errs []error
	for _, value := range []string{result.Stdout, result.Stderr} {
		if err := s.appendWorkerLog(workerID, value); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) launchWorker(workerID uint) {
	ctx := s.workerCtx
	if ctx == nil {
		return
	}
	workerCtx, cancel := context.WithCancel(ctx)
	s.workerWG.Add(1)
	go func() {
		defer s.workerWG.Done()
		defer cancel()
		s.processWorker(workerCtx, workerID)
	}()
}

func (s *Service) processWorker(ctx context.Context, workerID uint) {
	worker, attempt, err := s.claimWorker(workerID)
	if err != nil {
		return
	}
	owned := &ownedWorkerProcess{attemptID: attempt.ID, lease: attempt.LeaseToken, cancel: func() {}}
	s.workerMu.Lock()
	if s.workerProcesses == nil {
		s.workerProcesses = make(map[uint]*ownedWorkerProcess)
	}
	s.workerProcesses[workerID] = owned
	s.workerMu.Unlock()
	defer func() {
		s.workerMu.Lock()
		if s.workerProcesses[workerID] == owned {
			delete(s.workerProcesses, workerID)
		}
		s.workerMu.Unlock()
	}()

	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	defer heartbeatCancel()
	heartbeatErr := s.startWorkerLeaseHeartbeat(heartbeatCtx, worker.ID, attempt.ID, attempt.LeaseToken)
	defer heartbeatErr.stop()
	if s.Runner == nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateFailed, errors.New("manual worker runner is unavailable"), nil)
		return
	}
	if err := validateManualWorkerConfig(worker.ConfigIdentity); err != nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateFailed, err, nil)
		return
	}
	if s.isWorkerCancelRequested(worker.ID, attempt.ID) {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateCancelled, errors.New("worker cancellation requested before launch"), nil)
		return
	}
	files, err := s.currentWorkerFiles(worker.ID)
	if err != nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateFailed, err, nil)
		return
	}
	if err := s.verifyExistingRemote(ctx, worker, attempt, files); err != nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateUnknown, err, nil)
		return
	}
	files, err = s.incompleteWorkerFiles(worker.ID)
	if err != nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateFailed, err, nil)
		return
	}
	if len(files) == 0 {
		if err := s.validateWorkerSourceSnapshot(worker); err != nil {
			_ = s.finishWorker(worker, attempt, ManualWorkerStateUnknown, err, nil)
			return
		}
		_ = s.finishWorker(worker, attempt, ManualWorkerStateSucceeded, nil, intPointerValue(0))
		return
	}
	var run ManualTransferRun
	if err := s.DB.First(&run, worker.RunID).Error; err != nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateFailed, err, nil)
		return
	}
	root, err := quota.OpenSourceRoot(run.SourcePath)
	if err != nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateFailed, err, nil)
		return
	}
	defer root.Close()
	if root.Device != run.SourceRootDevice || root.Inode != run.SourceRootInode {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateUnknown, errors.New("source root identity changed"), nil)
		return
	}
	if run.TransferMode == models.TransferModeMove {
		s.processMoveWorker(ctx, worker, attempt, run, root, heartbeatErr)
		return
	}
	stageBase := s.stageDirectory()
	if err := ensureWorkerStageBase(stageBase); err != nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateFailed, err, nil)
		return
	}
	stage, err := quota.PrepareStage(stageBase, worker.ID, randomWorkerToken(), attempt.LeaseToken)
	if err != nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateFailed, err, nil)
		return
	}
	defer func() { _ = stage.Cleanup() }()
	manifestFiles := make([]models.RotationQuotaBatchFile, 0, len(files))
	for _, file := range files {
		if err := s.DB.Model(&ManualRunWorker{}).Where("id = ? AND current_attempt_id = ?", worker.ID, attempt.ID).Update("current_relative_path", file.RelativePath).Error; err != nil {
			_ = s.finishWorker(worker, attempt, ManualWorkerStateNeedsAttention, fmt.Errorf("worker progress persistence failed: %w", err), nil)
			return
		}
		snapshot := quota.LocalSnapshot{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode, RootDevice: root.Device, RootInode: root.Inode, SnapshotKey: file.SnapshotKey}
		ok, validateErr := root.Validate(snapshot)
		if validateErr != nil || !ok {
			if validateErr == nil {
				validateErr = errors.New("source file changed")
			}
			_ = s.finishWorker(worker, attempt, ManualWorkerStateUnknown, validateErr, nil)
			return
		}
		opened, openErr := root.OpenValidated(snapshot)
		if openErr != nil {
			_ = s.finishWorker(worker, attempt, ManualWorkerStateUnknown, openErr, nil)
			return
		}
		if stageErr := stage.Snapshot(snapshot, opened); stageErr != nil {
			_ = opened.Close()
			_ = s.finishWorker(worker, attempt, ManualWorkerStateUnknown, stageErr, nil)
			return
		}
		_ = opened.Close()
		manifestFiles = append(manifestFiles, models.RotationQuotaBatchFile{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes})
	}
	manifestPath, manifestHash, _, err := (proactive.ManifestWriter{}).Write(s.manifestDirectory(), models.RotationQuotaBatch{ID: worker.ID, OwnerToken: attempt.LeaseToken}, manifestFiles)
	if err != nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateFailed, err, nil)
		return
	}
	if err := s.persistWorkerManifest(worker.ID, attempt.ID, attempt.LeaseToken, manifestPath, manifestHash); err != nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateUnknown, err, nil)
		return
	}
	for _, file := range files {
		if err := stage.Validate(quota.LocalSnapshot{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, Device: file.Device, Inode: file.Inode}); err != nil {
			_ = s.finishWorker(worker, attempt, ManualWorkerStateUnknown, err, nil)
			return
		}
	}
	if s.isWorkerCancelRequested(worker.ID, attempt.ID) {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateCancelled, errors.New("worker cancellation requested before process start"), nil)
		return
	}
	processCtx, processCancel := context.WithCancel(ctx)
	defer processCancel()
	owned.cancel = processCancel
	redactors := newWorkerStreamRedactors()
	progressState := newWorkerProgressState(worker)
	var streamErrorMu sync.Mutex
	var streamError error
	recordStreamError := func(err error) {
		if err == nil {
			return
		}
		streamErrorMu.Lock()
		streamError = errors.Join(streamError, err)
		streamErrorMu.Unlock()
		processCancel()
	}
	readStreamError := func() error {
		streamErrorMu.Lock()
		defer streamErrorMu.Unlock()
		return streamError
	}
	launchLock := s.workerLaunchLock(worker.ID)
	launchLock.Lock()
	if err := s.ensureWorkerLaunchAllowed(worker.ID, attempt.ID, attempt.LeaseToken); err != nil {
		launchLock.Unlock()
		state := ManualWorkerStateNeedsAttention
		if errors.Is(err, ErrWorkerConflict) && s.isWorkerCancelRequested(worker.ID, attempt.ID) {
			state = ManualWorkerStateCancelled
		}
		_ = s.finishWorker(worker, attempt, state, fmt.Errorf("worker launch CAS rejected: %w", err), nil)
		return
	}
	process, err := s.Runner.StartCopy(processCtx, proactive.CopySpec{ConfigPath: worker.ConfigIdentity, ManifestPath: manifestPath, SourceRoot: stage.File(), DestinationRemote: worker.RemoteName, DestinationPath: run.DestinationPath, Transfers: 4, OutputSink: func(chunk proactive.ProcessOutputChunk) {
		recordStreamError(s.consumeWorkerOutput(worker.ID, worker.AssignedBytes, redactors, progressState, chunk.Stream, chunk.Data))
	}, ProgressSink: func(progress proactive.ProcessProgress) {
		recordStreamError(s.consumeWorkerProgress(worker.ID, attempt.ID, progressState, progress))
	}})
	if err != nil {
		launchLock.Unlock()
		var logErr error
		var identityErr *proactive.StartedProcessIdentityError
		if errors.As(err, &identityErr) {
			logErr = appendWorkerResultLogs(s, worker.ID, identityErr.Result)
		}
		logErr = errors.Join(logErr, s.appendWorkerLog(worker.ID, "copy start failed: "+err.Error()))
		state := ManualWorkerStateFailed
		cause := err
		if logErr != nil {
			state = ManualWorkerStateNeedsAttention
			cause = errors.Join(cause, workerLogPersistenceError(logErr))
		}
		_ = s.finishWorker(worker, attempt, state, cause, nil)
		return
	}
	owned.process = process
	if process == nil || process.PID() <= 0 || process.StartToken() == "" {
		launchLock.Unlock()
		cleanup := s.stopAndDrainWorkerProcess(ctx, worker, attempt, process, heartbeatErr, redactors, progressState, errors.New("copy process identity is unavailable"), readStreamError)
		if cleanup.stopErr != nil {
			_ = s.markWorkerNeedsAttention(worker.ID, attempt.ID, errors.Join(cleanup.err, fmt.Errorf("copy process identity could not be stopped safely: %w", cleanup.stopErr)))
			return
		}
		_ = s.finishWorker(worker, attempt, ManualWorkerStateUnknown, cleanup.err, intPointerValue(cleanup.result.ExitCode))
		return
	}
	persistProcess := s.persistWorkerProcess
	if s.PersistWorkerProcessFunc != nil {
		persistProcess = s.PersistWorkerProcessFunc
	}
	if persistErr := persistProcess(worker.ID, attempt.ID, attempt.LeaseToken, process); persistErr != nil {
		// Retry with the real persistence path so an injected or transient error
		// cannot leave a live child without durable ownership metadata.
		if fallbackErr := s.persistWorkerProcess(worker.ID, attempt.ID, attempt.LeaseToken, process); fallbackErr != nil {
			persistErr = errors.Join(persistErr, fmt.Errorf("process ownership fallback persistence failed: %w", fallbackErr))
		}
		launchLock.Unlock()
		cleanup := s.stopAndDrainWorkerProcess(ctx, worker, attempt, process, heartbeatErr, redactors, progressState, persistErr, readStreamError)
		if cleanup.stopErr != nil {
			_ = s.markWorkerNeedsAttention(worker.ID, attempt.ID, errors.Join(cleanup.err, fmt.Errorf("process identity persistence failed and process could not be stopped safely: %w", cleanup.stopErr)))
			return
		}
		_ = s.finishWorker(worker, attempt, ManualWorkerStateUnknown, cleanup.err, intPointerValue(cleanup.result.ExitCode))
		return
	}
	launchLock.Unlock()
	if s.isWorkerCancelRequested(worker.ID, attempt.ID) {
		if stopErr := s.stopPersistedWorkerProcess(worker.ID, attempt.ID, process); stopErr != nil {
			_ = s.markWorkerNeedsAttention(worker.ID, attempt.ID, fmt.Errorf("worker cancellation could not stop the verified process: %w", stopErr))
			return
		}
		result, waitErr, heartbeatFailure, waitStopErr := s.waitWorkerProcess(ctx, worker, attempt, process, heartbeatErr, redactors, progressState)
		if heartbeatFailure == nil {
			heartbeatFailure = readStreamError()
		}
		var resultLogErr error
		if _, streaming := process.(proactive.ProcessOutput); !streaming {
			resultLogErr = appendWorkerResultLogs(s, worker.ID, result)
		}
		if waitStopErr != nil {
			_ = s.markWorkerNeedsAttention(worker.ID, attempt.ID, errors.Join(heartbeatFailure, workerLogPersistenceError(resultLogErr), waitErr, fmt.Errorf("worker cancellation process wait was not safely reconciled: %w", waitStopErr)))
			return
		}
		var reconcileErr error
		if heartbeatFailure == nil {
			reconcileErr = s.reconcileWorkerFiles(ctx, worker, attempt)
		}
		if heartbeatFailure != nil {
			reconcileErr = heartbeatFailure
		}
		state := ManualWorkerStateCancelled
		if heartbeatFailure != nil {
			state = ManualWorkerStateNeedsAttention
			reconcileErr = heartbeatFailure
		}
		if resultLogErr != nil {
			state = ManualWorkerStateNeedsAttention
			reconcileErr = errors.Join(reconcileErr, workerLogPersistenceError(resultLogErr))
		}
		_ = s.finishWorker(worker, attempt, state, reconcileErr, &result.ExitCode)
		_ = waitErr
		return
	}
	result, waitErr, heartbeatFailure, stopErr := s.waitWorkerProcess(ctx, worker, attempt, process, heartbeatErr, redactors, progressState)
	if heartbeatFailure == nil {
		heartbeatFailure = readStreamError()
	}
	var resultLogErr error
	if _, streaming := process.(proactive.ProcessOutput); !streaming {
		resultLogErr = appendWorkerResultLogs(s, worker.ID, result)
	}
	if stopErr != nil {
		_ = s.markWorkerNeedsAttention(worker.ID, attempt.ID, errors.Join(heartbeatFailure, workerLogPersistenceError(resultLogErr), waitErr, fmt.Errorf("worker process could not be stopped safely: %w", stopErr)))
		return
	}
	state := ManualWorkerStateFailed
	if s.isWorkerCancelRequested(worker.ID, attempt.ID) {
		state = ManualWorkerStateCancelled
	} else if waitErr == nil && result.ExitCode == 0 {
		state = ManualWorkerStateSucceeded
	}
	if heartbeatFailure != nil {
		state = ManualWorkerStateNeedsAttention
		err = errors.Join(heartbeatFailure, workerLogPersistenceError(resultLogErr))
	} else if resultLogErr != nil {
		state = ManualWorkerStateNeedsAttention
		err = workerLogPersistenceError(resultLogErr)
	} else if waitErr != nil {
		err = waitErr
	} else if result.ExitCode != 0 {
		err = fmt.Errorf("copy exited with code %d", result.ExitCode)
	}
	if reconcileErr := s.reconcileWorkerFiles(ctx, worker, attempt); reconcileErr != nil {
		if state == ManualWorkerStateSucceeded {
			state = ManualWorkerStateUnknown
		}
		if err == nil {
			err = reconcileErr
		}
	}
	if state == ManualWorkerStateSucceeded {
		var remaining int64
		s.DB.Model(&ManualWorkerFile{}).Where("worker_id = ? AND state <> ?", worker.ID, ManualWorkerFileStateVerified).Count(&remaining)
		if remaining != 0 {
			state = ManualWorkerStateUnknown
			if err == nil {
				err = errors.New("remote verification did not confirm every assigned file")
			}
		}
	}
	_ = s.finishWorker(worker, attempt, state, err, &result.ExitCode)
}

func (s *Service) processMoveWorker(ctx context.Context, worker ManualRunWorker, attempt ManualWorkerAttempt, run ManualTransferRun, root *quota.SourceRootHandle, heartbeat *workerLeaseHeartbeat) {
	moveRunner, ok := s.Runner.(proactive.MoveRunner)
	if !ok {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateFailed, errors.New("manual move runner is unavailable"), nil)
		return
	}
	if err := heartbeat.err(); err != nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateNeedsAttention, err, nil)
		return
	}
	if s.isWorkerCancelRequested(worker.ID, attempt.ID) {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateCancelled, errors.New("worker cancellation requested before move handoff"), nil)
		return
	}

	files, err := s.currentWorkerFiles(worker.ID)
	if err != nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateFailed, err, nil)
		return
	}
	manifestFiles := make([]models.RotationQuotaBatchFile, 0, len(files))
	for _, file := range files {
		manifestFiles = append(manifestFiles, models.RotationQuotaBatchFile{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode})
	}
	manifestPath, manifestHash, _, err := (proactive.ManifestWriter{}).Write(s.manifestDirectory(), models.RotationQuotaBatch{ID: worker.ID, OwnerToken: manualWorkerMoveOwner(worker.ID)}, manifestFiles)
	if err != nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateFailed, err, nil)
		return
	}
	if err := s.persistWorkerManifest(worker.ID, attempt.ID, attempt.LeaseToken, manifestPath, manifestHash); err != nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateNeedsAttention, err, nil)
		return
	}

	for _, file := range files {
		snapshot := moveWorkerSnapshot(file, root)
		valid, validateErr := root.Validate(snapshot)
		if validateErr != nil || !valid {
			if validateErr == nil {
				validateErr = fmt.Errorf("source file changed: %s", file.RelativePath)
			}
			_ = s.finishWorker(worker, attempt, ManualWorkerStateFailed, validateErr, nil)
			return
		}
	}

	quarantine, err := quota.PrepareMoveQuarantine(root, worker.ID, manualWorkerMoveOwner(worker.ID))
	if err != nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateFailed, err, nil)
		return
	}
	defer quarantine.Close()
	if _, _, err := quarantine.Identity(); err != nil {
		_ = s.finishWorker(worker, attempt, ManualWorkerStateFailed, err, nil)
		return
	}

	finishBeforeStart := func(state string, cause error) {
		restoreErr := restoreMoveWorkerFiles(root, quarantine, files)
		if restoreErr != nil {
			cause = errors.Join(cause, restoreErr)
			state = ManualWorkerStateNeedsAttention
		}
		_ = s.finishWorker(worker, attempt, state, cause, nil)
	}
	for _, file := range files {
		if err := heartbeat.err(); err != nil {
			finishBeforeStart(ManualWorkerStateFailed, err)
			return
		}
		if s.isWorkerCancelRequested(worker.ID, attempt.ID) {
			finishBeforeStart(ManualWorkerStateCancelled, errors.New("worker cancellation requested before move process start"))
			return
		}
		snapshot := moveWorkerSnapshot(file, root)
		valid, validateErr := root.Validate(snapshot)
		if validateErr != nil || !valid {
			if validateErr == nil {
				validateErr = fmt.Errorf("source file changed: %s", file.RelativePath)
			}
			finishBeforeStart(ManualWorkerStateFailed, validateErr)
			return
		}
		if _, _, err := quarantine.Move(file.RelativePath, snapshot); err != nil {
			finishBeforeStart(ManualWorkerStateFailed, fmt.Errorf("move handoff failed: %w", err))
			return
		}
	}
	if err := heartbeat.err(); err != nil {
		finishBeforeStart(ManualWorkerStateFailed, err)
		return
	}
	if s.isWorkerCancelRequested(worker.ID, attempt.ID) {
		finishBeforeStart(ManualWorkerStateCancelled, errors.New("worker cancellation requested before move process start"))
		return
	}

	launchLock := s.workerLaunchLock(worker.ID)
	launchLock.Lock()
	if err := s.ensureWorkerLaunchAllowed(worker.ID, attempt.ID, attempt.LeaseToken); err != nil {
		launchLock.Unlock()
		state := ManualWorkerStateNeedsAttention
		if errors.Is(err, ErrWorkerConflict) && s.isWorkerCancelRequested(worker.ID, attempt.ID) {
			state = ManualWorkerStateCancelled
		}
		finishBeforeStart(state, fmt.Errorf("worker launch CAS rejected: %w", err))
		return
	}
	process, err := moveRunner.StartMove(ctx, proactive.MoveSpec{ConfigPath: worker.ConfigIdentity, ManifestPath: manifestPath, SourceRoot: quarantine.File(), DestinationRemote: worker.RemoteName, DestinationPath: run.DestinationPath, Transfers: 4})
	if err != nil {
		launchLock.Unlock()
		var identityErr *proactive.StartedProcessIdentityError
		if errors.As(err, &identityErr) {
			logErr := appendWorkerResultLogs(s, worker.ID, identityErr.Result)
			logErr = errors.Join(logErr, s.appendWorkerLog(worker.ID, "move start identity failed: "+err.Error()))
			_ = s.markWorkerNeedsAttention(worker.ID, attempt.ID, errors.Join(err, workerLogPersistenceError(logErr)))
			return
		}
		logErr := s.appendWorkerLog(worker.ID, "move start failed: "+err.Error())
		restoreErr := restoreMoveWorkerFiles(root, quarantine, files)
		if restoreErr != nil {
			_ = s.finishWorker(worker, attempt, ManualWorkerStateNeedsAttention, errors.Join(err, logErr, restoreErr), nil)
			return
		}
		state := ManualWorkerStateFailed
		if logErr != nil {
			state = ManualWorkerStateNeedsAttention
		}
		_ = s.finishWorker(worker, attempt, state, errors.Join(err, logErr), nil)
		return
	}
	s.workerMu.Lock()
	owned := s.workerProcesses[worker.ID]
	if owned != nil {
		owned.process = process
	}
	s.workerMu.Unlock()
	if process == nil || process.PID() <= 0 || process.StartToken() == "" {
		launchLock.Unlock()
		cleanup := s.stopAndDrainWorkerProcess(ctx, worker, attempt, process, heartbeat, newWorkerStreamRedactors(), newWorkerProgressState(worker), errors.New("move process identity is unavailable"), nil)
		if cleanup.stopErr != nil {
			_ = s.markWorkerNeedsAttention(worker.ID, attempt.ID, errors.Join(cleanup.err, fmt.Errorf("move process identity could not be stopped safely: %w", cleanup.stopErr)))
			return
		}
		_ = s.finishWorker(worker, attempt, ManualWorkerStateNeedsAttention, cleanup.err, intPointerValue(cleanup.result.ExitCode))
		return
	}
	persistProcess := s.persistWorkerProcess
	if s.PersistWorkerProcessFunc != nil {
		persistProcess = s.PersistWorkerProcessFunc
	}
	if persistErr := persistProcess(worker.ID, attempt.ID, attempt.LeaseToken, process); persistErr != nil {
		if fallbackErr := s.persistWorkerProcess(worker.ID, attempt.ID, attempt.LeaseToken, process); fallbackErr != nil {
			persistErr = errors.Join(persistErr, fmt.Errorf("process ownership fallback persistence failed: %w", fallbackErr))
		}
		launchLock.Unlock()
		cleanup := s.stopAndDrainWorkerProcess(ctx, worker, attempt, process, heartbeat, newWorkerStreamRedactors(), newWorkerProgressState(worker), persistErr, nil)
		if cleanup.stopErr != nil {
			_ = s.markWorkerNeedsAttention(worker.ID, attempt.ID, errors.Join(cleanup.err, fmt.Errorf("move process identity persistence failed and process could not be stopped safely: %w", cleanup.stopErr)))
			return
		}
		_ = s.finishWorker(worker, attempt, ManualWorkerStateNeedsAttention, cleanup.err, intPointerValue(cleanup.result.ExitCode))
		return
	}
	launchLock.Unlock()

	result, waitErr, heartbeatFailure, stopErr := s.waitWorkerProcess(ctx, worker, attempt, process, heartbeat, newWorkerStreamRedactors(), newWorkerProgressState(worker))
	var resultLogErr error
	if _, streaming := process.(proactive.ProcessOutput); !streaming {
		resultLogErr = appendWorkerResultLogs(s, worker.ID, result)
	}
	if stopErr != nil {
		_ = s.markWorkerNeedsAttention(worker.ID, attempt.ID, errors.Join(heartbeatFailure, workerLogPersistenceError(resultLogErr), waitErr, fmt.Errorf("move process could not be stopped safely: %w", stopErr)))
		return
	}
	reconcileErr := s.reconcileMoveWorkerFiles(worker, attempt, root, quarantine)
	state := ManualWorkerStateSucceeded
	var cause error
	if heartbeatFailure != nil {
		state = ManualWorkerStateNeedsAttention
		cause = heartbeatFailure
	} else if waitErr != nil {
		state = ManualWorkerStateNeedsAttention
		cause = waitErr
	} else if result.ExitCode != 0 {
		state = ManualWorkerStateNeedsAttention
		cause = fmt.Errorf("move exited with code %d", result.ExitCode)
	}
	if reconcileErr != nil {
		state = ManualWorkerStateNeedsAttention
		cause = errors.Join(cause, reconcileErr)
	}
	if resultLogErr != nil {
		state = ManualWorkerStateNeedsAttention
		cause = errors.Join(cause, workerLogPersistenceError(resultLogErr))
	}
	if state == ManualWorkerStateSucceeded {
		paths := make([]string, 0, len(files))
		for _, file := range files {
			paths = append(paths, file.RelativePath)
		}
		if err := root.RemoveEmptyParents(paths); err != nil {
			state = ManualWorkerStateNeedsAttention
			cause = err
		}
	}
	_ = s.finishWorker(worker, attempt, state, cause, &result.ExitCode)
}

func moveWorkerSnapshot(file ManualWorkerFile, root *quota.SourceRootHandle) quota.LocalSnapshot {
	return quota.LocalSnapshot{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode, RootDevice: root.Device, RootInode: root.Inode, SnapshotKey: file.SnapshotKey}
}

func restoreMoveWorkerFiles(root *quota.SourceRootHandle, quarantine *quota.MoveQuarantine, files []ManualWorkerFile) error {
	var firstErr error
	for _, file := range files {
		snapshot := moveWorkerSnapshot(file, root)
		quarantined, _, _, quarantineErr := quarantine.Present(file.RelativePath, snapshot)
		if quarantineErr != nil {
			firstErr = errors.Join(firstErr, fmt.Errorf("move handoff discovery failed for %q: %w", file.RelativePath, quarantineErr))
			continue
		}
		original, validateErr := root.Validate(snapshot)
		if validateErr != nil {
			firstErr = errors.Join(firstErr, fmt.Errorf("source recovery failed for %q: %w", file.RelativePath, validateErr))
			continue
		}
		if quarantined && original {
			firstErr = errors.Join(firstErr, fmt.Errorf("move handoff is ambiguous for %q: both source and quarantine are present", file.RelativePath))
			continue
		}
		if quarantined {
			if err := quarantine.Restore(file.RelativePath, snapshot); err != nil {
				firstErr = errors.Join(firstErr, fmt.Errorf("move pre-start restore failed for %q: %w", file.RelativePath, err))
				continue
			}
			original, validateErr = root.Validate(snapshot)
			if validateErr != nil || !original {
				if validateErr == nil {
					validateErr = errors.New("restored source identity could not be verified")
				}
				firstErr = errors.Join(firstErr, fmt.Errorf("move pre-start restore verification failed for %q: %w", file.RelativePath, validateErr))
				continue
			}
			if present, _, _, presentErr := quarantine.Present(file.RelativePath, snapshot); presentErr != nil || present {
				if presentErr == nil {
					presentErr = errors.New("restored file remains in quarantine")
				}
				firstErr = errors.Join(firstErr, fmt.Errorf("move pre-start restore left ambiguous evidence for %q: %w", file.RelativePath, presentErr))
			}
			continue
		}
		if !original {
			firstErr = errors.Join(firstErr, fmt.Errorf("move handoff is ambiguous for %q: file is absent from source and quarantine", file.RelativePath))
		}
	}
	return firstErr
}

func (s *Service) reconcileMoveWorkerFiles(worker ManualRunWorker, attempt ManualWorkerAttempt, root *quota.SourceRootHandle, quarantine *quota.MoveQuarantine) error {
	files, err := s.incompleteWorkerFiles(worker.ID)
	if err != nil {
		return err
	}
	var firstErr error
	for _, file := range files {
		snapshot := moveWorkerSnapshot(file, root)
		quarantined, _, _, presentErr := quarantine.Present(file.RelativePath, snapshot)
		if presentErr != nil {
			firstErr = errors.Join(firstErr, fmt.Errorf("move quarantine evidence failed for %q: %w", file.RelativePath, presentErr))
			continue
		}
		original, validateErr := root.Validate(snapshot)
		if validateErr != nil {
			firstErr = errors.Join(firstErr, fmt.Errorf("move source evidence failed for %q: %w", file.RelativePath, validateErr))
			continue
		}
		if quarantined || original {
			firstErr = errors.Join(firstErr, fmt.Errorf("move outcome is ambiguous for %q", file.RelativePath))
			continue
		}
		if err := s.markWorkerFileVerified(file.ID, worker.ID, attempt.ID); err != nil {
			firstErr = errors.Join(firstErr, err)
		}
	}
	return firstErr
}

func manualWorkerMoveOwner(workerID uint) string {
	return fingerprintBytes(fmt.Sprintf("manual-move-worker-%d", workerID))[:48]
}

func (s *Service) waitWorkerProcess(ctx context.Context, worker ManualRunWorker, attempt ManualWorkerAttempt, process proactive.ProcessHandle, heartbeat *workerLeaseHeartbeat, redactors *workerStreamRedactors, progressState *workerProgressState) (proactive.ProcessResult, error, error, error) {
	type waited struct {
		result proactive.ProcessResult
		err    error
	}
	waitCh := make(chan waited, 1)
	go func() {
		result, err := process.Wait()
		waitCh <- waited{result: result, err: err}
	}()
	var outputCh <-chan proactive.ProcessOutputChunk
	if streamer, ok := process.(proactive.ProcessOutput); ok {
		outputCh = streamer.Output()
	}
	var progressCh <-chan proactive.ProcessProgress
	if progressor, ok := process.(proactive.ProcessProgressor); ok {
		progressCh = progressor.Progress()
	}
	var result proactive.ProcessResult
	var waitErr error
	var heartbeatFailure error
	var stopErr error
	addFailure := func(err error) {
		if err != nil {
			heartbeatFailure = errors.Join(heartbeatFailure, err)
		}
	}
	requestStop := func() {
		if err := s.stopPersistedWorkerProcess(worker.ID, attempt.ID, process); err != nil {
			stopErr = errors.Join(stopErr, err)
		}
	}
	heartbeatErrCh := (<-chan error)(nil)
	if heartbeat != nil {
		heartbeatErrCh = heartbeat.errCh
	}
	waiting := true
	for waiting || outputCh != nil || progressCh != nil {
		select {
		case waitedResult := <-waitCh:
			result, waitErr, waiting = waitedResult.result, waitedResult.err, false
		case chunk, ok := <-outputCh:
			if !ok {
				outputCh = nil
				continue
			}
			if err := s.consumeWorkerOutput(worker.ID, worker.AssignedBytes, redactors, progressState, chunk.Stream, chunk.Data); err != nil {
				addFailure(err)
				requestStop()
			}
		case progress, ok := <-progressCh:
			if !ok {
				progressCh = nil
				continue
			}
			if err := s.consumeWorkerProgress(worker.ID, attempt.ID, progressState, progress); err != nil {
				addFailure(err)
				requestStop()
			}
		case heartbeatErr := <-heartbeatErrCh:
			addFailure(heartbeatErr)
			requestStop()
		}
		if !waiting && outputCh == nil && progressCh == nil {
			break
		}
	}
	if heartbeat != nil {
		if err := heartbeat.err(); err != nil {
			addFailure(err)
			requestStop()
		}
	}
	if redactors != nil {
		for _, redactor := range []*workerLogRedactor{redactors.stdout, redactors.stderr} {
			if tail := redactor.Flush(); tail != "" {
				if err := s.appendWorkerLog(worker.ID, tail); err != nil {
					addFailure(workerLogPersistenceError(err))
				}
			}
		}
	}
	return result, waitErr, heartbeatFailure, stopErr
}

type workerProcessCleanup struct {
	result  proactive.ProcessResult
	err     error
	stopErr error
}

// stopAndDrainWorkerProcess handles the interval after StartCopy succeeds but
// before process identity persistence succeeds. Every stream still has to be
// consumed and every redactor tail has to be flushed before the worker can be
// terminalized.
func (s *Service) stopAndDrainWorkerProcess(ctx context.Context, worker ManualRunWorker, attempt ManualWorkerAttempt, process proactive.ProcessHandle, heartbeat *workerLeaseHeartbeat, redactors *workerStreamRedactors, progressState *workerProgressState, initialErr error, readStreamError func() error) workerProcessCleanup {
	cleanup := workerProcessCleanup{err: initialErr}
	readStreamsErr := func() error {
		if readStreamError == nil {
			return nil
		}
		return readStreamError()
	}
	if process == nil {
		cleanup.err = errors.Join(cleanup.err, readStreamsErr())
		return cleanup
	}
	identityPersisted := false
	var persistedWorker ManualRunWorker
	var persistedAttempt ManualWorkerAttempt
	if workerErr := s.DB.First(&persistedWorker, worker.ID).Error; workerErr == nil {
		if attemptErr := s.DB.First(&persistedAttempt, attempt.ID).Error; attemptErr == nil {
			identityPersisted = persistedWorker.ProcessID > 0 || persistedWorker.ProcessStartToken != "" || persistedAttempt.ProcessID > 0 || persistedAttempt.ProcessStartToken != ""
		}
	}
	if identityPersisted {
		cleanup.stopErr = s.stopPersistedWorkerProcess(worker.ID, attempt.ID, process)
	} else {
		cleanup.stopErr = process.Stop()
	}
	if errors.Is(cleanup.stopErr, os.ErrProcessDone) {
		cleanup.stopErr = nil
	}
	var waitErr, drainErr, drainStopErr error
	cleanup.result, waitErr, drainErr, drainStopErr = s.waitWorkerProcess(ctx, worker, attempt, process, heartbeat, redactors, progressState)
	cleanup.stopErr = errors.Join(cleanup.stopErr, drainStopErr)
	cleanup.err = errors.Join(cleanup.err, waitErr, drainErr, readStreamsErr())
	if _, streaming := process.(proactive.ProcessOutput); !streaming {
		cleanup.err = errors.Join(cleanup.err, workerLogPersistenceError(appendWorkerResultLogs(s, worker.ID, cleanup.result)))
	}
	return cleanup
}

func (s *Service) consumeWorkerOutput(workerID uint, assignedBytes int64, redactors *workerStreamRedactors, progressState *workerProgressState, stream, value string) error {
	redactor := redactors.forStream(stream)
	var errs []error
	if sanitized := redactor.Feed(value); sanitized != "" {
		if err := s.appendWorkerLog(workerID, sanitized); err != nil {
			errs = append(errs, workerLogPersistenceError(err))
		}
	}
	if progress, ok := workerProgressFromOutput(value, assignedBytes); ok {
		errs = append(errs, s.consumeWorkerProgress(workerID, 0, progressState, progress))
	}
	return errors.Join(errs...)
}

func (s *Service) consumeWorkerProgress(workerID, attemptID uint, progressState *workerProgressState, progress proactive.ProcessProgress) error {
	if progressState == nil {
		progressState = &workerProgressState{}
	}
	persistProgress := s.persistWorkerProgress
	if s.PersistWorkerProgressFunc != nil {
		persistProgress = s.PersistWorkerProgressFunc
	}
	if err := persistProgress(workerID, attemptID, progressState.merge(progress)); err != nil {
		return fmt.Errorf("worker progress persistence failed: %w", err)
	}
	return nil
}

func (s *Service) workerLaunchLock(workerID uint) *sync.Mutex {
	s.launchMu.Lock()
	defer s.launchMu.Unlock()
	if s.launchLocks == nil {
		s.launchLocks = make(map[uint]*sync.Mutex)
	}
	if lock := s.launchLocks[workerID]; lock != nil {
		return lock
	}
	lock := &sync.Mutex{}
	s.launchLocks[workerID] = lock
	return lock
}

func (s *Service) ensureWorkerLaunchAllowed(workerID, attemptID uint, lease string) error {
	return retryManualSQLite(func() error {
		return s.DB.Transaction(func(tx *gorm.DB) error {
			var worker ManualRunWorker
			if err := tx.Where("id = ? AND current_attempt_id = ? AND lease_token = ? AND state = ? AND cancel_requested = ?", workerID, attemptID, lease, ManualWorkerStateStarting, false).First(&worker).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrWorkerConflict
				}
				return err
			}
			var attempt ManualWorkerAttempt
			if err := tx.Where("id = ? AND worker_id = ? AND lease_token = ? AND state = ? AND cancel_requested = ?", attemptID, workerID, lease, ManualWorkerStateStarting, false).First(&attempt).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrWorkerConflict
				}
				return err
			}
			return nil
		})
	})
}

func (s *Service) stopPersistedWorkerProcess(workerID, attemptID uint, process proactive.ProcessHandle) error {
	var worker ManualRunWorker
	if err := s.DB.First(&worker, workerID).Error; err != nil {
		return err
	}
	var attempt ManualWorkerAttempt
	if err := s.DB.First(&attempt, attemptID).Error; err != nil {
		return err
	}
	pid, token := worker.ProcessID, worker.ProcessStartToken
	if worker.ProcessID > 0 && worker.ProcessStartToken != "" && attempt.ProcessID > 0 && attempt.ProcessStartToken != "" && (worker.ProcessID != attempt.ProcessID || worker.ProcessStartToken != attempt.ProcessStartToken) {
		return errors.New("worker and attempt process identities disagree")
	}
	if pid <= 0 || token == "" {
		pid, token = attempt.ProcessID, attempt.ProcessStartToken
	}
	if pid <= 0 && token == "" && (worker.State == ManualWorkerStateNeedsAttention || attempt.State == ManualWorkerStateNeedsAttention) && (worker.LeaseToken != "" || attempt.LeaseToken != "") {
		return errors.New("manual recovery requires durable worker process identity")
	}
	if pid > 0 || token != "" {
		if pid <= 0 || token == "" {
			return errors.New("persisted worker process identity is incomplete")
		}
		inspector := s.processInspector()
		status, err := inspector.Inspect(pid, token)
		if err != nil {
			return err
		}
		if status.Alive {
			if !status.Confirmed {
				return errors.New("persisted worker process identity no longer matches")
			}
			if err := inspector.StopVerified(pid, token); err != nil {
				return err
			}
			status, err = inspector.Inspect(pid, token)
			if err != nil {
				return err
			}
			if status.Alive {
				return errors.New("verified worker process remained alive after stop")
			}
		}
	}
	if process != nil {
		if err := process.Stop(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
	}
	return nil
}

func workerProgressFromOutput(value string, assignedBytes int64) (proactive.ProcessProgress, bool) {
	percentIndex := strings.Index(value, "%")
	if percentIndex <= 0 || assignedBytes <= 0 {
		return proactive.ProcessProgress{}, false
	}
	start := percentIndex - 1
	for start >= 0 && ((value[start] >= '0' && value[start] <= '9') || value[start] == '.') {
		start--
	}
	percent, err := strconv.ParseFloat(strings.TrimSpace(value[start+1:percentIndex]), 64)
	if err != nil || percent < 0 || percent > 100 {
		return proactive.ProcessProgress{}, false
	}
	progress := proactive.ProcessProgress{CompletedBytes: int64(float64(assignedBytes) * percent / 100), ProgressPercent: percent, SpeedBytesPerSecond: parseWorkerSpeed(value), RelativePath: parseWorkerPath(value, percentIndex)}
	if transferred := strings.Index(strings.ToLower(value), "transferred:"); transferred >= 0 {
		fields := strings.Fields(value[transferred+len("transferred:"):])
		if len(fields) > 0 {
			countText := strings.TrimSpace(fields[0])
			if count, countErr := strconv.ParseInt(countText, 10, 64); countErr == nil && count >= 0 {
				progress.CompletedCount = count
			}
		}
	}
	return progress, true
}

func parseWorkerPath(value string, percentIndex int) string {
	prefix := strings.TrimSpace(value[:percentIndex])
	prefix = strings.TrimSpace(strings.TrimLeft(prefix, "*"))
	if colon := strings.LastIndex(prefix, ":"); colon >= 0 {
		prefix = strings.TrimSpace(prefix[:colon])
	}
	lower := strings.ToLower(prefix)
	if prefix == "" || lower == "checks" || lower == "elapsed time" || lower == "errors" || strings.Contains(lower, "transferred") {
		return ""
	}
	return prefix
}

func parseWorkerSpeed(value string) int64 {
	fields := strings.Fields(strings.ReplaceAll(value, ",", ""))
	for index, field := range fields {
		if !strings.HasSuffix(strings.ToLower(field), "/s") || index == 0 {
			continue
		}
		rate := strings.TrimSuffix(strings.ToLower(field), "/s")
		numberText := fields[index-1]
		unit := rate
		if parsed, err := strconv.ParseFloat(strings.TrimSuffix(rate, "/s"), 64); err == nil {
			numberText = strings.TrimSuffix(rate, "/s")
			unit = "b"
			_ = parsed
		}
		number, err := strconv.ParseFloat(numberText, 64)
		if err != nil || number < 0 {
			continue
		}
		multiplier := float64(1)
		switch unit {
		case "kb", "kib":
			multiplier = 1024
		case "mb", "mib":
			multiplier = 1024 * 1024
		case "gb", "gib":
			multiplier = 1024 * 1024 * 1024
		case "tb", "tib":
			multiplier = 1024 * 1024 * 1024 * 1024
		}
		return int64(number * multiplier)
	}
	return 0
}

func (s *Service) persistWorkerProgress(workerID, attemptID uint, progress proactive.ProcessProgress) error {
	if progress.CompletedCount < 0 || progress.CompletedBytes < 0 || progress.ProgressPercent < 0 || progress.ProgressPercent > 100 {
		return errors.New("invalid worker progress")
	}
	return retryManualSQLite(func() error {
		return s.DB.Transaction(func(tx *gorm.DB) error {
			var worker ManualRunWorker
			if err := tx.First(&worker, workerID).Error; err != nil {
				return err
			}
			if attemptID == 0 {
				attemptID = worker.CurrentAttemptID
			}
			updates := map[string]interface{}{"completed_count": progress.CompletedCount, "completed_bytes": progress.CompletedBytes, "speed_bytes_per_second": progress.SpeedBytesPerSecond, "progress_percent": progress.ProgressPercent, "last_progress_at": s.now()}
			if progress.RelativePath != "" {
				updates["current_relative_path"] = progress.RelativePath
			}
			if err := tx.Model(&ManualRunWorker{}).Where("id = ? AND current_attempt_id = ?", workerID, attemptID).Updates(updates).Error; err != nil {
				return err
			}
			if err := tx.Model(&ManualWorkerAttempt{}).Where("id = ? AND worker_id = ?", attemptID, workerID).Updates(map[string]interface{}{"completed_count": progress.CompletedCount, "completed_bytes": progress.CompletedBytes, "speed_bytes_per_second": progress.SpeedBytesPerSecond, "progress_percent": progress.ProgressPercent}).Error; err != nil {
				return err
			}
			return tx.Create(&ManualWorkerProgress{RunID: worker.RunID, WorkerID: workerID, AttemptID: attemptID, Sequence: nextWorkerProgressSequence(tx, workerID), State: ManualWorkerStateRunning, RelativePath: progress.RelativePath, CompletedCount: progress.CompletedCount, CompletedBytes: progress.CompletedBytes, SpeedBytesPerSecond: progress.SpeedBytesPerSecond, ProgressPercent: progress.ProgressPercent}).Error
		})
	})
}

func (s *Service) claimWorker(workerID uint) (ManualRunWorker, ManualWorkerAttempt, error) {
	var worker ManualRunWorker
	var attempt ManualWorkerAttempt
	token := randomWorkerToken()
	err := retryManualSQLite(func() error {
		return s.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.First(&worker, workerID).Error; err != nil {
				return err
			}
			if worker.State != ManualWorkerStatePending || worker.CurrentAttemptID == 0 {
				return ErrWorkerConflict
			}
			if err := tx.First(&attempt, worker.CurrentAttemptID).Error; err != nil {
				return err
			}
			if attempt.State != ManualWorkerStatePending {
				return ErrWorkerConflict
			}
			now := s.now()
			updated := tx.Model(&ManualRunWorker{}).Where("id = ? AND state = ? AND revision = ?", worker.ID, ManualWorkerStatePending, worker.Revision).Updates(map[string]interface{}{"state": ManualWorkerStateStarting, "lease_token": token, "lease_until": now.Add(s.workerLeaseDuration()), "revision": gorm.Expr("revision + 1"), "started_at": now, "last_error": ""})
			if updated.Error != nil || updated.RowsAffected != 1 {
				if updated.Error != nil {
					return updated.Error
				}
				return ErrWorkerConflict
			}
			updated = tx.Model(&ManualWorkerAttempt{}).Where("id = ? AND worker_id = ? AND state = ?", attempt.ID, worker.ID, ManualWorkerStatePending).Updates(map[string]interface{}{"state": ManualWorkerStateStarting, "lease_token": token, "lease_until": now.Add(s.workerLeaseDuration()), "started_at": now})
			if updated.Error != nil || updated.RowsAffected != 1 {
				if updated.Error != nil {
					return updated.Error
				}
				return ErrWorkerConflict
			}
			attempt.LeaseToken = token
			attempt.LeaseUntil = ptrTime(now.Add(s.workerLeaseDuration()))
			worker.LeaseToken = token
			worker.LeaseUntil = attempt.LeaseUntil
			return tx.Create(&ManualWorkerEvent{RunID: worker.RunID, WorkerID: worker.ID, AttemptID: attempt.ID, EventType: ManualWorkerEventStarted, FromState: ManualWorkerStatePending, ToState: ManualWorkerStateStarting, ActorIdentity: "manual-transfer-worker", ActorType: "system", Details: "worker claimed its attempt"}).Error
		})
	})
	return worker, attempt, err
}

func (s *Service) persistWorkerProcess(workerID, attemptID uint, lease string, process proactive.ProcessHandle) error {
	if process == nil || process.PID() <= 0 || process.StartToken() == "" {
		return errors.New("process identity is required")
	}
	err := retryManualSQLite(func() error {
		return s.DB.Transaction(func(tx *gorm.DB) error {
			var worker ManualRunWorker
			if err := tx.First(&worker, workerID).Error; err != nil {
				return err
			}
			updated := tx.Model(&ManualWorkerAttempt{}).Where("id = ? AND worker_id = ? AND lease_token = ? AND state = ?", attemptID, workerID, lease, ManualWorkerStateStarting).Updates(map[string]interface{}{"state": ManualWorkerStateRunning, "process_id": process.PID(), "process_start_token": process.StartToken()})
			if updated.Error != nil || updated.RowsAffected != 1 {
				if updated.Error != nil {
					return updated.Error
				}
				return ErrWorkerConflict
			}
			updated = tx.Model(&ManualRunWorker{}).Where("id = ? AND current_attempt_id = ? AND lease_token = ? AND state = ?", workerID, attemptID, lease, ManualWorkerStateStarting).Updates(map[string]interface{}{"state": ManualWorkerStateRunning, "process_id": process.PID(), "process_start_token": process.StartToken()})
			if updated.Error != nil || updated.RowsAffected != 1 {
				if updated.Error != nil {
					return updated.Error
				}
				return ErrWorkerConflict
			}
			return tx.Create(&ManualWorkerEvent{RunID: worker.RunID, WorkerID: workerID, AttemptID: attemptID, EventType: ManualWorkerEventProcessStarted, FromState: ManualWorkerStateStarting, ToState: ManualWorkerStateRunning, ActorIdentity: "manual-transfer-worker", ActorType: "system", Details: "worker process identity persisted"}).Error
		})
	})
	return err
}

func (s *Service) persistWorkerManifest(workerID, attemptID uint, lease, path, hash string) error {
	if path == "" || hash == "" {
		return errors.New("worker manifest identity is required")
	}
	return retryManualSQLite(func() error {
		updated := s.DB.Model(&ManualWorkerAttempt{}).Where("id = ? AND worker_id = ? AND lease_token = ? AND state = ?", attemptID, workerID, lease, ManualWorkerStateStarting).Updates(map[string]interface{}{"manifest_path": path, "manifest_hash": hash})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return ErrWorkerConflict
		}
		return nil
	})
}

func (s *Service) finishWorker(worker ManualRunWorker, attempt ManualWorkerAttempt, state string, cause error, exitCode *int) error {
	message := ""
	if cause != nil {
		message = sanitizeMessage(cause.Error())
	}
	now := s.now()
	err := retryManualSQLite(func() error {
		return s.DB.Transaction(func(tx *gorm.DB) error {
			var current ManualRunWorker
			if err := tx.First(&current, worker.ID).Error; err != nil {
				return err
			}
			var currentAttempt ManualWorkerAttempt
			if err := tx.First(&currentAttempt, attempt.ID).Error; err != nil {
				return err
			}
			from := current.State
			updates := map[string]interface{}{"state": state, "lease_token": "", "lease_until": nil, "process_id": 0, "process_start_token": "", "finished_at": now, "last_error": message, "revision": gorm.Expr("revision + 1")}
			if err := tx.Model(&ManualRunWorker{}).Where("id = ? AND current_attempt_id = ? AND state IN ?", current.ID, attempt.ID, []string{ManualWorkerStateStarting, ManualWorkerStateRunning, ManualWorkerStateReconciling}).Updates(updates).Error; err != nil {
				return err
			}
			attemptUpdates := map[string]interface{}{"state": state, "lease_token": "", "lease_until": nil, "process_id": 0, "process_start_token": "", "finished_at": now, "last_error": message}
			if exitCode != nil {
				attemptUpdates["exit_code"] = *exitCode
			}
			if err := tx.Model(&ManualWorkerAttempt{}).Where("id = ? AND worker_id = ? AND state IN ?", attempt.ID, worker.ID, []string{ManualWorkerStateStarting, ManualWorkerStateRunning, ManualWorkerStateReconciling}).Updates(attemptUpdates).Error; err != nil {
				return err
			}
			if err := s.updateWorkerProgressTx(tx, current.ID, attempt.ID); err != nil {
				return err
			}
			if err := tx.Create(&ManualWorkerEvent{RunID: current.RunID, WorkerID: current.ID, AttemptID: attempt.ID, EventType: ManualWorkerEventFinished, FromState: from, ToState: state, ActorIdentity: "manual-transfer-worker", ActorType: "system", Details: message}).Error; err != nil {
				return err
			}
			return s.deriveRunStateTx(tx, current.RunID)
		})
	})
	return err
}

// markWorkerNeedsAttention records a failed identity-safe stop without
// releasing the lease or clearing PID/start-token ownership. A later explicit
// cancel/reconciliation must prove the child is gone before finishWorker may
// release those fields.
func (s *Service) markWorkerNeedsAttention(workerID, attemptID uint, cause error) error {
	message := "worker requires manual recovery"
	if cause != nil {
		message = sanitizeMessage(cause.Error())
	}
	return retryManualSQLite(func() error {
		return s.DB.Transaction(func(tx *gorm.DB) error {
			var worker ManualRunWorker
			if err := tx.First(&worker, workerID).Error; err != nil {
				return err
			}
			if attemptID == 0 {
				attemptID = worker.CurrentAttemptID
			}
			var attempt ManualWorkerAttempt
			if err := tx.First(&attempt, attemptID).Error; err != nil {
				return err
			}
			from := worker.State
			if err := tx.Model(&ManualRunWorker{}).Where("id = ? AND current_attempt_id = ?", workerID, attemptID).Updates(map[string]interface{}{
				"state":      ManualWorkerStateNeedsAttention,
				"last_error": message,
				"revision":   gorm.Expr("revision + 1"),
			}).Error; err != nil {
				return err
			}
			if err := tx.Model(&ManualWorkerAttempt{}).Where("id = ? AND worker_id = ?", attemptID, workerID).Updates(map[string]interface{}{
				"state":      ManualWorkerStateNeedsAttention,
				"last_error": message,
			}).Error; err != nil {
				return err
			}
			if err := tx.Create(&ManualWorkerEvent{RunID: worker.RunID, WorkerID: workerID, AttemptID: attemptID, EventType: ManualWorkerEventFinished, FromState: from, ToState: ManualWorkerStateNeedsAttention, ActorIdentity: "manual-transfer-worker", ActorType: "system", Details: message}).Error; err != nil {
				return err
			}
			return s.deriveRunStateTx(tx, worker.RunID)
		})
	})
}

func (s *Service) reconcileWorkerFiles(ctx context.Context, worker ManualRunWorker, attempt ManualWorkerAttempt) error {
	var run ManualTransferRun
	if err := s.DB.First(&run, worker.RunID).Error; err != nil {
		return err
	}
	if run.TransferMode == models.TransferModeMove {
		root, err := quota.OpenSourceRoot(run.SourcePath)
		if err != nil {
			return err
		}
		defer root.Close()
		if root.Device != run.SourceRootDevice || root.Inode != run.SourceRootInode {
			return errors.New("source root identity changed during move recovery")
		}
		quarantine, err := openMoveWorkerQuarantine(root, run, worker)
		if err != nil {
			return err
		}
		defer quarantine.Close()
		return s.reconcileMoveWorkerFiles(worker, attempt, root, quarantine)
	}
	files, err := s.currentWorkerFiles(worker.ID)
	if err != nil {
		return err
	}
	var firstErr error
	for _, file := range files {
		if file.State == ManualWorkerFileStateVerified {
			continue
		}
		object, statErr := s.Runner.StatRemote(ctx, worker.ConfigIdentity, worker.RemoteName, workerDestination(s.DB, worker.RunID), file.RelativePath)
		if statErr != nil {
			if firstErr == nil {
				firstErr = statErr
			}
			continue
		}
		if object.IsDir || object.Size != file.SizeBytes || (object.Path != "" && object.Path != file.RelativePath && filepath.Base(object.Path) != filepath.Base(file.RelativePath)) {
			if firstErr == nil {
				firstErr = fmt.Errorf("remote verification mismatch for %q", file.RelativePath)
			}
			continue
		}
		if err := s.markWorkerFileVerified(file.ID, worker.ID, attempt.ID); err != nil {
			return err
		}
	}
	return firstErr
}

func openMoveWorkerQuarantine(root *quota.SourceRootHandle, run ManualTransferRun, worker ManualRunWorker) (*quota.MoveQuarantine, error) {
	owner := manualWorkerMoveOwner(worker.ID)
	quarantine, err := quota.OpenMoveQuarantine(root, worker.ID, owner)
	if err != nil {
		return nil, err
	}
	expected := filepath.Join(filepath.Clean(run.SourcePath), ".rclone-manager-move", fmt.Sprintf("%d-%s", worker.ID, owner))
	if quarantine.Path() != expected {
		_ = quarantine.Close()
		return nil, errors.New("move quarantine path changed during recovery")
	}
	if _, _, err := quarantine.Identity(); err != nil {
		_ = quarantine.Close()
		return nil, err
	}
	return quarantine, nil
}

func (s *Service) verifyExistingRemote(ctx context.Context, worker ManualRunWorker, attempt ManualWorkerAttempt, files []ManualWorkerFile) error {
	for _, file := range files {
		if file.State == ManualWorkerFileStateVerified {
			continue
		}
		object, statErr := s.Runner.StatRemote(ctx, worker.ConfigIdentity, worker.RemoteName, workerDestination(s.DB, worker.RunID), file.RelativePath)
		if statErr != nil || object.IsDir || object.Size != file.SizeBytes || (object.Path != "" && object.Path != file.RelativePath && filepath.Base(object.Path) != filepath.Base(file.RelativePath)) {
			continue
		}
		if err := s.markWorkerFileVerified(file.ID, worker.ID, attempt.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) markWorkerFileVerified(fileID, workerID, attemptID uint) error {
	now := s.now()
	return retryManualSQLite(func() error {
		return s.DB.Transaction(func(tx *gorm.DB) error {
			updated := tx.Model(&ManualWorkerFile{}).Where("id = ? AND worker_id = ? AND state <> ?", fileID, workerID, ManualWorkerFileStateVerified).Updates(map[string]interface{}{"state": ManualWorkerFileStateVerified, "verified_at": now, "last_error": "", "attempt_id": attemptID})
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected == 0 {
				return nil
			}
			path := workerFilePath(tx, fileID)
			if err := tx.Model(&ManualRunWorker{}).Where("id = ?", workerID).Update("current_relative_path", path).Error; err != nil {
				return err
			}
			return tx.Create(&ManualWorkerProgress{RunID: workerRunID(tx, workerID), WorkerID: workerID, AttemptID: attemptID, Sequence: nextWorkerProgressSequence(tx, workerID), State: ManualWorkerFileStateVerified, RelativePath: path}).Error
		})
	})
}

func (s *Service) currentWorkerFiles(workerID uint) ([]ManualWorkerFile, error) {
	var files []ManualWorkerFile
	if err := s.DB.Where("worker_id = ?", workerID).Order("relative_path ASC, id ASC").Find(&files).Error; err != nil {
		return nil, err
	}
	return files, nil
}

func (s *Service) validateWorkerSourceSnapshot(worker ManualRunWorker) error {
	var run ManualTransferRun
	if err := s.DB.First(&run, worker.RunID).Error; err != nil {
		return err
	}
	root, err := quota.OpenSourceRoot(run.SourcePath)
	if err != nil {
		return err
	}
	defer root.Close()
	if root.Device != run.SourceRootDevice || root.Inode != run.SourceRootInode {
		return errors.New("source root identity changed")
	}
	files, err := s.currentWorkerFiles(worker.ID)
	if err != nil {
		return err
	}
	for _, file := range files {
		ok, validateErr := root.Validate(quota.LocalSnapshot{RelativePath: file.RelativePath, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode, RootDevice: root.Device, RootInode: root.Inode, SnapshotKey: file.SnapshotKey})
		if validateErr != nil {
			return validateErr
		}
		if !ok {
			return fmt.Errorf("source file changed: %s", file.RelativePath)
		}
	}
	return nil
}

func (s *Service) incompleteWorkerFiles(workerID uint) ([]ManualWorkerFile, error) {
	var files []ManualWorkerFile
	if err := s.DB.Where("worker_id = ? AND state <> ?", workerID, ManualWorkerFileStateVerified).Order("relative_path ASC, id ASC").Find(&files).Error; err != nil {
		return nil, err
	}
	return files, nil
}

func (s *Service) isWorkerCancelRequested(workerID, attemptID uint) bool {
	var worker ManualRunWorker
	if s.DB.First(&worker, workerID).Error != nil {
		return true
	}
	if worker.CancelRequested {
		return true
	}
	var attempt ManualWorkerAttempt
	if s.DB.First(&attempt, attemptID).Error != nil {
		return true
	}
	return attempt.CancelRequested
}

func (s *Service) deriveRunStateTx(tx *gorm.DB, runID uint) error {
	var workers []ManualRunWorker
	if err := tx.Where("run_id = ?", runID).Find(&workers).Error; err != nil {
		return err
	}
	if len(workers) == 0 {
		return nil
	}
	state := ManualRunStateSucceeded
	active := false
	needsAttention := false
	failed := false
	cancelled := false
	succeeded := false
	for _, worker := range workers {
		switch worker.State {
		case ManualWorkerStatePending, ManualWorkerStateStarting, ManualWorkerStateRunning, ManualWorkerStateReconciling:
			active = true
		case ManualWorkerStateNeedsAttention, ManualWorkerStateUnknown:
			needsAttention = true
		case ManualWorkerStateFailed:
			failed = true
		case ManualWorkerStateCancelled:
			cancelled = true
		case ManualWorkerStateSucceeded:
			succeeded = true
		default:
			needsAttention = true
		}
	}
	switch {
	case active:
		state = ManualRunStateRunning
	case needsAttention:
		state = ManualRunStateNeedsAttention
	case failed:
		// A failed worker keeps the run failed even when another worker was
		// cancelled or completed; it is never reported as succeeded.
		state = ManualRunStateFailed
	case cancelled:
		// A succeeded+canceled mix is a partial canceled run, not success.
		state = ManualRunStateCancelled
	case succeeded:
		state = ManualRunStateSucceeded
	default:
		state = ManualRunStateNeedsAttention
	}
	var run ManualTransferRun
	if err := tx.First(&run, runID).Error; err != nil {
		return err
	}
	if run.State == state {
		return nil
	}
	updated := tx.Model(&ManualTransferRun{}).Where("id = ? AND state IN ?", runID, []string{ManualRunStateAllocated, ManualRunStateRunning, ManualRunStateSucceeded, ManualRunStateFailed, ManualRunStateCancelled, ManualRunStateNeedsAttention}).Updates(map[string]interface{}{"state": state, "revision": gorm.Expr("revision + 1")})
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected == 0 {
		return ErrRevisionConflict
	}
	return tx.Create(&ManualRunEvent{RunID: runID, EventType: ManualRunEventWorkersReconciled, FromState: run.State, ToState: state, ActorIdentity: "manual-transfer-worker", ActorType: "system", Details: "manual run state derived from worker states"}).Error
}

func (s *Service) RecoverWorkers() error {
	if s == nil || s.DB == nil {
		return errors.New("manual transfer database is required")
	}
	var workers []ManualRunWorker
	if err := s.DB.Where("state IN ? OR (state = ? AND (lease_token <> '' OR process_id > 0 OR process_start_token <> ''))", []string{ManualWorkerStateStarting, ManualWorkerStateRunning, ManualWorkerStateReconciling}, ManualWorkerStateNeedsAttention).Find(&workers).Error; err != nil {
		return err
	}
	for _, worker := range workers {
		var attempt ManualWorkerAttempt
		if err := s.DB.First(&attempt, worker.CurrentAttemptID).Error; err != nil {
			return err
		}
		if err := s.stopPersistedWorkerProcess(worker.ID, attempt.ID, nil); err != nil {
			if attentionErr := s.markWorkerNeedsAttention(worker.ID, attempt.ID, fmt.Errorf("worker restart recovery could not stop the verified process: %w", err)); attentionErr != nil {
				return errors.Join(err, attentionErr)
			}
			continue
		}
		reconcileErr := error(nil)
		if s.Runner == nil {
			reconcileErr = ErrWorkerUnavailable
		} else {
			reconcileErr = s.reconcileWorkerFiles(context.Background(), worker, attempt)
		}
		message := "worker was active during server restart; explicit retry is required"
		if reconcileErr != nil {
			message += ": " + sanitizeMessage(reconcileErr.Error())
		}
		if err := retryManualSQLite(func() error {
			return s.DB.Transaction(func(tx *gorm.DB) error {
				updates := map[string]interface{}{"state": ManualWorkerStateNeedsAttention, "lease_token": "", "lease_until": nil, "process_id": 0, "process_start_token": "", "last_error": message, "finished_at": s.now(), "revision": gorm.Expr("revision + 1")}
				if err := tx.Model(&ManualRunWorker{}).Where("id = ? AND state IN ?", worker.ID, []string{ManualWorkerStateStarting, ManualWorkerStateRunning, ManualWorkerStateReconciling, ManualWorkerStateNeedsAttention}).Updates(updates).Error; err != nil {
					return err
				}
				if err := tx.Model(&ManualWorkerAttempt{}).Where("id = ? AND state IN ?", attempt.ID, []string{ManualWorkerStateStarting, ManualWorkerStateRunning, ManualWorkerStateReconciling, ManualWorkerStateNeedsAttention}).Updates(map[string]interface{}{"state": ManualWorkerStateNeedsAttention, "lease_token": "", "lease_until": nil, "process_id": 0, "process_start_token": "", "last_error": message, "finished_at": s.now()}).Error; err != nil {
					return err
				}
				if err := tx.Create(&ManualWorkerEvent{RunID: worker.RunID, WorkerID: worker.ID, AttemptID: attempt.ID, EventType: ManualWorkerEventStartupReconciled, FromState: worker.State, ToState: ManualWorkerStateNeedsAttention, ActorIdentity: "system", ActorType: "system", Details: message}).Error; err != nil {
					return err
				}
				return s.deriveRunStateTx(tx, worker.RunID)
			})
		}); err != nil {
			return err
		}
	}
	var attempts []ManualWorkerAttempt
	if err := s.DB.Where("state IN ? OR (state = ? AND (lease_token <> '' OR process_id > 0 OR process_start_token <> ''))", []string{ManualWorkerStateStarting, ManualWorkerStateRunning, ManualWorkerStateReconciling}, ManualWorkerStateNeedsAttention).Find(&attempts).Error; err != nil {
		return err
	}
	for _, attempt := range attempts {
		var worker ManualRunWorker
		if err := s.DB.First(&worker, attempt.WorkerID).Error; err != nil {
			return err
		}
		if worker.State == ManualWorkerStateStarting || worker.State == ManualWorkerStateRunning || worker.State == ManualWorkerStateReconciling {
			continue
		}
		if err := s.stopPersistedWorkerProcess(worker.ID, attempt.ID, nil); err != nil {
			if attentionErr := s.markWorkerNeedsAttention(worker.ID, attempt.ID, fmt.Errorf("worker attempt recovery could not stop the verified process: %w", err)); attentionErr != nil {
				return errors.Join(err, attentionErr)
			}
			continue
		}
		reconcileErr := error(nil)
		if s.Runner == nil {
			reconcileErr = ErrWorkerUnavailable
		} else {
			reconcileErr = s.reconcileWorkerFiles(context.Background(), worker, attempt)
		}
		message := "worker attempt was active during server restart; explicit retry is required"
		if reconcileErr != nil {
			message += ": " + sanitizeMessage(reconcileErr.Error())
		}
		if err := retryManualSQLite(func() error {
			return s.DB.Transaction(func(tx *gorm.DB) error {
				if err := tx.Model(&ManualWorkerAttempt{}).Where("id = ? AND state IN ?", attempt.ID, []string{ManualWorkerStateStarting, ManualWorkerStateRunning, ManualWorkerStateReconciling}).Updates(map[string]interface{}{"state": ManualWorkerStateNeedsAttention, "lease_token": "", "lease_until": nil, "process_id": 0, "process_start_token": "", "last_error": message, "finished_at": s.now()}).Error; err != nil {
					return err
				}
				if err := tx.Model(&ManualRunWorker{}).Where("id = ? AND state NOT IN ?", worker.ID, []string{ManualWorkerStateStarting, ManualWorkerStateRunning, ManualWorkerStateReconciling}).Updates(map[string]interface{}{"state": ManualWorkerStateNeedsAttention, "lease_token": "", "lease_until": nil, "last_error": message, "finished_at": s.now(), "process_id": 0, "process_start_token": "", "revision": gorm.Expr("revision + 1")}).Error; err != nil {
					return err
				}
				if err := tx.Create(&ManualWorkerEvent{RunID: worker.RunID, WorkerID: worker.ID, AttemptID: attempt.ID, EventType: ManualWorkerEventAttemptReconciled, FromState: attempt.State, ToState: ManualWorkerStateNeedsAttention, ActorIdentity: "system", ActorType: "system", Details: message}).Error; err != nil {
					return err
				}
				return s.deriveRunStateTx(tx, worker.RunID)
			})
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) CancelWorker(ctx context.Context, workerID uint, actorIdentity, actorType string) (WorkerDetail, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	_, run, err := s.loadWorkerRun(workerID)
	if err != nil {
		return WorkerDetail{}, err
	}
	launchLock := s.workerLaunchLock(workerID)
	launchLock.Lock()
	defer launchLock.Unlock()
	request := func(_ *models.Task) error { return s.requestWorkerCancel(workerID, actorIdentity, actorType) }
	if s.TaskFence != nil {
		if err := s.TaskFence.WithTaskExclusive(ctx, run.TaskID, request); err != nil {
			return WorkerDetail{}, err
		}
	} else if err := request(nil); err != nil {
		return WorkerDetail{}, err
	}
	ownedProcess, stopErr := s.stopOwnedWorker(workerID)
	if stopErr != nil {
		var current ManualRunWorker
		if loadErr := s.DB.First(&current, workerID).Error; loadErr == nil {
			stopErr = errors.Join(stopErr, s.markWorkerNeedsAttention(current.ID, current.CurrentAttemptID, fmt.Errorf("worker cancellation could not stop the verified process: %w", stopErr)))
		}
		return WorkerDetail{}, stopErr
	}
	if !ownedProcess {
		if err := s.terminalizeWorkerCancel(workerID, actorIdentity, actorType); err != nil {
			return WorkerDetail{}, err
		}
	}
	return s.GetWorker(workerID)
}

func (s *Service) requestWorkerCancel(workerID uint, actorIdentity, actorType string) error {
	return retryManualSQLite(func() error {
		return s.DB.Transaction(func(tx *gorm.DB) error {
			var worker ManualRunWorker
			if err := tx.First(&worker, workerID).Error; err != nil {
				return err
			}
			if worker.State == ManualWorkerStateSucceeded || worker.State == ManualWorkerStateFailed || worker.State == ManualWorkerStateCancelled {
				return nil
			}
			now := s.now()
			updated := tx.Model(&ManualRunWorker{}).Where("id = ? AND state IN ?", worker.ID, []string{ManualWorkerStatePending, ManualWorkerStateStarting, ManualWorkerStateRunning, ManualWorkerStateReconciling, ManualWorkerStateUnknown, ManualWorkerStateNeedsAttention}).Updates(map[string]interface{}{"cancel_requested": true, "cancel_requested_at": now, "revision": gorm.Expr("revision + 1")})
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected != 1 {
				return ErrWorkerConflict
			}
			if worker.CurrentAttemptID != 0 {
				_ = tx.Model(&ManualWorkerAttempt{}).Where("id = ? AND state IN ?", worker.CurrentAttemptID, []string{ManualWorkerStatePending, ManualWorkerStateStarting, ManualWorkerStateRunning, ManualWorkerStateReconciling}).Updates(map[string]interface{}{"cancel_requested": true})
			}
			return tx.Create(&ManualWorkerEvent{RunID: worker.RunID, WorkerID: worker.ID, AttemptID: worker.CurrentAttemptID, EventType: ManualWorkerEventCancelRequested, FromState: worker.State, ToState: worker.State, ActorIdentity: actorIdentity, ActorType: actorType, Details: "operator requested worker cancellation"}).Error
		})
	})
}

func (s *Service) terminalizeWorkerCancel(workerID uint, actorIdentity, actorType string) error {
	now := s.now()
	return retryManualSQLite(func() error {
		return s.DB.Transaction(func(tx *gorm.DB) error {
			var worker ManualRunWorker
			if err := tx.First(&worker, workerID).Error; err != nil {
				return err
			}
			if worker.State == ManualWorkerStateSucceeded || worker.State == ManualWorkerStateFailed || worker.State == ManualWorkerStateCancelled {
				return nil
			}
			if err := tx.Model(&ManualRunWorker{}).Where("id = ? AND state NOT IN ?", worker.ID, []string{ManualWorkerStateSucceeded, ManualWorkerStateFailed, ManualWorkerStateCancelled}).Updates(map[string]interface{}{"state": ManualWorkerStateCancelled, "lease_token": "", "lease_until": nil, "process_id": 0, "process_start_token": "", "cancel_requested": true, "cancel_requested_at": now, "finished_at": now, "last_error": "cancelled by operator; no owned process remained", "revision": gorm.Expr("revision + 1")}).Error; err != nil {
				return err
			}
			if worker.CurrentAttemptID != 0 {
				if err := tx.Model(&ManualWorkerAttempt{}).Where("id = ? AND state NOT IN ?", worker.CurrentAttemptID, []string{ManualWorkerStateSucceeded, ManualWorkerStateFailed, ManualWorkerStateCancelled}).Updates(map[string]interface{}{"state": ManualWorkerStateCancelled, "lease_token": "", "lease_until": nil, "process_id": 0, "process_start_token": "", "cancel_requested": true, "finished_at": now, "last_error": "cancelled by operator; no owned process remained"}).Error; err != nil {
					return err
				}
			}
			if err := tx.Create(&ManualWorkerEvent{RunID: worker.RunID, WorkerID: worker.ID, AttemptID: worker.CurrentAttemptID, EventType: ManualWorkerEventFinished, FromState: worker.State, ToState: ManualWorkerStateCancelled, ActorIdentity: actorIdentity, ActorType: actorType, Details: "operator cancelled worker after owned-process verification"}).Error; err != nil {
				return err
			}
			return s.deriveRunStateTx(tx, worker.RunID)
		})
	})
}

func (s *Service) RetryWorker(ctx context.Context, workerID uint, actorIdentity, actorType string) (WorkerDetail, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	_, run, err := s.loadWorkerRun(workerID)
	if err != nil {
		return WorkerDetail{}, err
	}
	var launch bool
	request := func(_ *models.Task) error {
		launch, err = s.createWorkerRetry(workerID, actorIdentity, actorType)
		return err
	}
	if s.TaskFence != nil {
		err = s.TaskFence.WithTaskExclusive(ctx, run.TaskID, request)
	} else {
		err = request(nil)
	}
	if err != nil {
		return WorkerDetail{}, err
	}
	if launch {
		s.launchWorker(workerID)
	}
	return s.GetWorker(workerID)
}

func (s *Service) createWorkerRetry(workerID uint, actorIdentity, actorType string) (bool, error) {
	launch := false
	err := retryManualSQLite(func() error {
		return s.DB.Transaction(func(tx *gorm.DB) error {
			var worker ManualRunWorker
			if err := tx.First(&worker, workerID).Error; err != nil {
				return err
			}
			if worker.State != ManualWorkerStatePending && worker.State != ManualWorkerStateFailed && worker.State != ManualWorkerStateCancelled && worker.State != ManualWorkerStateNeedsAttention && worker.State != ManualWorkerStateUnknown {
				return ErrWorkerConflict
			}
			var run ManualTransferRun
			if err := tx.First(&run, worker.RunID).Error; err != nil {
				return err
			}
			if run.TransferMode != models.TransferModeCopy {
				return ErrManualMoveUnsupported
			}
			var incomplete int64
			if err := tx.Model(&ManualWorkerFile{}).Where("worker_id = ? AND state <> ?", worker.ID, ManualWorkerFileStateVerified).Count(&incomplete).Error; err != nil {
				return err
			}
			if incomplete == 0 {
				return ErrWorkerNoIncomplete
			}
			attempt := ManualWorkerAttempt{}
			if worker.State == ManualWorkerStatePending {
				if err := tx.First(&attempt, worker.CurrentAttemptID).Error; err != nil {
					return err
				}
				if attempt.State != ManualWorkerStatePending {
					return ErrWorkerConflict
				}
			} else {
				attempt = ManualWorkerAttempt{RunID: worker.RunID, WorkerID: worker.ID, AttemptNumber: worker.AttemptNumber + 1, State: ManualWorkerStatePending}
				if err := tx.Create(&attempt).Error; err != nil {
					return err
				}
			}
			var files []ManualWorkerFile
			if err := tx.Where("worker_id = ? AND state <> ?", worker.ID, ManualWorkerFileStateVerified).Find(&files).Error; err != nil {
				return err
			}
			var assignedBytes int64
			for _, file := range files {
				assignedBytes += file.SizeBytes
			}
			if err := tx.Model(&ManualWorkerFile{}).Where("worker_id = ? AND state <> ?", worker.ID, ManualWorkerFileStateVerified).Updates(map[string]interface{}{"attempt_id": attempt.ID, "state": ManualWorkerFileStatePending, "last_error": ""}).Error; err != nil {
				return err
			}
			if err := tx.Model(&ManualRunWorker{}).Where("id = ? AND state IN ?", worker.ID, []string{ManualWorkerStatePending, ManualWorkerStateFailed, ManualWorkerStateCancelled, ManualWorkerStateNeedsAttention, ManualWorkerStateUnknown}).Updates(map[string]interface{}{"state": ManualWorkerStatePending, "attempt_number": attempt.AttemptNumber, "current_attempt_id": attempt.ID, "cancel_requested": false, "cancel_requested_at": nil, "speed_bytes_per_second": 0, "progress_percent": 0, "last_error": "", "revision": gorm.Expr("revision + 1")}).Error; err != nil {
				return err
			}
			if err := tx.Model(&attempt).Updates(map[string]interface{}{"assigned_count": incomplete, "assigned_bytes": assignedBytes}).Error; err != nil {
				return err
			}
			if err := tx.Create(&ManualWorkerEvent{RunID: worker.RunID, WorkerID: worker.ID, AttemptID: attempt.ID, EventType: ManualWorkerEventRetryRequested, FromState: worker.State, ToState: ManualWorkerStatePending, ActorIdentity: actorIdentity, ActorType: actorType, Details: "operator created an explicit retry for incomplete files"}).Error; err != nil {
				return err
			}
			launch = true
			return s.deriveRunStateTx(tx, worker.RunID)
		})
	})
	return launch, err
}

func (s *Service) loadWorker(workerID uint) (ManualRunWorker, error) {
	worker, _, err := s.loadWorkerRun(workerID)
	return worker, err
}

func (s *Service) loadWorkerRun(workerID uint) (ManualRunWorker, ManualTransferRun, error) {
	var worker ManualRunWorker
	if err := s.DB.First(&worker, workerID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return worker, ManualTransferRun{}, ErrWorkerNotFound
		}
		return worker, ManualTransferRun{}, err
	}
	run, err := s.GetRun(worker.RunID)
	if err != nil {
		return ManualRunWorker{}, ManualTransferRun{}, err
	}
	return worker, run, nil
}

func (s *Service) decorateWorkerActionability(worker *ManualRunWorker) error {
	if worker == nil {
		return nil
	}
	worker.Actionability = "none"
	switch worker.State {
	case ManualWorkerStatePending:
		worker.Actionability = "retry"
	case ManualWorkerStateStarting, ManualWorkerStateRunning, ManualWorkerStateReconciling:
		worker.Actionability = "cancel"
	case ManualWorkerStateUnknown, ManualWorkerStateNeedsAttention, ManualWorkerStateFailed, ManualWorkerStateCancelled:
		owned, err := s.workerHasDurableOwnership(worker)
		if err != nil {
			return err
		}
		if owned {
			// A failed identity-safe stop must be retried through Cancel so the
			// persisted child identity is reconciled before another start.
			worker.Actionability = "cancel"
			return nil
		}
		var incomplete int64
		if err := s.DB.Model(&ManualWorkerFile{}).Where("worker_id = ? AND state <> ?", worker.ID, ManualWorkerFileStateVerified).Count(&incomplete).Error; err != nil {
			return err
		}
		if incomplete > 0 {
			worker.Actionability = "retry"
		}
	}
	return nil
}

func (s *Service) workerHasDurableOwnership(worker *ManualRunWorker) (bool, error) {
	if worker == nil {
		return false, nil
	}
	if worker.LeaseToken != "" || worker.ProcessID > 0 || worker.ProcessStartToken != "" {
		return true, nil
	}
	if worker.CurrentAttemptID == 0 {
		return false, nil
	}
	var attempt ManualWorkerAttempt
	if err := s.DB.First(&attempt, worker.CurrentAttemptID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	return attempt.LeaseToken != "" || attempt.ProcessID > 0 || attempt.ProcessStartToken != "", nil
}

func (s *Service) stopOwnedWorker(workerID uint) (bool, error) {
	s.workerMu.Lock()
	owned := s.workerProcesses[workerID]
	s.workerMu.Unlock()
	var persisted ManualRunWorker
	if s.DB.First(&persisted, workerID).Error != nil {
		return false, ErrWorkerNotFound
	}
	if owned != nil && persisted.CurrentAttemptID == owned.attemptID && owned.process != nil && persisted.ProcessID > 0 && (persisted.ProcessID != owned.process.PID() || persisted.ProcessStartToken != owned.process.StartToken()) {
		return false, errors.New("owned process identity changed during cancellation")
	}
	if err := s.stopPersistedWorkerProcess(workerID, persisted.CurrentAttemptID, processFromOwned(owned)); err != nil {
		return false, err
	}
	if owned != nil && owned.cancel != nil {
		owned.cancel()
	}
	return owned != nil && owned.process != nil, nil
}

func processFromOwned(owned *ownedWorkerProcess) proactive.ProcessHandle {
	if owned == nil {
		return nil
	}
	return owned.process
}

func (s *Service) startWorkerLeaseHeartbeat(ctx context.Context, workerID, attemptID uint, lease string) *workerLeaseHeartbeat {
	h := &workerLeaseHeartbeat{stopCh: make(chan struct{}), doneCh: make(chan struct{}), errCh: make(chan error, 1)}
	go func() {
		defer close(h.doneCh)
		ticker := time.NewTicker(s.workerLeaseRenewal())
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				now := s.now()
				until := now.Add(s.workerLeaseDuration())
				attemptUpdate := s.DB.Model(&ManualWorkerAttempt{}).Where("id = ? AND worker_id = ? AND lease_token = ? AND state IN ?", attemptID, workerID, lease, []string{ManualWorkerStateStarting, ManualWorkerStateRunning, ManualWorkerStateReconciling}).Update("lease_until", until)
				workerUpdate := s.DB.Model(&ManualRunWorker{}).Where("id = ? AND current_attempt_id = ? AND lease_token = ? AND state IN ?", workerID, attemptID, lease, []string{ManualWorkerStateStarting, ManualWorkerStateRunning, ManualWorkerStateReconciling}).Update("lease_until", until)
				if attemptUpdate.Error != nil || attemptUpdate.RowsAffected != 1 || workerUpdate.Error != nil || workerUpdate.RowsAffected != 1 {
					err := errors.New("manual worker lease was lost")
					if attemptUpdate.Error != nil {
						err = attemptUpdate.Error
					}
					select {
					case h.errCh <- err:
					default:
					}
					return
				}
			case <-h.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return h
}

type workerLeaseHeartbeat struct {
	stopCh chan struct{}
	doneCh chan struct{}
	errCh  chan error
	once   sync.Once
}

func (h *workerLeaseHeartbeat) stop() {
	if h == nil {
		return
	}
	h.once.Do(func() { close(h.stopCh); <-h.doneCh })
}

func (h *workerLeaseHeartbeat) err() error {
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

func (s *Service) updateWorkerProgressTx(tx *gorm.DB, workerID, attemptID uint) error {
	var aggregate struct {
		Count int64
		Bytes int64
	}
	if err := tx.Model(&ManualWorkerFile{}).Where("worker_id = ? AND state = ?", workerID, ManualWorkerFileStateVerified).Select("count(*) AS count, coalesce(sum(size_bytes), 0) AS bytes").Scan(&aggregate).Error; err != nil {
		return err
	}
	var total struct {
		Count int64
		Bytes int64
	}
	if err := tx.Model(&ManualWorkerFile{}).Where("worker_id = ?", workerID).Select("count(*) AS count, coalesce(sum(size_bytes), 0) AS bytes").Scan(&total).Error; err != nil {
		return err
	}
	percent := float64(0)
	if total.Bytes == 0 {
		percent = 100
	} else {
		percent = float64(aggregate.Bytes) * 100 / float64(total.Bytes)
	}
	if err := tx.Model(&ManualRunWorker{}).Where("id = ?", workerID).Updates(map[string]interface{}{"completed_count": aggregate.Count, "completed_bytes": aggregate.Bytes, "assigned_count": total.Count, "assigned_bytes": total.Bytes, "progress_percent": percent, "speed_bytes_per_second": 0, "last_progress_at": s.now()}).Error; err != nil {
		return err
	}
	var attemptAggregate struct {
		Count int64
		Bytes int64
	}
	if err := tx.Model(&ManualWorkerFile{}).Where("worker_id = ? AND attempt_id = ? AND state = ?", workerID, attemptID, ManualWorkerFileStateVerified).Select("count(*) AS count, coalesce(sum(size_bytes), 0) AS bytes").Scan(&attemptAggregate).Error; err != nil {
		return err
	}
	var attemptTotal struct {
		Count int64
		Bytes int64
	}
	if err := tx.Model(&ManualWorkerFile{}).Where("worker_id = ? AND attempt_id = ?", workerID, attemptID).Select("count(*) AS count, coalesce(sum(size_bytes), 0) AS bytes").Scan(&attemptTotal).Error; err != nil {
		return err
	}
	return tx.Model(&ManualWorkerAttempt{}).Where("id = ?", attemptID).Updates(map[string]interface{}{"completed_count": attemptAggregate.Count, "completed_bytes": attemptAggregate.Bytes, "assigned_count": attemptTotal.Count, "assigned_bytes": attemptTotal.Bytes, "progress_percent": percent, "speed_bytes_per_second": 0}).Error
}

func (s *Service) workerLogPath(workerID uint) (string, error) {
	dir := s.logDirectory()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("worker log directory is not trusted")
	}
	return filepath.Join(dir, fmt.Sprintf("worker-%d.log", workerID)), nil
}

func validateManualWorkerConfig(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return errors.New("manual worker config must be an absolute clean path")
	}
	info, err := os.Lstat(value)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("manual worker config is not a trusted regular file")
	}
	return nil
}

func (s *Service) validateWorkerLogPath(value string, workerID uint) error {
	expected, err := s.workerLogPath(workerID)
	if err != nil {
		return err
	}
	actual, err := filepath.Abs(value)
	if err != nil || actual != expected || filepath.Dir(actual) != filepath.Dir(expected) || filepath.Base(actual) != fmt.Sprintf("worker-%d.log", workerID) {
		return errors.New("worker log ownership mismatch")
	}
	return nil
}

func (s *Service) appendWorkerLog(workerID uint, value string) error {
	value = sanitizeWorkerLog(value)
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if s.appendWorkerLogHook != nil {
		return s.appendWorkerLogHook(workerID, value)
	}
	path, err := s.workerLogPath(workerID)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	if err := verifyOwnedWorkerLogFile(file, path); err != nil {
		_ = file.Close()
		return err
	}
	_, writeErr := file.WriteString(value + "\n")
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		return statErr
	}
	if err := s.DB.Model(&ManualWorkerLog{}).Where("worker_id = ?", workerID).Update("size_bytes", info.Size()).Error; err != nil {
		return err
	}
	return nil
}

func verifyOwnedWorkerLogFile(file *os.File, path string) error {
	if file == nil {
		return errors.New("worker log file is unavailable")
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	linked, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if linked.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, linked) {
		return errors.New("worker log file ownership changed")
	}
	return nil
}

func (s *Service) processInspector() workerProcessInspector {
	if s.Inspector != nil {
		return s.Inspector
	}
	return proactive.LinuxProcessInspector{}
}

func (s *Service) workerLeaseDuration() time.Duration {
	if s.LeaseDuration > 0 {
		return s.LeaseDuration
	}
	return manualWorkerLeaseDuration
}

func (s *Service) workerLeaseRenewal() time.Duration {
	if s.LeaseRenewInterval > 0 {
		return s.LeaseRenewInterval
	}
	return manualWorkerLeaseRenewal
}

func (s *Service) logDirectory() string {
	if strings.TrimSpace(s.LogDir) != "" {
		return filepath.Clean(s.LogDir)
	}
	return filepath.Join(os.TempDir(), "rclone-manager-manual-worker-logs")
}

func (s *Service) manifestDirectory() string {
	if strings.TrimSpace(s.ManifestDir) != "" {
		return filepath.Clean(s.ManifestDir)
	}
	return filepath.Join(s.logDirectory(), "manifests")
}

func (s *Service) stageDirectory() string {
	if strings.TrimSpace(s.StageDir) != "" {
		return filepath.Clean(s.StageDir)
	}
	return s.logDirectory()
}

func ensureWorkerStageBase(base string) error {
	if base == "" || !filepath.IsAbs(base) || filepath.Clean(base) != base {
		return errors.New("manual worker stage base must be an absolute clean path")
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(base)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("manual worker stage base is not a trusted directory")
	}
	return nil
}

func randomWorkerToken() string {
	data := make([]byte, 24)
	if _, err := rand.Read(data); err != nil {
		panic(err)
	}
	return hex.EncodeToString(data)
}

func ptrTime(value time.Time) *time.Time { return &value }

func intPointerValue(value int) *int { return &value }

func workerRunSource(database *gorm.DB, runID uint) string {
	var run ManualTransferRun
	if database.First(&run, runID).Error != nil {
		return ""
	}
	return run.SourcePath
}

func workerDestination(database *gorm.DB, runID uint) string {
	var run ManualTransferRun
	if database.First(&run, runID).Error != nil {
		return ""
	}
	return run.DestinationPath
}

func workerRunID(tx *gorm.DB, workerID uint) uint {
	var worker ManualRunWorker
	if tx.First(&worker, workerID).Error != nil {
		return 0
	}
	return worker.RunID
}

func workerFilePath(tx *gorm.DB, fileID uint) string {
	var file ManualWorkerFile
	if tx.First(&file, fileID).Error != nil {
		return ""
	}
	return file.RelativePath
}

func nextWorkerProgressSequence(tx *gorm.DB, workerID uint) int64 {
	var max struct{ Sequence int64 }
	tx.Model(&ManualWorkerProgress{}).Where("worker_id = ?", workerID).Select("coalesce(max(sequence), 0) AS sequence").Scan(&max)
	return max.Sequence + 1
}

func retryManualSQLite(fn func() error) error {
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		err = fn()
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "database is locked") {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 15 * time.Millisecond)
	}
	return err
}
