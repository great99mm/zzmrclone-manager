package config

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	DataDir                 string
	LogDir                  string
	Port                    string
	RcloneConfig            string
	APIToken                string
	MountRoot               string
	WebhookLocalBaseDir     string
	WebhookRclonePath       string
	WebhookRcloneRemote     string
	WebhookTransfers        int
	WebhookCheckers         int
	WebhookRetries          int
	WebhookLowLevelRetries  int
	WebhookBWLimit          string
	WebhookWorkers          int
	WebhookQueueSize        int
	WebhookJobTimeout       string
	WebhookHTTPTimeout      string
	WebhookMaxRcloneLogSize int
	WebhookTagDirs          map[string]string
	WebhookTagTasks         map[string]string
	SmartStrmWebhookURL     string
	SmartStrmPathMappings   []PathMapping
	AllowedCallbackHosts    []string
	AllowedSmartStrmHosts   []string
}

type PathMapping struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func Load() *Config {
	dataDir := getEnv("RCLONE_MANAGER_DATA_DIR", "/app/data")
	return &Config{
		DataDir:                 dataDir,
		LogDir:                  getEnv("RCLONE_MANAGER_LOG_DIR", "/app/logs"),
		Port:                    getEnv("RCLONE_MANAGER_PORT", "6050"),
		RcloneConfig:            getEnv("RCLONE_CONFIG", "/root/.config/rclone/rclone.conf"),
		APIToken:                getEnv("RCLONE_MANAGER_API_TOKEN", ""),
		MountRoot:               getEnv("RCLONE_MANAGER_MOUNT_ROOT", ""),
		WebhookLocalBaseDir:     getEnv("RCLONE_MANAGER_WEBHOOK_LOCAL_BASE_DIR", dataDir+"/downloads"),
		WebhookRclonePath:       getEnv("RCLONE_MANAGER_WEBHOOK_RCLONE_PATH", "rclone"),
		WebhookRcloneRemote:     getEnv("RCLONE_MANAGER_WEBHOOK_RCLONE_REMOTE", ""),
		WebhookTransfers:        getEnvInt("RCLONE_MANAGER_WEBHOOK_TRANSFERS", 4),
		WebhookCheckers:         getEnvInt("RCLONE_MANAGER_WEBHOOK_CHECKERS", 8),
		WebhookRetries:          getEnvInt("RCLONE_MANAGER_WEBHOOK_RETRIES", 3),
		WebhookLowLevelRetries:  getEnvInt("RCLONE_MANAGER_WEBHOOK_LOW_LEVEL_RETRIES", 10),
		WebhookBWLimit:          getEnv("RCLONE_MANAGER_WEBHOOK_BWLIMIT", ""),
		WebhookWorkers:          getEnvInt("RCLONE_MANAGER_WEBHOOK_WORKERS", 2),
		WebhookQueueSize:        getEnvInt("RCLONE_MANAGER_WEBHOOK_QUEUE_SIZE", 100),
		WebhookJobTimeout:       getEnv("RCLONE_MANAGER_WEBHOOK_JOB_TIMEOUT", "0s"),
		WebhookHTTPTimeout:      getEnv("RCLONE_MANAGER_WEBHOOK_HTTP_TIMEOUT", "30s"),
		WebhookMaxRcloneLogSize: getEnvInt("RCLONE_MANAGER_WEBHOOK_MAX_RCLONE_LOG_BYTES", 1048576),
		WebhookTagDirs:          map[string]string{},
		WebhookTagTasks:         map[string]string{},
		SmartStrmWebhookURL:     getEnv("RCLONE_MANAGER_SMARTSTRM_WEBHOOK_URL", ""),
		AllowedCallbackHosts:    getEnvList("RCLONE_MANAGER_WEBHOOK_ALLOWED_CALLBACK_HOSTS"),
		AllowedSmartStrmHosts:   getEnvList("RCLONE_MANAGER_SMARTSTRM_ALLOWED_HOSTS"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		var parsed int
		if _, err := fmt.Sscanf(value, "%d", &parsed); err == nil {
			return parsed
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	switch os.Getenv(key) {
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	case "0", "false", "FALSE", "no", "NO", "off", "OFF":
		return false
	default:
		return defaultValue
	}
}

func getEnvList(key string) []string {
	value := os.Getenv(key)
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			items = append(items, part)
		}
	}
	return items
}
