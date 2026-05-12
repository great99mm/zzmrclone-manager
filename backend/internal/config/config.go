package config

import (
	"os"
)

type Config struct {
	DataDir      string
	LogDir       string
	Port         string
	RcloneConfig string
	APIToken     string
	MountRoot    string
}

func Load() *Config {
	return &Config{
		DataDir:      getEnv("RCLONE_MANAGER_DATA_DIR", "/app/data"),
		LogDir:       getEnv("RCLONE_MANAGER_LOG_DIR", "/app/logs"),
		Port:         getEnv("RCLONE_MANAGER_PORT", "7070"),
		RcloneConfig: getEnv("RCLONE_CONFIG", "/root/.config/rclone/rclone.conf"),
		APIToken:     getEnv("RCLONE_MANAGER_API_TOKEN", ""),
		MountRoot:    getEnv("RCLONE_MANAGER_MOUNT_ROOT", ""),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
