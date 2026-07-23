package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"gorm.io/gorm"
)

func IsValidOwnerToken(value string) bool {
	if len(value) != 48 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func IsValidLeaseToken(value string) bool { return IsValidOwnerToken(value) }

const (
	DefaultRotationQuotaLimitBytes int64 = 700 * 1024 * 1024 * 1024
	DefaultRcloneConfigPath              = "/root/.config/rclone/rclone.conf"

	BatchStatePlanned                = "planned"
	BatchStateReserved               = "reserved"
	BatchStateRunning                = "running"
	BatchStateUnknown                = "unknown"
	BatchStateReconciling            = "reconciling"
	BatchStateSucceeded              = "succeeded"
	BatchStateFailed                 = "failed"
	BatchStateCanceled               = "canceled"
	BatchStateExpired                = "expired"
	ReservationStateHeld             = "held"
	ReservationStateActive           = "active"
	ReservationStateCommitted        = "committed"
	ReservationStateUnknown          = "unknown"
	ReservationStateReleased         = "released"
	ReservationStateExpired          = "expired"
	BatchFileStateHeld               = "held"
	BatchFileStateActive             = "active"
	BatchFileStateVerified           = "verified"
	BatchFileStateUnknown            = "unknown"
	BatchFileStateFailed             = "failed"
	BatchFileStateCommitted          = "committed"
	TransferModeCopy                 = "copy"
	TransferModeMove                 = "move"
	CompletionEvidenceRemote         = "remote_verified"
	CompletionEvidenceLocal          = "local_move"
	ProactiveMoveSettingKey          = "proactive_move_enabled"
	ProactiveMoveSettingMigrationKey = "proactive_move_enabled_phase3_initialized"
	MoveHandoffVersion               = 1
	MoveHandoffReady                 = "ready"
	MoveHandoffQuarantined           = "quarantined"
	MoveHandoffRestored              = "restored"
	MoveHandoffMoved                 = "moved"
	MoveHandoffUnknown               = "unknown"
	MoveCompletionEvidenceVersion    = 1
	MoveResolutionResolving          = "resolving"
	MoveResolutionFrozen             = "frozen"
	ReserveClassNoFiles              = "no_files"
	ReserveClassReserved             = "reserved"
	ReserveClassBudgetExhausted      = "budget_exhausted"
	ReserveClassProviderBlocked      = "provider_blocked"
	ReserveClassDisabled             = "disabled"
	ReserveClassTaskCeiling          = "task_ceiling"
	ReserveClassOversize             = "oversize"
	ReserveClassNoFit                = "no_fit"
	ReserveClassActive               = "active"
	ReserveClassAccountBlocked       = "account_blocked"
	ReserveClassUnknown              = "unknown"
	ReserveClassFailed               = "failed"
	MaintenanceStateExhausted        = "exhausted"
	MaintenanceStateClaimed          = "claimed"
	MaintenanceStateRunning          = "running"
	MaintenanceStateSucceeded        = "succeeded"
	MaintenanceStateFailed           = "failed"
	MaintenanceStateUnknown          = "unknown"
	MaintenanceStateClosed           = "closed"
	MaintenanceReasonQuotaExhaustion = "quota_exhaustion"
	MaintenanceReasonManualMerge     = "manual_merge"
	DedupeStatePending               = "pending"
	DedupeStateRunning               = "running"
	DedupeStateSucceeded             = "succeeded"
	DedupeStateFailed                = "failed"
	DedupeStateUnknown               = "unknown"
	DedupeStateClaimed               = "claimed"
)

type DestinationScopeMaintenance struct {
	ID                     uint       `json:"id" gorm:"primaryKey"`
	DestinationScope       string     `json:"destination_scope" gorm:"uniqueIndex:uq_scope_maintenance_epoch"`
	Epoch                  int64      `json:"epoch" gorm:"uniqueIndex:uq_scope_maintenance_epoch"`
	OwnerTaskID            uint       `json:"owner_task_id"`
	FirstRemote            string     `json:"first_remote"`
	RemoteDir              string     `json:"remote_dir"`
	Reason                 string     `json:"reason" gorm:"not null;default:'quota_exhaustion';check:maintenance_reason_valid,reason IN ('quota_exhaustion','manual_merge')"`
	ResolvedConfigPath     string     `json:"-"`
	ResolvedConfigIdentity string     `json:"-"`
	CapacityWake           *time.Time `json:"-"`
	State                  string     `json:"state"`
	DedupeState            string     `json:"-"`
	LeaseToken             string     `json:"-"`
	LeaseUntil             *time.Time `json:"-"`
	ProcessID              int        `json:"-"`
	ProcessStartToken      string     `json:"-"`
	StartedAt              *time.Time `json:"started_at"`
	FinishedAt             *time.Time `json:"finished_at"`
	ExitCode               *int       `json:"exit_code"`
	Result                 string     `json:"result"`
	LastError              string     `json:"last_error"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
	Revision               int64      `json:"revision" gorm:"not null;default:1"`
}

// DestinationScopeCoordinator serializes scanner dispatch, manual maintenance
// claims, and the final quota reservation decision for one destination scope.
type DestinationScopeCoordinator struct {
	ID                 uint       `json:"id" gorm:"primaryKey"`
	DestinationScope   string     `json:"destination_scope" gorm:"uniqueIndex"`
	ScannerLeaseToken  string     `json:"-"`
	ScannerLeaseUntil  *time.Time `json:"-"`
	MaintenanceEpochID uint       `json:"maintenance_epoch_id"`
	Revision           int64      `json:"revision" gorm:"not null;default:1"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

func IsKnownMaintenanceReason(reason string) bool {
	return reason == MaintenanceReasonQuotaExhaustion || reason == MaintenanceReasonManualMerge
}

func IsKnownBatchState(state string) bool {
	switch state {
	case BatchStatePlanned, BatchStateReserved, BatchStateRunning, BatchStateUnknown, BatchStateReconciling,
		BatchStateSucceeded, BatchStateFailed, BatchStateCanceled, BatchStateExpired:
		return true
	default:
		return false
	}
}

func IsActiveBatchState(state string) bool {
	switch state {
	case BatchStatePlanned, BatchStateReserved, BatchStateRunning, BatchStateUnknown, BatchStateReconciling:
		return true
	default:
		return false
	}
}

func IsTerminalBatchState(state string) bool {
	switch state {
	case BatchStateSucceeded, BatchStateFailed, BatchStateCanceled, BatchStateExpired:
		return true
	default:
		return false
	}
}

func IsKnownBatchFileState(state string) bool {
	switch state {
	case "", BatchFileStateHeld, BatchFileStateActive, BatchFileStateVerified, BatchFileStateUnknown, BatchFileStateFailed, BatchFileStateCommitted:
		return true
	default:
		return false
	}
}

type Task struct {
	ID   uint   `json:"id" gorm:"primaryKey"`
	Name string `json:"name" gorm:"not null"`
	// Source: type determines how source_dir is interpreted
	//   "local"  → source_dir is a local filesystem path
	//   "remote" → source_dir is a rclone remote path (e.g. "op:/videos")
	SourceType string `json:"source_type" gorm:"default:local"`
	SourceDir  string `json:"source_dir" gorm:"not null"`
	// Destination: type determines how remote_name / remote_dir are interpreted
	//   "remote" → remote_name:remote_dir  (default, backward-compatible)
	//   "local"  → remote_dir is a local filesystem path, remote_name is ignored
	DestType   string `json:"dest_type" gorm:"default:remote"`
	RemoteName string `json:"remote_name"`
	RemoteDir  string `json:"remote_dir"`
	// Operation mode
	//   "move" → rclone move  (default)
	//   "copy" → rclone copy
	//   "sync" → rclone sync
	TransferMode string `json:"transfer_mode" gorm:"default:move"`
	// ---- memory-safe defaults ----
	// Old: transfers=16  => with buffer-size 512M that's 8GB RAM.
	// New: transfers=8   => with buffer-size 64M that's 512MB peak.
	// Users can still raise via UI up to the hard cap in router.go.
	Transfers int `json:"transfers" gorm:"default:8"`
	// checkers raised from 32 to 16 — still fast, far less RAM.
	Checkers     int    `json:"checkers" gorm:"default:16"`
	BindIP       string `json:"bind_ip"`
	RcloneConfig string `json:"rclone_config"`
	Enabled      bool   `json:"enabled" gorm:"default:true"`
	AutoDedupe   bool   `json:"auto_dedupe" gorm:"default:false"`
	MinAge       string `json:"min_age" gorm:"default:10s"`
	// drive-chunk-size: 256M -> 64M.  Still fast, 4x less RAM per transfer.
	DriveChunkSize string `json:"drive_chunk_size" gorm:"default:64M"`
	// buffer-size: 512M -> 64M.  THIS IS THE BIGGEST WIN.
	// 8 transfers * 64M = 512MB peak vs old 8GB.
	BufferSize       string         `json:"buffer_size" gorm:"default:64M"`
	Retries          int            `json:"retries" gorm:"default:3"`
	ScheduleEnabled  bool           `json:"schedule_enabled" gorm:"default:false"`
	ScheduleInterval int            `json:"schedule_interval" gorm:"default:15"`
	WatchEnabled     bool           `json:"watch_enabled" gorm:"default:true"`
	QBEnabled        bool           `json:"qb_enabled" gorm:"default:false"`
	QBURL            string         `json:"qb_url" gorm:"default:''"`
	QBUsername       string         `json:"qb_username" gorm:"default:''"`
	QBPassword       string         `json:"qb_password" gorm:"default:''"`
	QBPollInterval   int            `json:"qb_poll_interval" gorm:"default:60"`
	QBDeleteFiles    bool           `json:"qb_delete_files" gorm:"default:false"`
	Status           string         `json:"status" gorm:"default:idle"`
	LastRun          *time.Time     `json:"last_run"`
	LastError        string         `json:"last_error"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	DeletedAt        gorm.DeletedAt `json:"-" gorm:"index"`

	// OpenList refresh configuration
	OpenlistEnabled  bool   `json:"openlist_enabled" gorm:"default:false"`
	OpenlistURL      string `json:"openlist_url" gorm:"default:''"`
	OpenlistMapping  string `json:"openlist_mapping" gorm:"default:''"`
	OpenlistToken    string `json:"openlist_token" gorm:"default:''"`
	OpenlistConfigID uint   `json:"openlist_config_id" gorm:"default:0"`
	// Optional: explicit OpenList refresh directory (overrides auto-extraction)
	OpenlistRefreshDir string `json:"openlist_refresh_dir" gorm:"default:''"`

	// Quick task: created from file browser, auto-hides after completion
	IsQuickTask bool `json:"is_quick_task" gorm:"default:false"`

	// TaskType controls execution behavior:
	//   "normal"   -> existing single-remote behavior
	//   "rotation" -> sequentially rotates destination remotes on 403/429/quota errors
	TaskType string `json:"task_type" gorm:"default:normal"`
	// RotationStrategy isolates legacy error-driven rotation from future proactive quota rotation.
	RotationStrategy        string `json:"rotation_strategy" gorm:"default:legacy_error"`
	RotationQuotaLimitBytes int64  `json:"rotation_quota_limit_bytes"`
	RotationQuotaKeys       string `json:"rotation_quota_keys" gorm:"type:text"`
	// RotationRemotes stores a JSON string array of rclone remote names, e.g. ["a","b","c"].
	RotationRemotes                 string     `json:"rotation_remotes" gorm:"type:text"`
	RotationMaxRounds               int        `json:"rotation_max_rounds" gorm:"default:3"`
	RotationResumeTime              string     `json:"rotation_resume_time" gorm:"default:'01:00'"`
	RotationCurrentIndex            int        `json:"rotation_current_index" gorm:"default:0"`
	RotationCurrentRound            int        `json:"rotation_current_round" gorm:"default:0"`
	RotationPausedUntil             *time.Time `json:"rotation_paused_until"`
	RotationRescanPending           bool       `json:"rotation_rescan_pending" gorm:"default:false"`
	RotationRescanGeneration        uint64     `json:"rotation_rescan_generation" gorm:"default:0"`
	RotationRescanHandledGeneration uint64     `json:"rotation_rescan_handled_generation" gorm:"default:0"`
	RotationLastScanAt              *time.Time `json:"rotation_last_scan_at"`
	RotationQuotaWakeAt             *time.Time `json:"rotation_quota_wake_at"`
	RotationWakeClaimToken          string     `json:"rotation_wake_claim_token" gorm:"default:''"`
	RotationWakeClaimUntil          *time.Time `json:"rotation_wake_claim_until"`
	RotationStopRequested           bool       `json:"rotation_stop_requested" gorm:"default:false"`
	RotationStopGeneration          uint64     `json:"rotation_stop_generation" gorm:"default:0"`

	// Cascading: when a Task is deleted, all its OutputLogs are deleted
	OutputLogs []OutputLog `json:"-" gorm:"constraint:OnDelete:CASCADE;"`
}

func ParseRotationRemotes(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	var parsed []string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		parsed = strings.Split(raw, ",")
	}

	seen := make(map[string]bool, len(parsed))
	remotes := make([]string, 0, len(parsed))
	for _, remote := range parsed {
		remote = strings.TrimSpace(remote)
		if remote == "" || seen[remote] {
			continue
		}
		seen[remote] = true
		remotes = append(remotes, remote)
	}
	return remotes
}

func EncodeRotationRemotes(remotes []string) string {
	cleaned := make([]string, 0, len(remotes))
	seen := make(map[string]bool, len(remotes))
	for _, remote := range remotes {
		remote = strings.TrimSpace(remote)
		if remote == "" || seen[remote] {
			continue
		}
		seen[remote] = true
		cleaned = append(cleaned, remote)
	}
	data, _ := json.Marshal(cleaned)
	return string(data)
}

// ParseRotationQuotaKeys decodes the remote-to-quota-key mapping without
// coercing malformed input into a usable configuration.
func ParseRotationQuotaKeys(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}, nil
	}

	var parsed map[string]string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("rotation quota keys must be a JSON object: %w", err)
	}
	if parsed == nil {
		return nil, fmt.Errorf("rotation quota keys must be a JSON object")
	}

	cleaned := make(map[string]string, len(parsed))
	for remote, quotaKey := range parsed {
		remote = strings.TrimSpace(remote)
		quotaKey = strings.TrimSpace(quotaKey)
		if remote == "" {
			return nil, fmt.Errorf("rotation quota keys contains a blank remote")
		}
		if quotaKey == "" {
			return nil, fmt.Errorf("rotation quota key for remote %q is blank", remote)
		}
		if _, exists := cleaned[remote]; exists {
			return nil, fmt.Errorf("rotation quota keys contains duplicate remote %q after trimming", remote)
		}
		cleaned[remote] = quotaKey
	}
	return cleaned, nil
}

func EncodeRotationQuotaKeys(keys map[string]string) string {
	if len(keys) == 0 {
		return "{}"
	}

	cleaned := make(map[string]string, len(keys))
	for remote, quotaKey := range keys {
		cleaned[strings.TrimSpace(remote)] = strings.TrimSpace(quotaKey)
	}
	data, _ := json.Marshal(cleaned)
	return string(data)
}

func CanonicalDestinationPath(destination string) string {
	destination = strings.TrimSpace(strings.ReplaceAll(destination, "\\", "/"))
	if destination == "" {
		return "/"
	}
	destination = path.Clean(destination)
	if destination == "." {
		return "/"
	}
	if !strings.HasPrefix(destination, "/") {
		destination = "/" + destination
	}
	return path.Clean(destination)
}

func DestinationScope(resolvedConfigPath, destination string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(resolvedConfigPath))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(CanonicalDestinationPath(destination)))
	_, _ = hash.Write([]byte{0})
	return hex.EncodeToString(hash.Sum(nil))
}

// DefaultRotationQuotaKey is the canonical identity for an implicit proactive
// quota account. Keep this in models so dispatch and read APIs cannot drift.
func DefaultRotationQuotaKey(configIdentity, remote string) string {
	sum := sha256.Sum256([]byte(configIdentity + "\x00" + remote))
	return "default-" + hex.EncodeToString(sum[:])
}

// QuotaAccount is the durable identity and budget for one provider quota key.
type QuotaAccount struct {
	ID                   uint       `json:"id" gorm:"primaryKey"`
	QuotaKey             string     `json:"quota_key" gorm:"not null;uniqueIndex;check:quota_key_nonempty,quota_key <> ''"`
	RemoteName           string     `json:"remote_name"`
	ConfigIdentity       string     `json:"config_identity"`
	BudgetBytes          int64      `json:"budget_bytes" gorm:"not null;default:751619276800;check:quota_account_budget_nonnegative,budget_bytes >= 0"`
	WindowSeconds        int        `json:"window_seconds" gorm:"default:86400"`
	ProviderBlockedUntil *time.Time `json:"provider_blocked_until"`
	Enabled              bool       `json:"enabled" gorm:"not null;default:true"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

type RotationQuotaOversize struct {
	ID           uint      `json:"id" gorm:"primaryKey"`
	TaskID       uint      `json:"task_id" gorm:"uniqueIndex:uq_rotation_quota_oversize_task_path"`
	RelativePath string    `json:"relative_path" gorm:"uniqueIndex:uq_rotation_quota_oversize_task_path"`
	SnapshotKey  string    `json:"snapshot_key"`
	SizeBytes    int64     `json:"size_bytes"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// RotationQuotaBatch is an immutable attempt. A retry creates a new batch;
// existing attempts must not be mutated.
type RotationQuotaBatch struct {
	ID                         uint       `json:"id" gorm:"primaryKey"`
	TaskID                     uint       `json:"task_id" gorm:"index:idx_rotation_quota_batches_task_state;uniqueIndex:uq_rotation_quota_batches_request_identity"`
	QuotaAccountID             uint       `json:"quota_account_id" gorm:"index:idx_rotation_quota_batches_account_state_lease"`
	DestinationScope           string     `json:"destination_scope" gorm:"index:idx_rotation_quota_batches_destination_state"`
	SourceRoot                 string     `json:"source_root"`
	SourceRootDevice           int64      `json:"source_root_device" gorm:"not null;default:0"`
	SourceRootInode            int64      `json:"source_root_inode" gorm:"not null;default:0"`
	DestinationRemote          string     `json:"destination_remote" gorm:"uniqueIndex:uq_rotation_quota_batches_request_identity"`
	TransferMode               string     `json:"transfer_mode" gorm:"default:''"`
	DestinationScopeVersion    int        `json:"destination_scope_version" gorm:"default:0"`
	RcloneConfigPath           string     `json:"rclone_config_path" gorm:"not null;default:''"`
	RequestKey                 string     `json:"request_key" gorm:"not null;uniqueIndex:uq_rotation_quota_batches_request_identity"`
	RequestFingerprint         string     `json:"request_fingerprint" gorm:"not null;default:''"`
	DestinationPath            string     `json:"destination_path"`
	ManifestPath               string     `json:"manifest_path"`
	ManifestHash               string     `json:"manifest_hash"`
	State                      string     `json:"state" gorm:"not null;check:rotation_quota_batch_state_valid,state IN ('planned','reserved','running','unknown','reconciling','succeeded','failed','canceled','expired');index:idx_rotation_quota_batches_task_state;index:idx_rotation_quota_batches_account_state_lease;index:idx_rotation_quota_batches_destination_state"`
	OwnerToken                 string     `json:"owner_token" gorm:"uniqueIndex"`
	LeaseToken                 string     `json:"lease_token"`
	ProcessStartToken          string     `json:"process_start_token"`
	ProcessID                  int        `json:"process_id"`
	StartedAt                  *time.Time `json:"started_at"`
	LeaseUntil                 *time.Time `json:"lease_until" gorm:"index:idx_rotation_quota_batches_account_state_lease"`
	FinishedAt                 *time.Time `json:"finished_at"`
	ExitCode                   *int       `json:"exit_code"`
	LimitMarker                string     `json:"limit_marker"`
	MarkerDetectedAt           *time.Time `json:"marker_detected_at"`
	LastError                  string     `json:"last_error"`
	ReservedBytes              int64      `json:"reserved_bytes" gorm:"check:rotation_quota_batch_reserved_nonnegative,reserved_bytes >= 0"`
	CompletionEvidence         string     `json:"completion_evidence" gorm:"default:''"`
	CompletionEvidenceVersion  int        `json:"completion_evidence_version" gorm:"default:0"`
	MoveHandoffContractVersion int        `json:"move_handoff_contract_version" gorm:"default:0"`
	MoveQuarantinePath         string     `json:"move_quarantine_path"`
	MoveQuarantineDevice       int64      `json:"move_quarantine_device"`
	MoveQuarantineInode        int64      `json:"move_quarantine_inode"`
	MoveHandoffStartedAt       *time.Time `json:"move_handoff_started_at"`
	CreatedAt                  time.Time  `json:"created_at"`
	UpdatedAt                  time.Time  `json:"updated_at"`
}

// RotationQuotaBatchFile is an immutable file snapshot belonging to one attempt.
type RotationQuotaBatchFile struct {
	ID                      uint       `json:"id" gorm:"primaryKey"`
	BatchID                 uint       `json:"batch_id" gorm:"uniqueIndex:uq_rotation_quota_batch_files_batch_path;uniqueIndex:uq_rotation_quota_batch_files_batch_snapshot;index:idx_rotation_quota_batch_files_snapshot_state"`
	RelativePath            string     `json:"relative_path" gorm:"uniqueIndex:uq_rotation_quota_batch_files_batch_path"`
	SnapshotKey             string     `json:"snapshot_key" gorm:"uniqueIndex:uq_rotation_quota_batch_files_batch_snapshot;index:idx_rotation_quota_batch_files_snapshot_state"`
	SizeBytes               int64      `json:"size_bytes" gorm:"check:rotation_quota_batch_file_size_nonnegative,size_bytes >= 0"`
	MtimeNS                 int64      `json:"mtime_ns"`
	Device                  int64      `json:"device"`
	Inode                   int64      `json:"inode"`
	ContentHash             string     `json:"content_hash"`
	State                   string     `json:"state" gorm:"check:rotation_quota_batch_file_state_valid,state IN ('','held','active','verified','unknown','failed','committed');index:idx_rotation_quota_batch_files_snapshot_state"`
	RemotePath              string     `json:"remote_path"`
	RemoteSize              *int64     `json:"remote_size"`
	RemoteHash              string     `json:"remote_hash"`
	VerifiedAt              *time.Time `json:"verified_at"`
	LastError               string     `json:"last_error"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
	MoveHandoffState        string     `json:"move_handoff_state" gorm:"default:''"`
	MoveHandoffDevice       int64      `json:"move_handoff_device"`
	MoveHandoffInode        int64      `json:"move_handoff_inode"`
	MoveHandoffSize         int64      `json:"move_handoff_size"`
	MoveHandoffMtimeNS      int64      `json:"move_handoff_mtime_ns"`
	MoveResolutionState     string     `json:"-"`
	MoveResolutionToken     string     `json:"-"`
	MoveResolutionStartedAt *time.Time `json:"-"`
}

// QuotaReservation is an immutable reservation for one batch file. A retry
// creates a new reservation; existing reservations must not be mutated.
type QuotaReservation struct {
	ID             uint       `json:"id" gorm:"primaryKey"`
	QuotaAccountID uint       `json:"quota_account_id" gorm:"index:idx_quota_reservations_account_state_expiry"`
	BatchID        uint       `json:"batch_id" gorm:"index:idx_quota_reservations_batch_state"`
	BatchFileID    uint       `json:"batch_file_id" gorm:"not null;uniqueIndex:uq_quota_reservations_batch_file;index:idx_quota_reservations_batch_file"`
	Bytes          int64      `json:"bytes" gorm:"check:quota_reservation_bytes_nonnegative,bytes >= 0"`
	State          string     `json:"state" gorm:"not null;check:quota_reservation_state_valid,state IN ('held','active','committed','unknown','released','expired');index:idx_quota_reservations_account_state_expiry;index:idx_quota_reservations_batch_state"`
	IdempotencyKey string     `json:"idempotency_key" gorm:"not null;uniqueIndex"`
	ReservedAt     *time.Time `json:"reserved_at"`
	StartedAt      *time.Time `json:"started_at"`
	ReleasedAt     *time.Time `json:"released_at"`
	ExpiresAt      *time.Time `json:"expires_at" gorm:"index:idx_quota_reservations_account_state_expiry"`
	LastError      string     `json:"last_error"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type OpenlistConfig struct {
	ID        uint           `json:"id" gorm:"primaryKey"`
	Name      string         `json:"name" gorm:"not null"`
	URL       string         `json:"url" gorm:"not null"`
	Token     string         `json:"token"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

type MountConfig struct {
	ID            uint           `json:"id" gorm:"primaryKey"`
	Name          string         `json:"name" gorm:"not null"`
	RemoteName    string         `json:"remote_name" gorm:"not null"`
	RemotePath    string         `json:"remote_path" gorm:"default:'/'"`
	MountPath     string         `json:"mount_path" gorm:"not null;uniqueIndex"`
	RcloneConfig  string         `json:"rclone_config"`
	Enabled       bool           `json:"enabled"`
	AllowOther    bool           `json:"allow_other" gorm:"default:true"`
	ReadOnly      bool           `json:"read_only" gorm:"default:false"`
	VFSCacheMode  string         `json:"vfs_cache_mode" gorm:"default:'writes'"`
	DirCacheTime  string         `json:"dir_cache_time" gorm:"default:'5m'"`
	PollInterval  string         `json:"poll_interval" gorm:"default:'1m'"`
	UID           int            `json:"uid" gorm:"default:0"`
	GID           int            `json:"gid" gorm:"default:0"`
	ExtraArgs     string         `json:"extra_args" gorm:"type:text"`
	Status        string         `json:"status" gorm:"default:'stopped'"`
	LastError     string         `json:"last_error"`
	LastMountedAt *time.Time     `json:"last_mounted_at"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	DeletedAt     gorm.DeletedAt `json:"-" gorm:"index"`
}

type TaskLog struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	TaskID    uint      `json:"task_id"`
	TaskName  string    `json:"task_name"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

type SystemSetting struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	Key       string    `json:"key" gorm:"uniqueIndex;not null"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

type User struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	Username  string    `json:"username" gorm:"uniqueIndex;not null"`
	Password  string    `json:"-" gorm:"not null"`
	IsAdmin   bool      `json:"is_admin" gorm:"default:false"`
	CreatedAt time.Time `json:"created_at"`
}

// OutputLog is a persistent structured transfer log stored in SQLite.
// Each record represents one file transfer operation.
// Records are automatically deleted when the parent Task is deleted (CASCADE).
type OutputLog struct {
	ID          uint           `json:"id" gorm:"primaryKey"`
	TaskID      uint           `json:"task_id" gorm:"index;not null"`
	Src         string         `json:"src" gorm:"type:text"`
	SrcStorage  string         `json:"src_storage"`
	Dest        string         `json:"dest" gorm:"type:text"`
	DestStorage string         `json:"dest_storage"`
	Mode        string         `json:"mode"`
	FileName    string         `json:"file_name"`
	FileSize    int64          `json:"file_size"`
	FileExt     string         `json:"file_ext"`
	Status      bool           `json:"status" gorm:"default:true"`
	Progress    int            `json:"progress" gorm:"default:0"`
	Errmsg      string         `json:"errmsg" gorm:"type:text"`
	Date        time.Time      `json:"date"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `json:"-" gorm:"index"`

	// OpenList refresh status
	OpenlistStatus string `json:"openlist_status" gorm:"default:''"`
	OpenlistMsg    string `json:"openlist_msg" gorm:"default:''"`
}

// OutputLogResponse is the unified API response wrapper for the frontend.
type OutputLogResponse struct {
	Success bool          `json:"success"`
	Message *string       `json:"message"`
	Data    OutputLogData `json:"data"`
}

// OutputLogData contains the paginated list and total count.
type OutputLogData struct {
	List  []OutputLog `json:"list"`
	Total int64       `json:"total"`
}
