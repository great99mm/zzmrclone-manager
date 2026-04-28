package models

import (
	"time"

	"gorm.io/gorm"
)

type Task struct {
	ID               uint           `json:"id" gorm:"primaryKey"`
	Name             string         `json:"name" gorm:"not null"`
	SourceDir        string         `json:"source_dir" gorm:"not null"`
	RemoteName       string         `json:"remote_name" gorm:"not null"`
	RemoteDir        string         `json:"remote_dir" gorm:"not null"`
	Transfers        int            `json:"transfers" gorm:"default:16"`
	Checkers         int            `json:"checkers" gorm:"default:32"`
	BindIP           string         `json:"bind_ip"`
	RcloneConfig     string         `json:"rclone_config"`
	Enabled          bool           `json:"enabled" gorm:"default:true"`
	AutoDedupe       bool           `json:"auto_dedupe" gorm:"default:true"`
	MinAge           string         `json:"min_age" gorm:"default:10s"`
	DriveChunkSize   string         `json:"drive_chunk_size" gorm:"default:256M"`
	BufferSize       string         `json:"buffer_size" gorm:"default:512M"`
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

	// Cascading: when a Task is deleted, all its OutputLogs are deleted
	OutputLogs []OutputLog `json:"-" gorm:"constraint:OnDelete:CASCADE;"`
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
	ID           uint           `json:"id" gorm:"primaryKey"`
	TaskID       uint           `json:"task_id" gorm:"index;not null"`
	Src          string         `json:"src" gorm:"type:text"`
	SrcStorage   string         `json:"src_storage"`
	Dest         string         `json:"dest" gorm:"type:text"`
	DestStorage  string         `json:"dest_storage"`
	Mode         string         `json:"mode"`
	FileName     string         `json:"file_name"`
	FileSize     int64          `json:"file_size"`
	FileExt      string         `json:"file_ext"`
	Status       bool           `json:"status" gorm:"default:true"`
	Errmsg       string         `json:"errmsg" gorm:"type:text"`
	Date         time.Time      `json:"date"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	DeletedAt    gorm.DeletedAt `json:"-" gorm:"index"`
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
