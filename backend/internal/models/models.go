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
