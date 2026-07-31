package manualtransfer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
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
	ErrIdempotencyConflict = errors.New("manual analyze idempotency key was reused with a different request")
	ErrRevisionConflict    = errors.New("manual analyze revision conflict")
	ErrRunNotFound         = errors.New("manual transfer run not found")
	ErrActiveAnalysis      = errors.New("an active manual analysis already exists for the task")
	ErrSnapshotUnavailable = errors.New("manual snapshot is not activated")
	ErrAccountNotFound     = errors.New("manual account not found")
	ErrNotManualTask       = errors.New("task is not manual")
)

type TaskExclusive interface {
	WithTaskExclusive(context.Context, uint, func(*models.Task) error) error
}

type AccountInput struct {
	ID              uint   `json:"id"`
	AccountID       uint   `json:"account_id"`
	AccountIdentity string `json:"account_identity"`
	Identity        string `json:"identity"`
	AccountKey      string `json:"account_key"`
	QuotaKey        string `json:"quota_key"`
	RemoteName      string `json:"remote_name"`
	ConfigIdentity  string `json:"config_identity"`
}

type AnalyzeRequest struct {
	TaskID           uint `json:"-"`
	SourcePath       string
	DestinationPath  string
	TransferMode     string
	ConfigIdentity   string
	Accounts         []AccountInput
	IdempotencyKey   string
	ExpectedRunID    *uint
	ExpectedRevision *int64
	ActorIdentity    string
	ActorType        string
}

type AnalyzeResult struct {
	Run      ManualTransferRun
	Existing bool
}

type RunStatus struct {
	ID                      uint       `json:"id"`
	TaskID                  uint       `json:"task_id"`
	State                   string     `json:"state"`
	Revision                int64      `json:"revision"`
	ManualInputRevision     int64      `json:"manual_input_revision"`
	ManualConfigRevision    int64      `json:"manual_config_revision"`
	Terminal                bool       `json:"terminal"`
	Analyzing               bool       `json:"analyzing"`
	Allocating              bool       `json:"allocating"`
	Allocated               bool       `json:"allocated"`
	Running                 bool       `json:"running"`
	Succeeded               bool       `json:"succeeded"`
	Failed                  bool       `json:"failed"`
	Cancelled               bool       `json:"cancelled"`
	NeedsAttention          bool       `json:"needs_attention"`
	AllocationFailed        bool       `json:"allocation_failed"`
	NeedsExplicitReanalyze  bool       `json:"needs_explicit_reanalyze"`
	FailurePolicy           string     `json:"failure_policy"`
	SnapshotDigest          string     `json:"snapshot_digest,omitempty"`
	SnapshotCount           int64      `json:"snapshot_count"`
	SnapshotBytes           int64      `json:"snapshot_bytes"`
	AllocationVersion       int        `json:"allocation_version"`
	AllocationGeneration    int64      `json:"allocation_generation"`
	AllocationDigest        string     `json:"allocation_digest,omitempty"`
	AllocationCount         int64      `json:"allocation_count"`
	AllocationBytes         int64      `json:"allocation_bytes"`
	AssignedCount           int64      `json:"assigned_count"`
	AssignedBytes           int64      `json:"assigned_bytes"`
	AlreadyTransferredCount int64      `json:"already_transferred_count"`
	AlreadyTransferredBytes int64      `json:"already_transferred_bytes"`
	UnassignedCount         int64      `json:"unassigned_count"`
	UnassignedBytes         int64      `json:"unassigned_bytes"`
	OversizeCount           int64      `json:"oversize_count"`
	OversizeBytes           int64      `json:"oversize_bytes"`
	AggregateCapacityCount  int64      `json:"aggregate_capacity_count"`
	AggregateCapacityBytes  int64      `json:"aggregate_capacity_bytes"`
	AccountCapacityCount    int64      `json:"account_capacity_count"`
	AccountCapacityBytes    int64      `json:"account_capacity_bytes"`
	LastError               string     `json:"last_error,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
	AnalysisStartedAt       *time.Time `json:"analysis_started_at,omitempty"`
	AnalyzedAt              *time.Time `json:"analyzed_at,omitempty"`
	FailedAt                *time.Time `json:"failed_at,omitempty"`
}

type FilePage struct {
	Files      []ManualRunFile `json:"files"`
	NextCursor string          `json:"next_cursor,omitempty"`
	HasMore    bool            `json:"has_more"`
	Limit      int             `json:"limit"`
}

type Service struct {
	DB                        *gorm.DB
	Scanner                   quota.Scanner
	Now                       func() time.Time
	TaskFence                 TaskExclusive
	BeforeStartEvent          func(*gorm.DB) error
	Runner                    proactive.CommandRunner
	Inspector                 workerProcessInspector
	LogDir                    string
	ManifestDir               string
	StageDir                  string
	LeaseDuration             time.Duration
	LeaseRenewInterval        time.Duration
	PersistWorkerProcessFunc  func(uint, uint, string, proactive.ProcessHandle) error
	PersistWorkerProgressFunc func(uint, uint, proactive.ProcessProgress) error
	appendWorkerLogHook       func(uint, string) error

	jobs            chan uint
	allocationJobs  chan uint
	workerCtx       context.Context
	workerCancel    context.CancelFunc
	workerDone      chan struct{}
	startOnce       sync.Once
	stopOnce        sync.Once
	workerWG        sync.WaitGroup
	workerMu        sync.Mutex
	workerProcesses map[uint]*ownedWorkerProcess
	launchMu        sync.Mutex
	launchLocks     map[uint]*sync.Mutex
}

func NewService(database *gorm.DB) *Service {
	if database != nil && database.Dialector != nil && database.Dialector.Name() == "sqlite" {
		if sqlDB, err := database.DB(); err == nil {
			sqlDB.SetMaxOpenConns(1)
			sqlDB.SetMaxIdleConns(1)
		}
	}
	return &Service{DB: database, jobs: make(chan uint, manualRunQueueSize), allocationJobs: make(chan uint, manualRunQueueSize), workerProcesses: make(map[uint]*ownedWorkerProcess), launchLocks: make(map[uint]*sync.Mutex)}
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// RecoverAnalyzing terminalizes work that was in progress before a restart.
// It deliberately never enqueues those rows: a later operator request is the
// only way to create a fresh analysis attempt.
func (s *Service) RecoverAnalyzing() error {
	if s == nil || s.DB == nil {
		return errors.New("manual transfer database is required")
	}
	var runs []ManualTransferRun
	if err := s.DB.Where("state = ?", ManualRunStateAnalyzing).Order("id ASC").Find(&runs).Error; err != nil {
		return err
	}
	for _, run := range runs {
		message := "analysis interrupted by server restart; explicit reanalyze required"
		if err := s.failRun(run, message, ManualRunEventStartupTerminalized, "system", "system"); err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			return err
		}
	}
	var allocating []ManualTransferRun
	if err := s.DB.Where("state = ?", ManualRunStateAllocating).Order("id ASC").Find(&allocating).Error; err != nil {
		return err
	}
	for _, run := range allocating {
		if err := s.failAllocationByID(run.ID, "allocation interrupted by server restart; explicit reanalyze required"); err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			return err
		}
	}
	return nil
}

func (s *Service) Start() {
	if s == nil || s.DB == nil {
		return
	}
	s.startOnce.Do(func() {
		s.workerCtx, s.workerCancel = context.WithCancel(context.Background())
		s.workerDone = make(chan struct{})
		go func() {
			defer close(s.workerDone)
			for {
				select {
				case runID := <-s.jobs:
					s.processJob(runID)
				case runID := <-s.allocationJobs:
					s.processAllocationJob(runID)
				case <-s.workerCtx.Done():
					return
				}
			}
		}()
	})
}

func (s *Service) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.StopContext(ctx)
}

func (s *Service) StopContext(ctx context.Context) error {
	if s == nil || s.workerCancel == nil || s.workerDone == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.stopOnce.Do(s.workerCancel)
	select {
	case <-s.workerDone:
		workersDone := make(chan struct{})
		go func() {
			s.workerWG.Wait()
			close(workersDone)
		}()
		select {
		case <-workersDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) Enqueue(runID uint) error {
	if s == nil || s.jobs == nil || s.workerCtx == nil {
		return errors.New("manual transfer worker is not started")
	}
	select {
	case <-s.workerCtx.Done():
		return errors.New("manual transfer worker is stopping")
	case s.jobs <- runID:
		return nil
	default:
		return errors.New("manual transfer analysis queue is full")
	}
}

func (s *Service) EnqueueAllocation(runID uint) error {
	if s == nil || s.allocationJobs == nil || s.workerCtx == nil {
		return errors.New("manual transfer worker is not started")
	}
	select {
	case <-s.workerCtx.Done():
		return errors.New("manual transfer worker is stopping")
	case s.allocationJobs <- runID:
		return nil
	default:
		return errors.New("manual allocation queue is full")
	}
}

func (s *Service) CreateAnalyze(request AnalyzeRequest) (AnalyzeResult, error) {
	return s.CreateAnalyzeContext(context.Background(), request)
}

func (s *Service) CreateAnalyzeContext(ctx context.Context, request AnalyzeRequest) (AnalyzeResult, error) {
	if s == nil || s.DB == nil {
		return AnalyzeResult{}, errors.New("manual transfer database is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateAnalyzeRequest(&request); err != nil {
		return AnalyzeResult{}, err
	}
	if request.TaskID == 0 {
		return AnalyzeResult{}, errors.New("task id is required")
	}
	var result AnalyzeResult
	create := func(task *models.Task) error {
		var err error
		result, err = s.createRunUnderFence(ctx, task, request)
		return err
	}
	var createErr error
	if s.TaskFence != nil {
		createErr = s.TaskFence.WithTaskExclusive(ctx, request.TaskID, create)
	} else {
		var task models.Task
		if err := s.DB.First(&task, request.TaskID).Error; err != nil {
			return AnalyzeResult{}, err
		}
		createErr = create(&task)
	}
	if createErr != nil {
		if errors.Is(createErr, gorm.ErrDuplicatedKey) {
			var existing ManualTransferRun
			if err := s.DB.Where("task_id = ? AND idempotency_key = ?", request.TaskID, request.IdempotencyKey).First(&existing).Error; err == nil {
				return AnalyzeResult{}, ErrIdempotencyConflict
			}
			return AnalyzeResult{}, ErrActiveAnalysis
		}
		return AnalyzeResult{}, createErr
	}
	if result.Existing {
		return result, nil
	}
	if err := s.Enqueue(result.Run.ID); err != nil {
		if terminalErr := s.failRun(result.Run, err.Error(), ManualRunEventAnalysisFailed, "manual-transfer-system", "system"); terminalErr != nil {
			log.Printf("manual run %d failed to terminalize after enqueue failure: %v", result.Run.ID, terminalErr)
		}
		return AnalyzeResult{}, err
	}
	return result, nil
}

func (s *Service) createRunUnderFence(ctx context.Context, task *models.Task, request AnalyzeRequest) (AnalyzeResult, error) {
	if task == nil || task.ID == 0 {
		return AnalyzeResult{}, gorm.ErrRecordNotFound
	}
	if err := requireManualTask(task); err != nil {
		return AnalyzeResult{}, err
	}
	var existingByKey ManualTransferRun
	if lookup := s.DB.Where("task_id = ? AND idempotency_key = ?", task.ID, request.IdempotencyKey).First(&existingByKey); lookup.Error == nil {
		if request.SourcePath != existingByKey.SourcePath || request.DestinationPath != existingByKey.DestinationPath || request.TransferMode != existingByKey.TransferMode {
			return AnalyzeResult{}, ErrIdempotencyConflict
		}
	} else if !errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
		return AnalyzeResult{}, lookup.Error
	}
	if request.SourcePath != task.SourceDir || !manualDestinationMatches(request.DestinationPath, task) || request.TransferMode != task.TransferMode {
		return AnalyzeResult{}, errors.New("manual analyze inputs do not match the task")
	}
	configIdentity, err := canonicalTaskConfig(task)
	if err != nil {
		return AnalyzeResult{}, err
	}
	accounts, err := s.resolveSelectedAccounts(task.ID, request.Accounts, configIdentity)
	if err != nil {
		return AnalyzeResult{}, err
	}
	inputRevision := normalizedManualInputRevision(task.ManualInputRevision)
	configRevision := normalizedManualAccountRevision(task.ManualAccountRevision)
	configFingerprint := manualConfigFingerprint(task, accounts)
	frozen, err := frozenInput{TaskID: task.ID, SourcePath: request.SourcePath, DestinationPath: request.DestinationPath, TransferMode: request.TransferMode, ConfigIdentity: configIdentity, InputRevision: inputRevision, ConfigRevision: configRevision, ConfigFingerprint: configFingerprint, Accounts: accounts, PredecessorRunID: request.ExpectedRunID, PredecessorRevision: request.ExpectedRevision}.Marshal()
	if err != nil {
		return AnalyzeResult{}, err
	}
	fingerprint := fingerprintBytes(frozen)
	var existing ManualTransferRun
	lookup := s.DB.Where("task_id = ? AND idempotency_key = ?", task.ID, request.IdempotencyKey).First(&existing)
	if lookup.Error == nil {
		if !samePredecessor(request.ExpectedRunID, request.ExpectedRevision, existing.PredecessorRunID, existing.PredecessorRevision) {
			return AnalyzeResult{}, ErrIdempotencyConflict
		}
		if existing.RequestFingerprint != fingerprint {
			return AnalyzeResult{}, ErrIdempotencyConflict
		}
		return AnalyzeResult{Run: existing, Existing: true}, nil
	}
	if !errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
		return AnalyzeResult{}, lookup.Error
	}
	var latest ManualTransferRun
	latestResult := s.DB.Where("task_id = ?", task.ID).Order("id DESC").First(&latest)
	if errors.Is(latestResult.Error, gorm.ErrRecordNotFound) {
		if request.ExpectedRunID != nil || request.ExpectedRevision != nil {
			return AnalyzeResult{}, ErrRevisionConflict
		}
	} else if latestResult.Error != nil {
		return AnalyzeResult{}, latestResult.Error
	} else if request.ExpectedRunID == nil || request.ExpectedRevision == nil || *request.ExpectedRunID != latest.ID || *request.ExpectedRevision != latest.Revision {
		return AnalyzeResult{}, ErrRevisionConflict
	}
	var active int64
	if err := s.DB.Model(&ManualTransferRun{}).Where("task_id = ? AND state IN ?", task.ID, []string{ManualRunStateAnalyzing, ManualRunStateAllocating}).Count(&active).Error; err != nil {
		return AnalyzeResult{}, err
	}
	if active != 0 {
		return AnalyzeResult{}, ErrActiveAnalysis
	}
	root, err := quota.OpenSourceRoot(request.SourcePath)
	if err != nil {
		return AnalyzeResult{}, fmt.Errorf("open source root: %w", err)
	}
	rootDevice, rootInode := root.Device, root.Inode
	_ = root.Close()
	run := ManualTransferRun{
		TaskID: task.ID, State: ManualRunStateAnalyzing, Revision: 1,
		ManualInputRevision: inputRevision, ManualConfigRevision: configRevision, ManualConfigFingerprint: configFingerprint,
		ActorIdentity: request.ActorIdentity, ActorType: request.ActorType,
		SourcePath: request.SourcePath, DestinationPath: request.DestinationPath,
		TransferMode: request.TransferMode, ConfigIdentity: configIdentity,
		FrozenInput: frozen, IdempotencyKey: request.IdempotencyKey,
		RequestFingerprint: fingerprint, SourceRootDevice: rootDevice, SourceRootInode: rootInode,
		PredecessorRunID: request.ExpectedRunID, PredecessorRevision: request.ExpectedRevision,
	}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&run).Error; err != nil {
			return err
		}
		for _, account := range accounts {
			row := ManualRunAccount{RunID: run.ID, Position: account.Position, AccountID: account.AccountID, AccountIdentity: account.AccountIdentity, RemoteName: account.RemoteName, ConfigIdentity: account.ConfigIdentity}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		return createEvent(tx, run, ManualRunEventRequested, "", ManualRunStateAnalyzing, "operator requested analyze")
	}); err != nil {
		return AnalyzeResult{}, err
	}
	return AnalyzeResult{Run: run}, nil
}

func (s *Service) processJob(runID uint) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if err := s.failRunByID(runID, fmt.Sprintf("analysis worker panic: %v", recovered)); err != nil {
				log.Printf("manual run %d failed to terminalize after panic: %v", runID, err)
			}
		}
	}()
	if err := s.runJob(runID); err != nil {
		if terminalErr := s.failRunByID(runID, err.Error()); terminalErr != nil {
			log.Printf("manual run %d failed to terminalize: %v", runID, terminalErr)
		}
	}
}

func (s *Service) runJob(runID uint) error {
	var run ManualTransferRun
	if err := s.DB.First(&run, runID).Error; err != nil {
		return err
	}
	if run.State != ManualRunStateAnalyzing {
		return nil
	}
	operation := func(_ *models.Task) error {
		started, err := s.markStarted(run)
		if err != nil {
			return err
		}
		return s.performAnalysis(s.workerCtx, started)
	}
	if s.TaskFence != nil {
		return s.TaskFence.WithTaskExclusive(s.workerCtx, run.TaskID, operation)
	}
	return operation(nil)
}

func (s *Service) markStarted(run ManualTransferRun) (ManualTransferRun, error) {
	now := s.now()
	var started ManualTransferRun
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&ManualTransferRun{}).Where("id = ? AND task_id = ? AND state = ? AND revision = ?", run.ID, run.TaskID, ManualRunStateAnalyzing, run.Revision).Updates(map[string]interface{}{"analysis_started_at": now, "revision": gorm.Expr("revision + 1")})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevisionConflict
		}
		if err := tx.First(&started, run.ID).Error; err != nil {
			return err
		}
		if s.BeforeStartEvent != nil {
			if err := s.BeforeStartEvent(tx); err != nil {
				return err
			}
		}
		return createEvent(tx, started, ManualRunEventAnalysisStarted, ManualRunStateAnalyzing, ManualRunStateAnalyzing, "analysis worker started")
	})
	return started, err
}

func (s *Service) performAnalysis(ctx context.Context, run ManualTransferRun) error {
	firstGeneration := int64(1)
	secondGeneration := int64(2)
	if err := s.persistGeneration(ctx, run, firstGeneration); err != nil {
		_ = s.deleteRunFiles(run.ID)
		return err
	}
	if err := s.persistGeneration(ctx, run, secondGeneration); err != nil {
		_ = s.deleteRunFiles(run.ID)
		return err
	}
	first, err := s.snapshotAggregates(run.ID, firstGeneration)
	if err != nil {
		_ = s.deleteRunFiles(run.ID)
		return err
	}
	second, err := s.snapshotAggregates(run.ID, secondGeneration)
	if err != nil {
		_ = s.deleteRunFiles(run.ID)
		return err
	}
	if first != second {
		_ = s.deleteRunFiles(run.ID)
		return errors.New("source changed between canonical analysis passes")
	}
	return s.completeRun(run, second.Digest, second.Count, second.Bytes, secondGeneration)
}

func (s *Service) persistGeneration(ctx context.Context, run ManualTransferRun, generation int64) error {
	root, err := quota.OpenSourceRoot(run.SourcePath)
	if err != nil {
		return fmt.Errorf("source root unavailable: %w", err)
	}
	rootMatches := root.Device == run.SourceRootDevice && root.Inode == run.SourceRootInode
	_ = root.Close()
	if !rootMatches {
		return errors.New("source root identity changed during analysis")
	}
	batch := make([]ManualRunFile, 0, manualRunFileBatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := s.DB.CreateInBatches(batch, manualRunFileBatchSize).Error; err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	if err := s.Scanner.StreamContext(ctx, run.SourcePath, 0, func(snapshot quota.LocalSnapshot) error {
		if snapshot.RootDevice != run.SourceRootDevice || snapshot.RootInode != run.SourceRootInode {
			return errors.New("source root identity changed during analysis")
		}
		if err := quota.ValidateRelativePath(snapshot.RelativePath); err != nil {
			return err
		}
		if snapshot.SizeBytes < 0 || snapshot.Device <= 0 || snapshot.Inode <= 0 || strings.TrimSpace(snapshot.SnapshotKey) == "" {
			return fmt.Errorf("invalid source snapshot %q", snapshot.RelativePath)
		}
		batch = append(batch, ManualRunFile{RunID: run.ID, Generation: generation, RelativePath: snapshot.RelativePath, SnapshotKey: snapshot.SnapshotKey, SizeBytes: snapshot.SizeBytes, MtimeNS: snapshot.MtimeNS, Device: snapshot.Device, Inode: snapshot.Inode})
		if len(batch) == manualRunFileBatchSize {
			return flush()
		}
		return nil
	}); err != nil {
		return err
	}
	return flush()
}

type snapshotAggregate struct {
	Digest string
	Count  int64
	Bytes  int64
}

func (s *Service) snapshotAggregates(runID uint, generation int64) (snapshotAggregate, error) {
	digest := sha256.New()
	result := snapshotAggregate{}
	lastPath := ""
	for {
		var files []ManualRunFile
		query := s.DB.Where("run_id = ? AND generation = ?", runID, generation)
		if lastPath != "" {
			query = query.Where("relative_path > ?", lastPath)
		}
		if err := query.Order("relative_path ASC").Limit(manualRunFileBatchSize).Find(&files).Error; err != nil {
			return snapshotAggregate{}, err
		}
		for _, file := range files {
			if err := appendDigestRecord(digest, file); err != nil {
				return snapshotAggregate{}, err
			}
			if file.SizeBytes < 0 || file.SizeBytes > int64(^uint64(0)>>1)-result.Bytes {
				return snapshotAggregate{}, errors.New("snapshot byte total overflows int64")
			}
			result.Bytes += file.SizeBytes
			result.Count++
			lastPath = file.RelativePath
		}
		if len(files) < manualRunFileBatchSize {
			break
		}
	}
	result.Digest = hex.EncodeToString(digest.Sum(nil))
	return result, nil
}

func (s *Service) deleteRunFiles(runID uint) error {
	return s.DB.Where("run_id = ?", runID).Delete(&ManualRunFile{}).Error
}

func (s *Service) completeRun(run ManualTransferRun, digest string, count, total, generation int64) error {
	now := s.now()
	return s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("run_id = ? AND generation <> ?", run.ID, generation).Delete(&ManualRunFile{}).Error; err != nil {
			return err
		}
		if err := tx.Model(&ManualRunFile{}).Where("run_id = ? AND generation = ? AND activated_at IS NULL", run.ID, generation).Update("activated_at", now).Error; err != nil {
			return err
		}
		result := tx.Model(&ManualTransferRun{}).Where("id = ? AND task_id = ? AND state = ? AND revision = ?", run.ID, run.TaskID, ManualRunStateAnalyzing, run.Revision).Updates(map[string]interface{}{"state": ManualRunStateAnalyzed, "revision": gorm.Expr("revision + 1"), "snapshot_digest": digest, "snapshot_count": count, "snapshot_bytes": total, "snapshot_generation": generation, "analyzed_at": now, "last_error": ""})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevisionConflict
		}
		return createEvent(tx, run, ManualRunEventAnalysisCompleted, ManualRunStateAnalyzing, ManualRunStateAnalyzed, fmt.Sprintf("analysis completed: %d files, %d bytes", count, total))
	})
}

func (s *Service) failRun(run ManualTransferRun, message, eventType, actorIdentity, actorType string) error {
	now := s.now()
	message = sanitizeMessage(message)
	return s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("run_id = ?", run.ID).Delete(&ManualRunFile{}).Error; err != nil {
			return err
		}
		result := tx.Model(&ManualTransferRun{}).Where("id = ? AND state = ? AND revision = ?", run.ID, ManualRunStateAnalyzing, run.Revision).Updates(map[string]interface{}{"state": ManualRunStateAnalysisFailed, "revision": gorm.Expr("revision + 1"), "failed_at": now, "last_error": message})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevisionConflict
		}
		return createEventAs(tx, run, eventType, ManualRunStateAnalyzing, ManualRunStateAnalysisFailed, message, actorIdentity, actorType)
	})
}

func (s *Service) failRunByID(runID uint, message string) error {
	message = sanitizeMessage(message)
	for attempt := 0; attempt < 5; attempt++ {
		var run ManualTransferRun
		if err := s.DB.First(&run, runID).Error; err != nil {
			return err
		}
		if run.State != ManualRunStateAnalyzing {
			return nil
		}
		if err := s.failRun(run, message, ManualRunEventAnalysisFailed, "manual-transfer-system", "system"); err == nil {
			return nil
		} else if !errors.Is(err, ErrRevisionConflict) {
			if attempt == 4 {
				return err
			}
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return ErrRevisionConflict
}

func (s *Service) ListRuns(taskID uint) ([]ManualTransferRun, error) {
	if _, err := s.GetTask(taskID); err != nil {
		return nil, err
	}
	var runs []ManualTransferRun
	if err := s.DB.Where("task_id = ?", taskID).Order("id DESC").Limit(100).Find(&runs).Error; err != nil {
		return nil, err
	}
	return runs, nil
}

func (s *Service) GetTask(taskID uint) (models.Task, error) {
	var task models.Task
	if err := s.DB.First(&task, taskID).Error; err != nil {
		return task, err
	}
	if err := requireManualTask(&task); err != nil {
		return task, err
	}
	return task, nil
}

func (s *Service) GetRun(runID uint) (ManualTransferRun, error) {
	var run ManualTransferRun
	if err := s.DB.First(&run, runID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return run, ErrRunNotFound
		}
		return run, err
	}
	var task models.Task
	if err := s.DB.First(&task, run.TaskID).Error; err != nil {
		return ManualTransferRun{}, err
	}
	if err := requireManualTask(&task); err != nil {
		return ManualTransferRun{}, err
	}
	return run, nil
}

func (s *Service) GetRunAccounts(runID uint) ([]ManualRunAccount, error) {
	if _, err := s.GetRun(runID); err != nil {
		return nil, err
	}
	var accounts []ManualRunAccount
	if err := s.DB.Where("run_id = ?", runID).Order("position ASC, id ASC").Find(&accounts).Error; err != nil {
		return nil, err
	}
	return accounts, nil
}

type AccountPage struct {
	Accounts   []ManualRunAccount `json:"accounts"`
	NextCursor string             `json:"next_cursor,omitempty"`
	HasMore    bool               `json:"has_more"`
	Limit      int                `json:"limit"`
}

func (s *Service) ListRunAccounts(runID uint, cursor string, limit int) (AccountPage, error) {
	if limit <= 0 {
		limit = ManualAccountPageLimit
	}
	if limit > ManualAccountPageLimit {
		return AccountPage{}, errors.New("account response limit exceeds the maximum")
	}
	if _, err := s.GetRun(runID); err != nil {
		return AccountPage{}, err
	}
	parsed := accountCursor{}
	if cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(decoded, &parsed) != nil || parsed.ID == 0 {
			return AccountPage{}, errors.New("invalid account cursor")
		}
	}
	query := s.DB.Where("run_id = ?", runID)
	if cursor != "" {
		query = query.Where("position > ? OR (position = ? AND id > ?)", parsed.Position, parsed.Position, parsed.ID)
	}
	var accounts []ManualRunAccount
	if err := query.Order("position ASC, id ASC").Limit(limit + 1).Find(&accounts).Error; err != nil {
		return AccountPage{}, err
	}
	for _, account := range accounts {
		if len(account.AccountIdentity) > manualStringLimit || len(account.RemoteName) > manualStringLimit || len(account.ConfigIdentity) > manualStringLimit {
			return AccountPage{}, errors.New("stored account identity exceeds response bounds")
		}
	}
	page := AccountPage{Accounts: accounts, Limit: limit}
	if len(accounts) > limit {
		page.HasMore = true
		page.Accounts = accounts[:limit]
		last := page.Accounts[len(page.Accounts)-1]
		encoded, _ := json.Marshal(accountCursor{Position: last.Position, ID: last.ID})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return page, nil
}

type accountCursor struct {
	Position int  `json:"position"`
	ID       uint `json:"id"`
}

func (s *Service) GetRunAccountsBounded(runID uint, limit int) ([]ManualRunAccount, bool, error) {
	page, err := s.ListRunAccounts(runID, "", limit)
	if err != nil {
		return nil, false, err
	}
	return page.Accounts, page.HasMore, nil
}

func (s *Service) ListFiles(runID uint, cursor string, limit int) (FilePage, error) {
	return s.ListFilesFiltered(runID, cursor, limit, "", "", "")
}

func (s *Service) ListFilesFiltered(runID uint, cursor string, limit int, assignment, reason, accountID string) (FilePage, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > manualRunFilePageLimit {
		return FilePage{}, errors.New("file response limit exceeds the maximum")
	}
	run, err := s.GetRun(runID)
	if err != nil {
		return FilePage{}, err
	}
	if isAllocationVisibleState(run.State) {
		return s.ListAllocationFilesFiltered(runID, cursor, limit, assignment, reason, accountID)
	}
	if assignment != "" || reason != "" || accountID != "" {
		return FilePage{}, ErrAllocationUnavailable
	}
	if run.State != ManualRunStateAnalyzed || run.SnapshotGeneration <= 0 {
		return FilePage{}, ErrSnapshotUnavailable
	}
	parsedCursor := fileCursor{Generation: run.SnapshotGeneration}
	if cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || string(decoded) == "" || json.Unmarshal(decoded, &parsedCursor) != nil || parsedCursor.Generation != run.SnapshotGeneration || parsedCursor.RelativePath == "" {
			return FilePage{}, errors.New("invalid files cursor")
		}
	}
	query := s.DB.Where("run_id = ? AND generation = ? AND activated_at IS NOT NULL", runID, run.SnapshotGeneration)
	if cursor != "" {
		query = query.Where("relative_path > ? OR (relative_path = ? AND id > ?)", parsedCursor.RelativePath, parsedCursor.RelativePath, parsedCursor.ID)
	}
	var files []ManualRunFile
	if err := query.Order("relative_path ASC, id ASC").Limit(limit + 1).Find(&files).Error; err != nil {
		return FilePage{}, err
	}
	page := FilePage{Limit: limit, Files: files}
	if len(files) > limit {
		page.HasMore = true
		page.Files = files[:limit]
		last := page.Files[len(page.Files)-1]
		encoded, _ := json.Marshal(fileCursor{Generation: run.SnapshotGeneration, RelativePath: last.RelativePath, ID: last.ID})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return page, nil
}

func isAllocationVisibleState(state string) bool {
	switch state {
	case ManualRunStateAllocated, ManualRunStateRunning, ManualRunStateSucceeded, ManualRunStateFailed, ManualRunStateCancelled, ManualRunStateNeedsAttention:
		return true
	default:
		return false
	}
}

type fileCursor struct {
	Generation       int64  `json:"generation"`
	RelativePath     string `json:"relative_path"`
	ID               uint   `json:"id"`
	AssignmentFilter string `json:"assignment,omitempty"`
	ReasonFilter     string `json:"reason,omitempty"`
	AccountIDFilter  uint   `json:"account_id,omitempty"`
}

func StatusForRun(run ManualTransferRun) RunStatus {
	return RunStatus{ID: run.ID, TaskID: run.TaskID, State: run.State, Revision: run.Revision, ManualInputRevision: run.ManualInputRevision, ManualConfigRevision: run.ManualConfigRevision, Terminal: IsTerminalState(run.State), Analyzing: run.State == ManualRunStateAnalyzing, Allocating: run.State == ManualRunStateAllocating, Allocated: run.State == ManualRunStateAllocated, Running: run.State == ManualRunStateRunning, Succeeded: run.State == ManualRunStateSucceeded, Failed: run.State == ManualRunStateFailed, Cancelled: run.State == ManualRunStateCancelled, NeedsAttention: run.State == ManualRunStateNeedsAttention, AllocationFailed: run.State == ManualRunStateAllocationFailed, NeedsExplicitReanalyze: run.State == ManualRunStateAnalysisFailed || run.State == ManualRunStateAllocationFailed, FailurePolicy: ManualRunFailurePolicy, SnapshotDigest: run.SnapshotDigest, SnapshotCount: run.SnapshotCount, SnapshotBytes: run.SnapshotBytes, AllocationVersion: run.AllocationVersion, AllocationGeneration: run.AllocationGeneration, AllocationDigest: run.AllocationDigest, AllocationCount: run.AllocationCount, AllocationBytes: run.AllocationBytes, AssignedCount: run.AssignedCount, AssignedBytes: run.AssignedBytes, AlreadyTransferredCount: run.AlreadyTransferredCount, AlreadyTransferredBytes: run.AlreadyTransferredBytes, UnassignedCount: run.UnassignedCount, UnassignedBytes: run.UnassignedBytes, OversizeCount: run.OversizeCount, OversizeBytes: run.OversizeBytes, AggregateCapacityCount: run.AggregateCapacityCount, AggregateCapacityBytes: run.AggregateCapacityBytes, LastError: SanitizeMessage(run.LastError), CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt, AnalysisStartedAt: run.AnalysisStartedAt, AnalyzedAt: run.AnalyzedAt, FailedAt: run.FailedAt}
}

func ValidatePublicRun(run ManualTransferRun) error {
	for _, value := range []string{run.SourcePath, run.DestinationPath, run.TransferMode, run.ConfigIdentity, run.ActorIdentity, run.ActorType, run.SnapshotDigest} {
		if len(value) > manualStringLimit || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("stored manual run string exceeds response bounds")
		}
	}
	if len(run.ActorIdentity) > manualActorStringLimit || len(run.ActorType) > manualActorStringLimit {
		return errors.New("stored manual actor exceeds response bounds")
	}
	return nil
}

func canonicalTaskConfig(task *models.Task) (string, error) {
	if task == nil {
		return "", errors.New("task is required")
	}
	raw := strings.TrimSpace(task.RcloneConfig)
	if raw == "" {
		raw = models.DefaultRcloneConfigPath
	}
	identity, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	identity = filepath.Clean(identity)
	if len(identity) > manualStringLimit || strings.ContainsAny(identity, "\x00\r\n") {
		return "", errors.New("task configuration identity is invalid")
	}
	return identity, nil
}

func requireManualTask(task *models.Task) error {
	if task == nil || task.ID == 0 {
		return gorm.ErrRecordNotFound
	}
	if task.TaskType != models.TaskTypeManual || (strings.TrimSpace(task.ManualStrategy) != "" && task.ManualStrategy != models.ManualStrategyAllocation) {
		return ErrNotManualTask
	}
	return nil
}

func normalizedManualInputRevision(value int64) int64 {
	if value < 1 {
		return 1
	}
	return value
}

func manualConfigFingerprint(task *models.Task, accounts []frozenAccount) string {
	config, _ := canonicalTaskConfig(task)
	destination := manualTaskDestination(task)
	value, _ := json.Marshal(struct {
		SourcePath      string          `json:"source_path"`
		DestinationPath string          `json:"destination_path"`
		TransferMode    string          `json:"transfer_mode"`
		ConfigIdentity  string          `json:"config_identity"`
		Accounts        []frozenAccount `json:"accounts"`
	}{SourcePath: task.SourceDir, DestinationPath: destination, TransferMode: task.TransferMode, ConfigIdentity: config, Accounts: accounts})
	return fingerprintBytes(string(value))
}

func manualTaskDestination(task *models.Task) string {
	if task == nil {
		return ""
	}
	if strings.TrimSpace(task.RemoteName) == "" {
		return task.RemoteDir
	}
	return strings.TrimSpace(task.RemoteName) + ":" + task.RemoteDir
}

func manualDestinationMatches(value string, task *models.Task) bool {
	if task == nil {
		return false
	}
	return value == task.RemoteDir
}

func (s *Service) resolveAccounts(inputs []AccountInput, defaultConfig string) ([]frozenAccount, error) {
	accounts := make([]frozenAccount, 0, len(inputs))
	seenIDs := make(map[uint]struct{}, len(inputs))
	seenKeys := make(map[string]struct{}, len(inputs))
	seenConfigRemotes := make(map[string]struct{}, len(inputs))
	for position, input := range inputs {
		accountID := input.AccountID
		if accountID == 0 {
			accountID = input.ID
		}
		if accountID == 0 {
			return nil, fmt.Errorf("account at position %d must identify a durable quota account", position)
		}
		if _, exists := seenIDs[accountID]; exists {
			return nil, fmt.Errorf("duplicate account id %d", accountID)
		}
		var account models.QuotaAccount
		if err := s.DB.First(&account, accountID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrAccountNotFound
			}
			return nil, err
		}
		if !account.Enabled {
			return nil, fmt.Errorf("account %d is disabled", accountID)
		}
		identity := strings.TrimSpace(account.QuotaKey)
		remote := strings.TrimSpace(account.RemoteName)
		config := strings.TrimSpace(account.ConfigIdentity)
		if config == "" {
			config = defaultConfig
		}
		if identity == "" || remote == "" || config == "" {
			return nil, fmt.Errorf("account %d has incomplete trusted identity", accountID)
		}
		if len(identity) > manualStringLimit || len(remote) > manualStringLimit || len(config) > manualStringLimit || strings.ContainsAny(identity+remote+config, "\x00\r\n") {
			return nil, fmt.Errorf("account %d has an invalid trusted identity", accountID)
		}
		if _, exists := seenKeys[identity]; exists {
			return nil, fmt.Errorf("duplicate account identity %q", identity)
		}
		configRemote := config + "\x00" + remote
		if _, exists := seenConfigRemotes[configRemote]; exists {
			return nil, fmt.Errorf("duplicate account config and remote pair %q/%q", config, remote)
		}
		seenIDs[accountID] = struct{}{}
		seenKeys[identity] = struct{}{}
		seenConfigRemotes[configRemote] = struct{}{}
		accounts = append(accounts, frozenAccount{Position: position, AccountID: accountID, AccountIdentity: identity, RemoteName: remote, ConfigIdentity: config})
	}
	return accounts, nil
}

type frozenAccount struct {
	Position        int    `json:"position"`
	AccountID       uint   `json:"account_id,omitempty"`
	AccountIdentity string `json:"account_identity"`
	RemoteName      string `json:"remote_name,omitempty"`
	ConfigIdentity  string `json:"config_identity,omitempty"`
}

type frozenInput struct {
	TaskID              uint            `json:"task_id"`
	SourcePath          string          `json:"source_path"`
	DestinationPath     string          `json:"destination_path"`
	TransferMode        string          `json:"transfer_mode"`
	ConfigIdentity      string          `json:"config_identity"`
	InputRevision       int64           `json:"input_revision"`
	ConfigRevision      int64           `json:"config_revision"`
	ConfigFingerprint   string          `json:"config_fingerprint"`
	Accounts            []frozenAccount `json:"accounts"`
	PredecessorRunID    *uint           `json:"predecessor_run_id,omitempty"`
	PredecessorRevision *int64          `json:"predecessor_revision,omitempty"`
}

func (f frozenInput) Marshal() (string, error) {
	data, err := json.Marshal(f)
	return string(data), err
}

func samePredecessor(requestRunID *uint, requestRevision *int64, storedRunID *uint, storedRevision *int64) bool {
	if requestRunID == nil || requestRevision == nil || storedRunID == nil || storedRevision == nil {
		return requestRunID == nil && requestRevision == nil && storedRunID == nil && storedRevision == nil
	}
	return *requestRunID == *storedRunID && *requestRevision == *storedRevision
}

func validateAnalyzeRequest(request *AnalyzeRequest) error {
	request.SourcePath = strings.TrimSpace(request.SourcePath)
	request.DestinationPath = strings.TrimSpace(request.DestinationPath)
	request.TransferMode = strings.ToLower(strings.TrimSpace(request.TransferMode))
	request.ConfigIdentity = strings.TrimSpace(request.ConfigIdentity)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.SourcePath == "" || len(request.SourcePath) > manualStringLimit || !filepath.IsAbs(request.SourcePath) || filepath.Clean(request.SourcePath) != request.SourcePath {
		return errors.New("source_path must be an absolute clean directory path")
	}
	if request.DestinationPath == "" || len(request.DestinationPath) > manualStringLimit || strings.ContainsAny(request.DestinationPath, "\x00\r\n") {
		return errors.New("destination_path is required and must not contain control characters")
	}
	if request.TransferMode != models.TransferModeCopy && request.TransferMode != models.TransferModeMove {
		return fmt.Errorf("unsupported transfer mode %q", request.TransferMode)
	}
	if len(request.ConfigIdentity) > manualStringLimit || strings.ContainsAny(request.ConfigIdentity, "\x00\r\n") {
		return errors.New("config_identity must not contain control characters")
	}
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 256 || strings.ContainsAny(request.IdempotencyKey, "\x00\r\n") {
		return errors.New("idempotency key is required")
	}
	if request.ActorIdentity == "" {
		request.ActorIdentity = "unknown-operator"
	}
	if request.ActorType == "" {
		request.ActorType = "admin_session"
	}
	if len(request.ActorIdentity) > manualActorStringLimit || len(request.ActorType) > manualActorStringLimit {
		return errors.New("actor metadata is too long")
	}
	if len(request.Accounts) > ManualMaxAccountInputs {
		return fmt.Errorf("account input count exceeds the technical maximum of %d", ManualMaxAccountInputs)
	}
	for _, input := range request.Accounts {
		for _, value := range []string{input.AccountIdentity, input.Identity, input.AccountKey, input.QuotaKey, input.RemoteName, input.ConfigIdentity} {
			if len(value) > manualStringLimit || strings.ContainsAny(value, "\x00\r\n") {
				return errors.New("account input string exceeds bounds")
			}
		}
	}
	if (request.ExpectedRunID == nil) != (request.ExpectedRevision == nil) {
		return ErrRevisionConflict
	}
	return nil
}

func fingerprintBytes(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func appendDigestRecord(hash interface{ Write([]byte) (int, error) }, file ManualRunFile) error {
	for _, value := range []string{file.RelativePath, strconv.FormatInt(file.SizeBytes, 10), strconv.FormatInt(file.MtimeNS, 10), strconv.FormatInt(file.Device, 10), strconv.FormatInt(file.Inode, 10), file.SnapshotKey} {
		if _, err := hash.Write([]byte(value)); err != nil {
			return err
		}
		if _, err := hash.Write([]byte{0}); err != nil {
			return err
		}
	}
	return nil
}

func createEvent(tx *gorm.DB, run ManualTransferRun, eventType, fromState, toState, details string) error {
	return createEventAs(tx, run, eventType, fromState, toState, details, run.ActorIdentity, run.ActorType)
}

func createEventAs(tx *gorm.DB, run ManualTransferRun, eventType, fromState, toState, details, actorIdentity, actorType string) error {
	return tx.Create(&ManualRunEvent{RunID: run.ID, EventType: eventType, FromState: fromState, ToState: toState, ActorIdentity: actorIdentity, ActorType: actorType, Details: sanitizeMessage(details)}).Error
}

var credentialAssignmentPattern = regexp.MustCompile(`(?i)(password|token|secret|credential)([[:space:]]*[:=][[:space:]]*)("[^"]*"|'[^']*'|[^[:space:],;}]+)`)
var credentialFlagPattern = regexp.MustCompile(`(?i)(--?(?:password|token|secret|credential)[[:space:]]+)([^[:space:],;]+)`)
var credentialJSONPattern = regexp.MustCompile(`(?i)(password|token|secret|credential)(["'][[:space:]]*:[[:space:]]*["'])([^"']+)`)

const workerLogMarkerCarry = len("credential") - 1

type workerLogRedactor struct {
	mu        sync.Mutex
	buffer    string
	marker    string
	redacting bool
	quoted    bool
	escaped   bool
}

func newWorkerLogRedactor() *workerLogRedactor { return &workerLogRedactor{} }

func (r *workerLogRedactor) Feed(value string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buffer += value
	return r.process(false)
}

func (r *workerLogRedactor) Flush() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.process(true)
}

func (r *workerLogRedactor) process(final bool) string {
	var output strings.Builder
	for {
		if r.redacting {
			if r.consumeCredentialValue(&output) {
				continue
			}
			return output.String()
		}
		if r.marker != "" {
			separator, quoted, complete, valid := credentialSeparator(r.buffer, final)
			if !complete {
				return output.String()
			}
			if !valid {
				output.WriteString(r.marker)
				r.marker = ""
				continue
			}
			output.WriteString(r.marker)
			output.WriteString(separator)
			output.WriteString("[redacted]")
			r.marker = ""
			r.buffer = r.buffer[len(separator):]
			r.redacting = true
			r.quoted = quoted
			r.escaped = false
			continue
		}
		index, marker := findCredentialMarker(r.buffer)
		if index < 0 {
			if final {
				output.WriteString(r.buffer)
				r.buffer = ""
				return output.String()
			}
			safe := len(r.buffer) - workerLogMarkerCarry
			if safe <= 0 {
				return output.String()
			}
			output.WriteString(r.buffer[:safe])
			r.buffer = r.buffer[safe:]
			return output.String()
		}
		output.WriteString(r.buffer[:index])
		r.buffer = r.buffer[index+len(marker):]
		r.marker = marker
	}
}

func (r *workerLogRedactor) consumeCredentialValue(output *strings.Builder) bool {
	for index := 0; index < len(r.buffer); index++ {
		char := r.buffer[index]
		if r.quoted {
			if r.escaped {
				r.escaped = false
				continue
			}
			if char == '\\' {
				r.escaped = true
				continue
			}
			if char == '"' || char == '\'' {
				output.WriteByte(char)
				r.buffer = r.buffer[index+1:]
				r.redacting = false
				r.quoted = false
				return true
			}
			continue
		}
		if char == ' ' || char == '\t' || char == '\r' || char == '\n' || char == ',' || char == ';' || char == '}' || char == ']' {
			output.WriteByte(char)
			r.buffer = r.buffer[index+1:]
			r.redacting = false
			return true
		}
	}
	r.buffer = ""
	return false
}

func findCredentialMarker(value string) (int, string) {
	lower := strings.ToLower(value)
	markers := []string{"password", "credential", "token", "secret"}
	index := -1
	selected := ""
	for _, marker := range markers {
		if candidate := strings.Index(lower, marker); candidate >= 0 && (index < 0 || candidate < index) {
			index = candidate
			selected = value[candidate : candidate+len(marker)]
		}
	}
	return index, selected
}

func credentialSeparator(value string, final bool) (string, bool, bool, bool) {
	if value == "" {
		return "", false, final, final
	}
	index := 0
	for index < len(value) && (value[index] == ' ' || value[index] == '\t' || value[index] == '\r' || value[index] == '\n') {
		index++
	}
	if index == len(value) {
		return "", false, final, final
	}
	if value[index] == '"' || value[index] == '\'' {
		quote := value[index]
		index++
		for index < len(value) && (value[index] == ' ' || value[index] == '\t') {
			index++
		}
		if index == len(value) {
			return "", false, final, final
		}
		if value[index] != ':' {
			return "", false, true, false
		}
		index++
		for index < len(value) && (value[index] == ' ' || value[index] == '\t') {
			index++
		}
		if index == len(value) {
			return value[:index], false, final, final
		}
		if value[index] == quote {
			index++
			return value[:index], true, true, true
		}
		return value[:index], false, true, true
	}
	if value[index] == ':' || value[index] == '=' {
		index++
		for index < len(value) && (value[index] == ' ' || value[index] == '\t') {
			index++
		}
		if index == len(value) {
			return value[:index], false, final, final
		}
		if value[index] == '"' || value[index] == '\'' {
			index++
			return value[:index], true, true, true
		}
		return value[:index], false, true, true
	}
	if index > 0 {
		return value[:index], false, true, true
	}
	return "", false, true, false
}

func sanitizeWorkerLog(message string) string {
	message = strings.TrimSpace(message)
	message = credentialJSONPattern.ReplaceAllString(message, `${1}${2}[redacted]`)
	message = credentialAssignmentPattern.ReplaceAllString(message, `${1}${2}[redacted]`)
	return credentialFlagPattern.ReplaceAllString(message, `${1}[redacted]`)
}

func SanitizeMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > manualStringLimit {
		message = message[:manualStringLimit]
	}
	message = credentialJSONPattern.ReplaceAllString(message, `${1}${2}[redacted]`)
	message = credentialAssignmentPattern.ReplaceAllString(message, `${1}${2}[redacted]`)
	return credentialFlagPattern.ReplaceAllString(message, `${1}[redacted]`)
}

func sanitizeMessage(message string) string { return SanitizeMessage(message) }

func encodeCursor(path string) string { return base64.RawURLEncoding.EncodeToString([]byte(path)) }
