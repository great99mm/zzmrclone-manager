package manualtransfer

import "time"

const (
	PerAccountBudgetBytes int64 = 700_000_000_000
	PerRunBudgetBytes     int64 = 2_400_000_000_000

	ManualRunStateAnalyzing        = "analyzing"
	ManualRunStateAnalyzed         = "analyzed"
	ManualRunStateAnalysisFailed   = "analysis_failed"
	ManualRunStateAllocating       = "allocating"
	ManualRunStateAllocated        = "allocated"
	ManualRunStateAllocationFailed = "allocation_failed"
	ManualRunStateRunning          = "running"
	ManualRunStateSucceeded        = "succeeded"
	ManualRunStateFailed           = "failed"
	ManualRunStateCancelled        = "cancelled"
	ManualRunStateNeedsAttention   = "needs_attention"

	ManualRunEventRequested            = "analyze_requested"
	ManualRunEventAnalysisStarted      = "analysis_started"
	ManualRunEventAnalysisCompleted    = "analysis_completed"
	ManualRunEventAnalysisFailed       = "analysis_failed"
	ManualRunEventStartupTerminalized  = "startup_terminalized"
	ManualRunEventAllocateRequested    = "allocate_requested"
	ManualRunEventAllocationStarted    = "allocation_started"
	ManualRunEventAllocationCompleted  = "allocation_completed"
	ManualRunEventAllocationFailed     = "allocation_failed"
	ManualRunEventWorkersStarted       = "workers_started"
	ManualRunEventWorkersReconciled    = "workers_reconciled"
	ManualWorkerEventStarted           = "worker_started"
	ManualWorkerEventProcessStarted    = "process_started"
	ManualWorkerEventFinished          = "worker_finished"
	ManualWorkerEventCancelRequested   = "cancel_requested"
	ManualWorkerEventRetryRequested    = "retry_requested"
	ManualWorkerEventStartupReconciled = "startup_reconciled"
	ManualWorkerEventAttemptReconciled = "startup_attempt_reconciled"

	ManualRunFailurePolicy = "needs_explicit_reanalyze"
	manualRunFileBatchSize = 256
	manualRunQueueSize     = 64
	manualRunFilePageLimit = 500
	ManualAccountPageLimit = 100
	// ManualMaxAccountInputs is a transport/memory safety bound, not a business
	// account-count limit; allocation must still support every accepted entry.
	ManualMaxAccountInputs                        = 256
	ManualMaxAnalyzeRequestBytes            int64 = 1 << 20
	manualStringLimit                             = 4096
	manualActorStringLimit                        = 256
	ManualRunIdempotencyIndex                     = "uq_manual_transfer_runs_task_idempotency"
	ManualRunActiveIndex                          = "uq_manual_transfer_runs_task_active"
	ManualRunFilesRunPathIndex                    = "idx_manual_run_files_run_path"
	ManualRunFilesPathUniqueIndex                 = "uq_manual_run_files_run_path"
	ManualRunFilesSnapshotUniqueIndex             = "uq_manual_run_files_run_snapshot"
	ManualRunAllocationsRunPathIndex              = "idx_manual_run_allocations_run_path"
	ManualRunAllocationsPathUniqueIndex           = "uq_manual_run_allocations_run_generation_path"
	ManualRunAllocationsSnapshotUniqueIndex       = "uq_manual_run_allocations_run_generation_snapshot"
	ManualRunWorkersRunPositionIndex              = "uq_manual_run_workers_run_position"
	ManualRunWorkerAttemptsNumberIndex            = "uq_manual_worker_attempts_worker_number"
	ManualRunWorkerFilesPathIndex                 = "uq_manual_worker_files_worker_path"
	ManualRunWorkerProgressSequenceIndex          = "uq_manual_worker_progress_worker_sequence"
	ManualRunWorkerLogsWorkerIndex                = "uq_manual_worker_logs_worker"
	ManualRunEventMigrationReconciled             = "migration_reconciled_duplicate_analysis"
	ManualAllocationVersion                       = 1
	ManualAllocationReasonOversize                = "oversize"
	ManualAllocationReasonAggregateCapacity       = "aggregate_capacity"
	ManualAllocationReasonAccountCapacity         = "account_capacity"
	// ManualAllocationReasonNoFit is retained as a source-compatible alias.
	ManualAllocationReasonNoFit = ManualAllocationReasonAccountCapacity

	ManualWorkerStatePending        = "pending"
	ManualWorkerStateStarting       = "starting"
	ManualWorkerStateRunning        = "running"
	ManualWorkerStateReconciling    = "reconciling"
	ManualWorkerStateSucceeded      = "succeeded"
	ManualWorkerStateFailed         = "failed"
	ManualWorkerStateCancelled      = "cancelled"
	ManualWorkerStateUnknown        = "unknown"
	ManualWorkerStateNeedsAttention = "needs_attention"

	ManualWorkerFileStatePending  = "pending"
	ManualWorkerFileStateVerified = "verified"
	ManualWorkerFileStateFailed   = "failed"
	ManualWorkerFileStateUnknown  = "unknown"
)

// ManualTaskAccount is the durable ordered account template for a task. It is
// intentionally separate from the existing quota account tables; later phases
// may use this ordering to allocate transfer work.
type ManualTaskAccount struct {
	ID              uint      `json:"id" gorm:"primaryKey"`
	TaskID          uint      `json:"task_id" gorm:"not null;index;uniqueIndex:uq_manual_task_accounts_task_position,priority:1"`
	Position        int       `json:"position" gorm:"not null;uniqueIndex:uq_manual_task_accounts_task_position,priority:2;check:manual_task_account_position_nonnegative,position >= 0"`
	AccountID       uint      `json:"account_id" gorm:"not null;index"`
	AccountIdentity string    `json:"account_identity" gorm:"not null;check:manual_task_account_identity_nonempty,account_identity <> ''"`
	RemoteName      string    `json:"remote_name"`
	ConfigIdentity  string    `json:"config_identity"`
	Enabled         bool      `json:"enabled" gorm:"not null;default:true"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ManualTransferRun is immutable input plus a CAS-protected analysis state.
// Snapshot rows are stored separately so a large source tree never needs to be
// represented by one in-memory request or response.
type ManualTransferRun struct {
	ID                           uint       `json:"id" gorm:"primaryKey"`
	TaskID                       uint       `json:"task_id" gorm:"not null;index;uniqueIndex:uq_manual_transfer_runs_task_idempotency,priority:1"`
	State                        string     `json:"state" gorm:"not null;default:'analyzing';check:manual_transfer_run_state_valid,state IN ('analyzing','analyzed','analysis_failed','allocating','allocated','allocation_failed','running','succeeded','failed','cancelled','needs_attention');index:idx_manual_transfer_runs_task_created"`
	Revision                     int64      `json:"revision" gorm:"not null;default:1;check:manual_transfer_run_revision_positive,revision > 0"`
	ManualInputRevision          int64      `json:"manual_input_revision" gorm:"not null;default:1;check:manual_transfer_run_input_revision_positive,manual_input_revision >= 1"`
	ManualConfigRevision         int64      `json:"manual_config_revision" gorm:"not null;default:1;check:manual_transfer_run_config_revision_positive,manual_config_revision >= 1"`
	ManualConfigFingerprint      string     `json:"manual_config_fingerprint,omitempty" gorm:"not null;default:''"`
	ActorIdentity                string     `json:"actor_identity" gorm:"not null"`
	ActorType                    string     `json:"actor_type" gorm:"not null"`
	SourcePath                   string     `json:"source_path" gorm:"not null"`
	DestinationPath              string     `json:"destination_path" gorm:"not null"`
	TransferMode                 string     `json:"transfer_mode" gorm:"not null"`
	ConfigIdentity               string     `json:"config_identity" gorm:"not null"`
	FrozenInput                  string     `json:"-" gorm:"not null;type:text"`
	IdempotencyKey               string     `json:"-" gorm:"not null;uniqueIndex:uq_manual_transfer_runs_task_idempotency,priority:2"`
	RequestFingerprint           string     `json:"-" gorm:"not null"`
	PredecessorRunID             *uint      `json:"-" gorm:"index"`
	PredecessorRevision          *int64     `json:"-"`
	SourceRootDevice             int64      `json:"source_root_device" gorm:"not null;check:manual_transfer_run_root_device_positive,source_root_device > 0"`
	SourceRootInode              int64      `json:"source_root_inode" gorm:"not null;check:manual_transfer_run_root_inode_positive,source_root_inode > 0"`
	SnapshotDigest               string     `json:"snapshot_digest"`
	SnapshotCount                int64      `json:"snapshot_count" gorm:"check:manual_transfer_run_snapshot_count_nonnegative,snapshot_count >= 0"`
	SnapshotBytes                int64      `json:"snapshot_bytes" gorm:"check:manual_transfer_run_snapshot_bytes_nonnegative,snapshot_bytes >= 0"`
	SnapshotGeneration           int64      `json:"-" gorm:"not null;default:0;check:manual_transfer_run_snapshot_generation_nonnegative,snapshot_generation >= 0"`
	AnalysisStartedAt            *time.Time `json:"analysis_started_at"`
	AnalyzedAt                   *time.Time `json:"analyzed_at"`
	FailedAt                     *time.Time `json:"failed_at"`
	LastError                    string     `json:"-" gorm:"type:text"`
	AllocationVersion            int        `json:"allocation_version" gorm:"not null;default:0"`
	AllocationGeneration         int64      `json:"-" gorm:"not null;default:0;check:manual_transfer_run_allocation_generation_nonnegative,allocation_generation >= 0"`
	AllocationDigest             string     `json:"allocation_digest"`
	AllocationCount              int64      `json:"allocation_count" gorm:"check:manual_transfer_run_allocation_count_nonnegative,allocation_count >= 0"`
	AllocationBytes              int64      `json:"allocation_bytes" gorm:"check:manual_transfer_run_allocation_bytes_nonnegative,allocation_bytes >= 0"`
	AssignedCount                int64      `json:"assigned_count" gorm:"check:manual_transfer_run_assigned_count_nonnegative,assigned_count >= 0"`
	AssignedBytes                int64      `json:"assigned_bytes" gorm:"check:manual_transfer_run_assigned_bytes_nonnegative,assigned_bytes >= 0"`
	UnassignedCount              int64      `json:"unassigned_count" gorm:"check:manual_transfer_run_unassigned_count_nonnegative,unassigned_count >= 0"`
	UnassignedBytes              int64      `json:"unassigned_bytes" gorm:"check:manual_transfer_run_unassigned_bytes_nonnegative,unassigned_bytes >= 0"`
	OversizeCount                int64      `json:"oversize_count" gorm:"check:manual_transfer_run_oversize_count_nonnegative,oversize_count >= 0"`
	OversizeBytes                int64      `json:"oversize_bytes" gorm:"check:manual_transfer_run_oversize_bytes_nonnegative,oversize_bytes >= 0"`
	AggregateCapacityCount       int64      `json:"aggregate_capacity_count" gorm:"check:manual_transfer_run_aggregate_count_nonnegative,aggregate_capacity_count >= 0"`
	AggregateCapacityBytes       int64      `json:"aggregate_capacity_bytes" gorm:"check:manual_transfer_run_aggregate_bytes_nonnegative,aggregate_capacity_bytes >= 0"`
	AccountCapacityCount         int64      `json:"account_capacity_count" gorm:"check:manual_transfer_run_account_capacity_count_nonnegative,account_capacity_count >= 0"`
	AccountCapacityBytes         int64      `json:"account_capacity_bytes" gorm:"check:manual_transfer_run_account_capacity_bytes_nonnegative,account_capacity_bytes >= 0"`
	AllocationIdempotencyKey     string     `json:"-"`
	AllocationRequestFingerprint string     `json:"-"`
	WorkerStartIdempotencyKey    string     `json:"-"`
	WorkerStartFingerprint       string     `json:"-"`
	WorkerStartedAt              *time.Time `json:"started_at,omitempty"`
	CreatedAt                    time.Time  `json:"created_at" gorm:"index:idx_manual_transfer_runs_task_created"`
	UpdatedAt                    time.Time  `json:"updated_at"`
}

// ManualRunAccount freezes the ordered account inputs at analyze request time.
type ManualRunAccount struct {
	ID              uint      `json:"id" gorm:"primaryKey"`
	RunID           uint      `json:"run_id" gorm:"not null;index;uniqueIndex:uq_manual_run_accounts_run_position,priority:1"`
	Position        int       `json:"position" gorm:"not null;uniqueIndex:uq_manual_run_accounts_run_position,priority:2;check:manual_run_account_position_nonnegative,position >= 0"`
	AccountID       uint      `json:"account_id"`
	AccountIdentity string    `json:"account_identity" gorm:"not null;check:manual_run_account_identity_nonempty,account_identity <> ''"`
	RemoteName      string    `json:"remote_name"`
	ConfigIdentity  string    `json:"config_identity"`
	AllocatedCount  int64     `json:"allocated_count" gorm:"check:manual_run_account_allocated_count_nonnegative,allocated_count >= 0"`
	AllocatedBytes  int64     `json:"allocated_bytes" gorm:"check:manual_run_account_allocated_bytes_nonnegative,allocated_bytes >= 0"`
	CreatedAt       time.Time `json:"created_at"`
}

// ManualRunAllocation is an immutable preview row. Rows are inserted under an
// inactive generation and become visible only when the generation is activated
// with the run state transition to allocated.
type ManualRunAllocation struct {
	ID               uint       `json:"id" gorm:"primaryKey"`
	RunID            uint       `json:"run_id" gorm:"not null;index:idx_manual_run_allocations_run_path,priority:1;uniqueIndex:uq_manual_run_allocations_run_generation_path,priority:1;uniqueIndex:uq_manual_run_allocations_run_generation_snapshot,priority:1"`
	Generation       int64      `json:"-" gorm:"not null;index:idx_manual_run_allocations_run_path,priority:2;uniqueIndex:uq_manual_run_allocations_run_generation_path,priority:2;uniqueIndex:uq_manual_run_allocations_run_generation_snapshot,priority:2;check:manual_run_allocation_generation_positive,generation > 0"`
	RelativePath     string     `json:"relative_path" gorm:"not null;index:idx_manual_run_allocations_run_path,priority:3;uniqueIndex:uq_manual_run_allocations_run_generation_path,priority:3"`
	SnapshotKey      string     `json:"snapshot_key" gorm:"not null;uniqueIndex:uq_manual_run_allocations_run_generation_snapshot,priority:3"`
	SizeBytes        int64      `json:"size_bytes" gorm:"not null;check:manual_run_allocation_size_nonnegative,size_bytes >= 0"`
	AccountPosition  *int       `json:"account_position,omitempty"`
	AccountID        uint       `json:"account_id,omitempty"`
	AccountIdentity  string     `json:"account_identity,omitempty"`
	UnassignedReason string     `json:"unassigned_reason,omitempty"`
	ActivatedAt      *time.Time `json:"-"`
	CreatedAt        time.Time  `json:"created_at"`
}

// ManualRunFile is an immutable regular-file snapshot.
type ManualRunFile struct {
	ID               uint       `json:"id" gorm:"primaryKey"`
	RunID            uint       `json:"run_id" gorm:"not null;index:idx_manual_run_files_run_path,priority:1;uniqueIndex:uq_manual_run_files_run_path,priority:1;uniqueIndex:uq_manual_run_files_run_snapshot,priority:1"`
	Generation       int64      `json:"-" gorm:"not null;default:1;index:idx_manual_run_files_run_path,priority:2;uniqueIndex:uq_manual_run_files_run_path,priority:2;uniqueIndex:uq_manual_run_files_run_snapshot,priority:2;check:manual_run_file_generation_positive,generation > 0"`
	RelativePath     string     `json:"relative_path" gorm:"not null;index:idx_manual_run_files_run_path,priority:3;uniqueIndex:uq_manual_run_files_run_path,priority:3"`
	SnapshotKey      string     `json:"snapshot_key" gorm:"not null;index;uniqueIndex:uq_manual_run_files_run_snapshot,priority:3"`
	SizeBytes        int64      `json:"size_bytes" gorm:"not null;check:manual_run_file_size_nonnegative,size_bytes >= 0"`
	MtimeNS          int64      `json:"mtime_ns" gorm:"not null"`
	Device           int64      `json:"device" gorm:"not null"`
	Inode            int64      `json:"inode" gorm:"not null"`
	CreatedAt        time.Time  `json:"created_at"`
	ActivatedAt      *time.Time `json:"-"`
	AccountPosition  *int       `json:"account_position,omitempty"`
	AccountID        uint       `json:"account_id,omitempty"`
	AccountIdentity  string     `json:"account_identity,omitempty"`
	UnassignedReason string     `json:"unassigned_reason,omitempty"`
}

// ManualRunEvent is append-only audit history. Details are generated by the
// service and never contain request credentials.
type ManualRunEvent struct {
	ID            uint      `json:"id" gorm:"primaryKey"`
	RunID         uint      `json:"run_id" gorm:"not null;index"`
	EventType     string    `json:"event_type" gorm:"not null"`
	FromState     string    `json:"from_state"`
	ToState       string    `json:"to_state"`
	ActorIdentity string    `json:"actor_identity" gorm:"not null"`
	ActorType     string    `json:"actor_type" gorm:"not null"`
	Details       string    `json:"details" gorm:"type:text"`
	CreatedAt     time.Time `json:"created_at"`
}

// ManualRunWorker is the durable execution owner for one allocated account.
// The lease and process identity are persisted before a copy process is
// considered running, so cancellation and restart recovery can fail closed.
type ManualRunWorker struct {
	ID                  uint       `json:"id" gorm:"primaryKey"`
	RunID               uint       `json:"run_id" gorm:"not null;index;uniqueIndex:uq_manual_run_workers_run_position,priority:1"`
	AccountID           uint       `json:"account_id" gorm:"not null;index"`
	AccountPosition     int        `json:"account_position" gorm:"not null;uniqueIndex:uq_manual_run_workers_run_position,priority:2"`
	AccountIdentity     string     `json:"account_identity" gorm:"not null"`
	RemoteName          string     `json:"remote_name" gorm:"not null"`
	ConfigIdentity      string     `json:"config_identity" gorm:"not null"`
	State               string     `json:"state" gorm:"not null;default:'pending';index"`
	Actionability       string     `json:"actionability" gorm:"-"`
	AttemptNumber       int        `json:"attempt_number" gorm:"not null;default:1"`
	CurrentAttemptID    uint       `json:"current_attempt_id" gorm:"not null;index"`
	Revision            int64      `json:"revision" gorm:"not null;default:1"`
	LeaseToken          string     `json:"-"`
	LeaseUntil          *time.Time `json:"-"`
	ProcessID           int        `json:"-"`
	ProcessStartToken   string     `json:"-"`
	CancelRequested     bool       `json:"cancel_requested"`
	CancelRequestedAt   *time.Time `json:"cancel_requested_at,omitempty"`
	AssignedCount       int64      `json:"assigned_count"`
	AssignedBytes       int64      `json:"assigned_bytes"`
	CompletedCount      int64      `json:"completed_count"`
	CompletedBytes      int64      `json:"completed_bytes"`
	SpeedBytesPerSecond int64      `json:"speed_bytes_per_second"`
	ProgressPercent     float64    `json:"progress_percent"`
	CurrentRelativePath string     `json:"current_relative_path,omitempty"`
	LastProgressAt      *time.Time `json:"last_progress_at,omitempty"`
	StartedAt           *time.Time `json:"started_at,omitempty"`
	FinishedAt          *time.Time `json:"finished_at,omitempty"`
	LastError           string     `json:"last_error,omitempty" gorm:"type:text"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// ManualWorkerAttempt is append-only. A retry always creates a new row and
// never reuses the lease or process identity of an earlier attempt.
type ManualWorkerAttempt struct {
	ID                  uint       `json:"id" gorm:"primaryKey"`
	RunID               uint       `json:"run_id" gorm:"not null;index"`
	WorkerID            uint       `json:"worker_id" gorm:"not null;index;uniqueIndex:uq_manual_worker_attempts_worker_number,priority:1"`
	AttemptNumber       int        `json:"attempt_number" gorm:"not null;uniqueIndex:uq_manual_worker_attempts_worker_number,priority:2"`
	State               string     `json:"state" gorm:"not null;default:'pending';index"`
	LeaseToken          string     `json:"-"`
	LeaseUntil          *time.Time `json:"-"`
	ProcessID           int        `json:"-"`
	ProcessStartToken   string     `json:"-"`
	ManifestPath        string     `json:"-"`
	ManifestHash        string     `json:"-"`
	CancelRequested     bool       `json:"cancel_requested"`
	AssignedCount       int64      `json:"assigned_count"`
	AssignedBytes       int64      `json:"assigned_bytes"`
	CompletedCount      int64      `json:"completed_count"`
	CompletedBytes      int64      `json:"completed_bytes"`
	SpeedBytesPerSecond int64      `json:"speed_bytes_per_second"`
	ProgressPercent     float64    `json:"progress_percent"`
	StartedAt           *time.Time `json:"started_at,omitempty"`
	FinishedAt          *time.Time `json:"finished_at,omitempty"`
	ExitCode            *int       `json:"exit_code,omitempty"`
	LastError           string     `json:"last_error,omitempty" gorm:"type:text"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// ManualWorkerFile is the durable assignment and completion ledger for one
// worker. Its source identity comes from the immutable analyzed snapshot.
type ManualWorkerFile struct {
	ID           uint       `json:"id" gorm:"primaryKey"`
	RunID        uint       `json:"run_id" gorm:"not null;index"`
	WorkerID     uint       `json:"worker_id" gorm:"not null;index;uniqueIndex:uq_manual_worker_files_worker_path,priority:1"`
	AttemptID    uint       `json:"attempt_id" gorm:"not null;index"`
	RelativePath string     `json:"relative_path" gorm:"not null;uniqueIndex:uq_manual_worker_files_worker_path,priority:2"`
	SnapshotKey  string     `json:"snapshot_key" gorm:"not null"`
	SizeBytes    int64      `json:"size_bytes" gorm:"not null"`
	MtimeNS      int64      `json:"mtime_ns" gorm:"not null"`
	Device       int64      `json:"device" gorm:"not null"`
	Inode        int64      `json:"inode" gorm:"not null"`
	State        string     `json:"state" gorm:"not null;default:'pending';index"`
	VerifiedAt   *time.Time `json:"verified_at,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ManualWorkerEvent and ManualWorkerProgress are append-only operational
// history. They intentionally contain only redacted operator-visible text.
type ManualWorkerEvent struct {
	ID            uint      `json:"id" gorm:"primaryKey"`
	RunID         uint      `json:"run_id" gorm:"not null;index"`
	WorkerID      uint      `json:"worker_id" gorm:"not null;index"`
	AttemptID     uint      `json:"attempt_id" gorm:"index"`
	EventType     string    `json:"event_type" gorm:"not null"`
	FromState     string    `json:"from_state"`
	ToState       string    `json:"to_state"`
	ActorIdentity string    `json:"actor_identity"`
	ActorType     string    `json:"actor_type"`
	Details       string    `json:"details" gorm:"type:text"`
	CreatedAt     time.Time `json:"created_at"`
}

type ManualWorkerProgress struct {
	ID                  uint      `json:"id" gorm:"primaryKey"`
	RunID               uint      `json:"run_id" gorm:"not null;index"`
	WorkerID            uint      `json:"worker_id" gorm:"not null;index;uniqueIndex:uq_manual_worker_progress_worker_sequence,priority:1"`
	AttemptID           uint      `json:"attempt_id" gorm:"not null;index"`
	Sequence            int64     `json:"sequence" gorm:"not null;uniqueIndex:uq_manual_worker_progress_worker_sequence,priority:2"`
	State               string    `json:"state"`
	RelativePath        string    `json:"relative_path,omitempty"`
	CompletedCount      int64     `json:"completed_count"`
	CompletedBytes      int64     `json:"completed_bytes"`
	SpeedBytesPerSecond int64     `json:"speed_bytes_per_second"`
	ProgressPercent     float64   `json:"progress_percent"`
	CreatedAt           time.Time `json:"created_at"`
}

// ManualWorkerLog stores ownership metadata for the append-only file. The
// filesystem path is never serialized to an API response.
type ManualWorkerLog struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	RunID     uint      `json:"run_id" gorm:"not null;index"`
	WorkerID  uint      `json:"worker_id" gorm:"not null;uniqueIndex:uq_manual_worker_logs_worker"`
	Path      string    `json:"-" gorm:"not null"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func IsTerminalState(state string) bool {
	return state == ManualRunStateAnalyzed || state == ManualRunStateAnalysisFailed || state == ManualRunStateAllocated || state == ManualRunStateAllocationFailed || state == ManualRunStateSucceeded || state == ManualRunStateFailed || state == ManualRunStateCancelled || state == ManualRunStateNeedsAttention
}
