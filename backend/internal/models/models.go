package models

import (
	"time"

	"gorm.io/gorm"
)

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
	AutoDedupe   bool   `json:"auto_dedupe" gorm:"default:true"`
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

	// Cascading: when a Task is deleted, all its OutputLogs are deleted
	OutputLogs []OutputLog `json:"-" gorm:"constraint:OnDelete:CASCADE;"`
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

type WebhookJob struct {
	ID          string     `json:"id" gorm:"primaryKey"`
	JobType     string     `json:"job_type" gorm:"default:'one_time'"`
	RemoteName  string     `json:"remote" gorm:"column:remote_name"`
	Tag         string     `json:"tag"`
	RemotePath  string     `json:"remote_path" gorm:"not null"`
	LocalPath   string     `json:"local_path"`
	CallbackURL string     `json:"callback_url" gorm:"not null"`
	CurlURL     string     `json:"curl_url"`
	CurlHeaders string     `json:"curl_headers" gorm:"type:text"`
	Status      string     `json:"status" gorm:"index;not null"`
	Error       string     `json:"error" gorm:"type:text"`
	RcloneLog   string     `json:"rclone_log" gorm:"type:text"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	FinishedAt  *time.Time `json:"finished_at"`
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
