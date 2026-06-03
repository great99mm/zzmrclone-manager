package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"rclone-manager/internal/config"
	"rclone-manager/internal/models"
	webhooksvc "rclone-manager/internal/webhook"
)

type webhookConfigRequest struct {
	LocalBaseDir          string        `json:"local_base_dir"`
	RcloneRemote          string        `json:"rclone_remote"`
	Transfers             int           `json:"transfers"`
	Checkers              int           `json:"checkers"`
	Retries               int           `json:"retries"`
	LowLevelRetries       int           `json:"low_level_retries"`
	BWLimit               string        `json:"bwlimit"`
	JobTimeout            string        `json:"job_timeout"`
	HTTPTimeout           string        `json:"http_timeout"`
	MaxRcloneLogBytes     int           `json:"max_rclone_log_bytes"`
	TagDirs               []tagDir      `json:"tag_dirs"`
	SmartStrmWebhookURL   string        `json:"smartstrm_webhook_url"`
	SmartStrmPathMappings []pathMapping `json:"smartstrm_path_mappings"`
	AllowedCallbackHosts  []string      `json:"allowed_callback_hosts"`
	AllowedSmartStrmHosts []string      `json:"allowed_smartstrm_hosts"`
}

type tagDir struct {
	Tag  string `json:"tag"`
	Dir  string `json:"dir"`
	Task string `json:"task"`
}

type pathMapping struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func webhookAuthMiddleware(c *gin.Context) {
	if webhookJobs == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "webhook service is not ready"})
		return
	}
	token := bearerToken(c.GetHeader("Authorization"))
	if token == "" {
		token = strings.TrimSpace(c.GetHeader("X-API-Token"))
	}
	if token == "" {
		token = strings.TrimSpace(c.GetHeader("X-Webhook-Token"))
	}
	if token == "" {
		token = strings.TrimSpace(c.Query("token"))
	}
	if !webhookJobs.VerifyToken(token) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	c.Next()
}

func createWebhookJob(c *gin.Context) {
	if isExternalWebhookRequest(c) {
		webhookAuthMiddleware(c)
		if c.IsAborted() {
			return
		}
	}

	var req webhooksvc.CreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	job, err := webhookJobs.CreateJob(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"job_id": job.ID, "job_type": job.JobType, "status": job.Status})
}

func isExternalWebhookRequest(c *gin.Context) bool {
	return c.FullPath() == "/webhook"
}

func listWebhookJobs(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	jobs, err := webhookJobs.ListJobs(c.Request.Context(), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"jobs": jobs})
}

func getWebhookJob(c *gin.Context) {
	job, err := webhookJobs.GetJob(c.Request.Context(), c.Param("id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, job)
}

func retryWebhookJob(c *gin.Context) {
	job, err := webhookJobs.RetryJob(c.Request.Context(), c.Param("id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
			return
		}
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"job_id": job.ID, "job_type": job.JobType, "status": job.Status})
}

func deleteWebhookJob(c *gin.Context) {
	if err := webhookJobs.DeleteJob(c.Request.Context(), c.Param("id")); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
			return
		}
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "webhook job deleted"})
}

func getWebhookConfig(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"local_base_dir":          cfgGlobal.WebhookLocalBaseDir,
		"rclone_path":             cfgGlobal.WebhookRclonePath,
		"rclone_config":           cfgGlobal.RcloneConfig,
		"rclone_remote":           cfgGlobal.WebhookRcloneRemote,
		"transfers":               cfgGlobal.WebhookTransfers,
		"checkers":                cfgGlobal.WebhookCheckers,
		"retries":                 cfgGlobal.WebhookRetries,
		"low_level_retries":       cfgGlobal.WebhookLowLevelRetries,
		"bwlimit":                 cfgGlobal.WebhookBWLimit,
		"workers":                 cfgGlobal.WebhookWorkers,
		"queue_size":              cfgGlobal.WebhookQueueSize,
		"job_timeout":             cfgGlobal.WebhookJobTimeout,
		"http_timeout":            cfgGlobal.WebhookHTTPTimeout,
		"max_rclone_log_bytes":    cfgGlobal.WebhookMaxRcloneLogSize,
		"tag_dirs":                tagDirsToList(cfgGlobal.WebhookTagDirs, cfgGlobal.WebhookTagTasks),
		"smartstrm_webhook_url":   cfgGlobal.SmartStrmWebhookURL,
		"smartstrm_path_mappings": pathMappingsToList(cfgGlobal.SmartStrmPathMappings),
		"allowed_callback_hosts":  cfgGlobal.AllowedCallbackHosts,
		"allowed_smartstrm_hosts": cfgGlobal.AllowedSmartStrmHosts,
		"token_required":          webhookJobs.AuthEnabled(),
		"api_token_enabled":       strings.TrimSpace(cfgGlobal.APIToken) != "",
		"token_source":            "RCLONE_MANAGER_API_TOKEN",
	})
}

func updateWebhookConfig(c *gin.Context) {
	var req webhookConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := applyWebhookConfigRequest(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := saveWebhookConfigRequest(&req); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := webhookJobs.ReloadConfig(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "webhook config updated"})
}

func applyWebhookConfigRequest(req *webhookConfigRequest) error {
	localBaseDir := strings.TrimSpace(req.LocalBaseDir)
	if localBaseDir == "" {
		localBaseDir = cfgGlobal.DataDir + "/downloads"
	}
	if !filepath.IsAbs(localBaseDir) {
		return errors.New("local_base_dir must be an absolute path")
	}
	if err := os.MkdirAll(localBaseDir, 0o755); err != nil {
		return err
	}
	if _, err := time.ParseDuration(defaultString(req.JobTimeout, "0s")); err != nil {
		return errors.New("job_timeout must be a valid Go duration, for example 0s or 2h")
	}
	if _, err := time.ParseDuration(defaultString(req.HTTPTimeout, "30s")); err != nil {
		return errors.New("http_timeout must be a valid Go duration, for example 30s")
	}

	cfgGlobal.WebhookLocalBaseDir = localBaseDir
	cfgGlobal.WebhookRcloneRemote = strings.TrimSuffix(strings.TrimSpace(req.RcloneRemote), ":")
	cfgGlobal.WebhookTransfers = clampInt(req.Transfers, 1, 64, 4)
	cfgGlobal.WebhookCheckers = clampInt(req.Checkers, 1, 128, 8)
	cfgGlobal.WebhookRetries = clampInt(req.Retries, 0, 20, 3)
	cfgGlobal.WebhookLowLevelRetries = clampInt(req.LowLevelRetries, 0, 50, 10)
	cfgGlobal.WebhookBWLimit = strings.TrimSpace(req.BWLimit)
	cfgGlobal.WebhookJobTimeout = defaultString(req.JobTimeout, "0s")
	cfgGlobal.WebhookHTTPTimeout = defaultString(req.HTTPTimeout, "30s")
	cfgGlobal.WebhookMaxRcloneLogSize = clampInt(req.MaxRcloneLogBytes, 1024, 10485760, 1048576)
	tagDirs, tagTasks, err := cleanTagDirs(req.TagDirs)
	if err != nil {
		return err
	}
	cfgGlobal.WebhookTagDirs = tagDirs
	cfgGlobal.WebhookTagTasks = tagTasks
	allowedSmartStrmHosts := cleanHostList(req.AllowedSmartStrmHosts)
	smartStrmWebhookURL := strings.TrimSpace(req.SmartStrmWebhookURL)
	if smartStrmWebhookURL != "" {
		for tag := range tagDirs {
			if strings.TrimSpace(tagTasks[tag]) == "" {
				return fmt.Errorf("SmartStrm task is required for tag %q when smartstrm_webhook_url is configured", tag)
			}
		}
		if _, err := webhooksvc.ValidateOutboundURLForConfig(smartStrmWebhookURL, allowedSmartStrmHosts); err != nil {
			return fmt.Errorf("invalid smartstrm_webhook_url: %w", err)
		}
	}
	pathMappings, err := cleanPathMappings(req.SmartStrmPathMappings)
	if err != nil {
		return err
	}
	cfgGlobal.SmartStrmWebhookURL = smartStrmWebhookURL
	cfgGlobal.SmartStrmPathMappings = pathMappings
	cfgGlobal.AllowedCallbackHosts = cleanHostList(req.AllowedCallbackHosts)
	cfgGlobal.AllowedSmartStrmHosts = allowedSmartStrmHosts
	return nil
}

func saveWebhookConfigRequest(req *webhookConfigRequest) error {
	tagDirsJSON, err := json.Marshal(cfgGlobal.WebhookTagDirs)
	if err != nil {
		return err
	}
	tagTasksJSON, err := json.Marshal(cfgGlobal.WebhookTagTasks)
	if err != nil {
		return err
	}
	pathMappingsJSON, err := json.Marshal(cfgGlobal.SmartStrmPathMappings)
	if err != nil {
		return err
	}
	settings := map[string]string{
		"webhook_local_base_dir":         cfgGlobal.WebhookLocalBaseDir,
		"webhook_rclone_remote":          cfgGlobal.WebhookRcloneRemote,
		"webhook_transfers":              strconv.Itoa(cfgGlobal.WebhookTransfers),
		"webhook_checkers":               strconv.Itoa(cfgGlobal.WebhookCheckers),
		"webhook_retries":                strconv.Itoa(cfgGlobal.WebhookRetries),
		"webhook_low_level_retries":      strconv.Itoa(cfgGlobal.WebhookLowLevelRetries),
		"webhook_bwlimit":                cfgGlobal.WebhookBWLimit,
		"webhook_job_timeout":            cfgGlobal.WebhookJobTimeout,
		"webhook_http_timeout":           cfgGlobal.WebhookHTTPTimeout,
		"webhook_max_rclone_log_bytes":   strconv.Itoa(cfgGlobal.WebhookMaxRcloneLogSize),
		"webhook_tag_dirs":               string(tagDirsJSON),
		"webhook_tag_tasks":              string(tagTasksJSON),
		"smartstrm_webhook_url":          cfgGlobal.SmartStrmWebhookURL,
		"smartstrm_path_mappings":        string(pathMappingsJSON),
		"webhook_allowed_callback_hosts": strings.Join(cfgGlobal.AllowedCallbackHosts, ","),
		"smartstrm_allowed_hosts":        strings.Join(cfgGlobal.AllowedSmartStrmHosts, ","),
	}
	for key, value := range settings {
		var setting models.SystemSetting
		if err := db.Where("`key` = ?", key).FirstOrCreate(&setting, models.SystemSetting{Key: key}).Error; err != nil {
			return err
		}
		setting.Value = value
		if err := db.Save(&setting).Error; err != nil {
			return err
		}
	}
	return nil
}

func cleanTagDirs(values []tagDir) (map[string]string, map[string]string, error) {
	items := make(map[string]string, len(values))
	tasks := make(map[string]string, len(values))
	for _, item := range values {
		tag := strings.TrimSpace(item.Tag)
		dir := strings.TrimSpace(item.Dir)
		task := strings.TrimSpace(item.Task)
		if tag == "" && dir == "" && task == "" {
			continue
		}
		if tag == "" || dir == "" {
			return nil, nil, errors.New("tag and dir are required for each tag mapping")
		}
		if strings.ContainsAny(tag, "\x00\r\n") {
			return nil, nil, fmt.Errorf("tag %q contains invalid characters", tag)
		}
		if strings.ContainsAny(dir, "\x00\r\n") || strings.ContainsAny(task, "\x00\r\n") {
			return nil, nil, fmt.Errorf("tag %q contains invalid mapping value", tag)
		}
		if !filepath.IsAbs(dir) {
			return nil, nil, fmt.Errorf("tag %q dir must be absolute", tag)
		}
		dir = filepath.Clean(dir)
		if err := rejectExistingSymlinkComponents(dir); err != nil {
			return nil, nil, err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, err
		}
		if err := rejectExistingSymlinkComponents(dir); err != nil {
			return nil, nil, err
		}
		items[tag] = dir
		tasks[tag] = task
	}
	return items, tasks, nil
}

func rejectExistingSymlinkComponents(absPath string) error {
	if !filepath.IsAbs(absPath) {
		return errors.New("path must be absolute")
	}
	current := string(os.PathSeparator)
	for _, part := range strings.Split(strings.TrimPrefix(absPath, string(os.PathSeparator)), string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("local path component %q is a symlink", current)
		}
	}
	return nil
}

func tagDirsToList(values map[string]string, tasks map[string]string) []tagDir {
	items := make([]tagDir, 0, len(values))
	for tag, dir := range values {
		items = append(items, tagDir{Tag: tag, Dir: dir, Task: tasks[tag]})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Tag < items[j].Tag
	})
	return items
}

func cleanPathMappings(values []pathMapping) ([]config.PathMapping, error) {
	items := make([]config.PathMapping, 0, len(values))
	for _, item := range values {
		from := strings.TrimSpace(item.From)
		to := strings.TrimSpace(item.To)
		if from == "" && to == "" {
			continue
		}
		if from == "" || to == "" {
			return nil, errors.New("from and to are required for each SmartStrm path mapping")
		}
		if strings.ContainsAny(from, "\x00\r\n") || strings.ContainsAny(to, "\x00\r\n") {
			return nil, errors.New("SmartStrm path mapping contains invalid characters")
		}
		if !filepath.IsAbs(from) {
			return nil, fmt.Errorf("SmartStrm mapping source %q must be absolute", from)
		}
		items = append(items, config.PathMapping{From: filepath.Clean(from), To: strings.TrimRight(to, "/")})
	}
	sort.Slice(items, func(i, j int) bool {
		return len(items[i].From) > len(items[j].From)
	})
	return items, nil
}

func pathMappingsToList(values []config.PathMapping) []pathMapping {
	items := make([]pathMapping, 0, len(values))
	for _, item := range values {
		items = append(items, pathMapping{From: item.From, To: item.To})
	}
	return items
}

func cleanHostList(values []string) []string {
	items := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		items = append(items, value)
	}
	return items
}

func defaultString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func clampInt(value, minValue, maxValue, fallback int) int {
	if value == 0 {
		value = fallback
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func bearerToken(value string) string {
	if value == "" {
		return ""
	}
	prefix := "Bearer "
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(value[len(prefix):])
}
