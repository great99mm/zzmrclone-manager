package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"rclone-manager/internal/config"
	"rclone-manager/internal/logger"
	"rclone-manager/internal/models"
	mountsvc "rclone-manager/internal/mounts"
	"rclone-manager/internal/rclone"
	"rclone-manager/internal/scheduler"
	"rclone-manager/internal/watcher"
	webhooksvc "rclone-manager/internal/webhook"
	"rclone-manager/internal/websocket"
)

var (
	executor    *rclone.Executor
	sched       *scheduler.Scheduler
	watch       *watcher.Watcher
	hub         *websocket.Hub
	cfgGlobal   *config.Config
	mountMgr    *mountsvc.Manager
	webhookJobs *webhooksvc.Service
)

// Hard caps for memory-hungry rclone flags.  These act as guardrails:
// a user (or the old defaults) cannot request so much per-transfer RAM
// that the host OOMs.  The caps are still high enough for fast transfers.
const (
	maxTransfers      = 16     // old default was 16, keep same cap
	maxCheckers       = 32     // old default was 32, keep same cap
	maxBufferSize     = "512M" // hard ceiling; most users will run at 64M
	maxDriveChunkSize = "256M" // hard ceiling
	localBrowserRoot  = "/"
	localBrowserStart = "/"
)

func SetupRouter(cfg *config.Config) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	cfgGlobal = cfg

	// CORS
	router.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", "X-API-Token", "X-Webhook-Token"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	// Init database
	if err := InitDB(cfg.DataDir); err != nil {
		panic(err)
	}
	if err := loadSystemSettingsIntoConfig(cfg); err != nil {
		panic(err)
	}

	// Init WebSocket hub
	hub = websocket.NewHub()
	go hub.Run()

	// Init executor (pass db so rclone can persist structured logs)
	executor = rclone.NewExecutor(hub, db)

	// Init scheduler
	sched = scheduler.NewScheduler(executor)
	sched.Start()

	// Init watcher
	watch = watcher.NewWatcher(executor)

	// Init mount manager and auto-mount enabled mount configs
	mountMgr = mountsvc.NewManager(db, cfg.MountRoot, cfg.DataDir)
	go mountMgr.RestoreAndStartEnabled()

	var err error
	webhookJobs, err = webhooksvc.NewService(cfg, db)
	if err != nil {
		panic(err)
	}
	if err := webhookJobs.Start(context.Background()); err != nil {
		panic(err)
	}

	// Load existing tasks and start watchers/schedules
	var tasks []models.Task
	db.Where("enabled = ?", true).Find(&tasks)
	for _, task := range tasks {
		if task.WatchEnabled {
			watch.StartTaskWatch(&task, executor)
		}
		if task.ScheduleEnabled {
			sched.AddTask(&task)
		}
	}

	// Routes
	api := router.Group("/api")
	{
		// Auth
		api.POST("/login", handleLogin)
		api.POST("/register", handleRegister)
		api.POST("/change-password", handleChangePassword)

		// Token management (read/update)
		api.GET("/token", requireTokenQuery, getTokenInfo)
		api.POST("/token", requireTokenQuery, updateToken)

		// Tasks
		tasks := api.Group("/tasks")
		{
			tasks.GET("", listTasks)
			tasks.POST("", createTask)
			tasks.GET("/quick", listQuickTasks)
			tasks.POST("/quick", createQuickTask)
			tasks.GET("/:id", getTask)
			tasks.PUT("/:id", updateTask)
			tasks.DELETE("/:id", deleteTask)
			tasks.POST("/:id/start", startTask)
			tasks.POST("/:id/pause", pauseTask)
			tasks.POST("/:id/cancel", cancelTask)
			tasks.POST("/:id/stop", stopTask)
			tasks.POST("/:id/dedupe", dedupeTask)
			tasks.GET("/:id/logs", getTaskLogs)
			tasks.GET("/:id/status", getTaskStatus)
		}

		// System
		api.GET("/system/stats", getSystemStats)
		api.GET("/system/rclone-stats", getRcloneStats)
		api.POST("/system/log-level", setLogLevel)
		api.GET("/system/logs", getSystemLogs)
		api.POST("/system/logs/clean", cleanLogs)
		api.GET("/mounts/system", getMountSystemInfo)

		// Rclone config
		api.GET("/rclone/remotes", listRemotes)
		api.GET("/rclone/remotes/detail", listRemoteDetails)
		api.GET("/rclone/config", getRcloneConfig)
		api.GET("/rclone/ls", listRemoteDir)
		api.POST("/rclone/mkdir", createRemoteDir)

		// Mounts
		mounts := api.Group("/mounts")
		{
			mounts.GET("", listMounts)
			mounts.POST("", createMount)
			mounts.GET("/:id", getMount)
			mounts.PUT("/:id", updateMount)
			mounts.DELETE("/:id", deleteMount)
			mounts.POST("/:id/start", startMount)
			mounts.POST("/:id/stop", stopMount)
		}

		// File browser
		api.GET("/files/local", listLocalFiles)
		api.GET("/files/remote", listRemoteFiles)

		// OpenList configs
		api.GET("/openlist-configs", listOpenlistConfigs)
		api.POST("/openlist-configs", createOpenlistConfig)
		api.PUT("/openlist-configs/:id", updateOpenlistConfig)
		api.DELETE("/openlist-configs/:id", deleteOpenlistConfig)

		// Output logs (structured persistent format) - protected by token query
		api.GET("/output-logs", requireTokenQuery, getOutputLogs)
		api.DELETE("/output-logs/:id", requireTokenQuery, deleteOutputLog)
		api.DELETE("/output-logs/clean", requireTokenQuery, cleanOutputLogs)

		// Webhook-driven download jobs
		webhookJobsGroup := api.Group("/webhook-jobs")
		{
			webhookJobsGroup.GET("", listWebhookJobs)
			webhookJobsGroup.POST("", createWebhookJob)
			webhookJobsGroup.GET("/config", getWebhookConfig)
			webhookJobsGroup.PUT("/config", updateWebhookConfig)
			webhookJobsGroup.GET("/:id", getWebhookJob)
			webhookJobsGroup.POST("/:id/retry", retryWebhookJob)
		}
	}

	router.POST("/webhook", createWebhookJob)

	// WebSocket
	router.GET("/ws", hub.HandleWebSocket)

	return router
}

func loadSystemSettingsIntoConfig(cfg *config.Config) error {
	settings := make([]models.SystemSetting, 0)
	if err := db.Find(&settings).Error; err != nil {
		return err
	}
	for _, setting := range settings {
		applySystemSetting(cfg, setting.Key, setting.Value)
	}
	return nil
}

func applySystemSetting(cfg *config.Config, key, value string) {
	switch key {
	case "api_token":
		cfg.APIToken = value
	case "webhook_local_base_dir":
		cfg.WebhookLocalBaseDir = value
	case "webhook_rclone_remote":
		cfg.WebhookRcloneRemote = value
	case "webhook_transfers":
		if parsed, err := strconv.Atoi(value); err == nil {
			cfg.WebhookTransfers = parsed
		}
	case "webhook_checkers":
		if parsed, err := strconv.Atoi(value); err == nil {
			cfg.WebhookCheckers = parsed
		}
	case "webhook_retries":
		if parsed, err := strconv.Atoi(value); err == nil {
			cfg.WebhookRetries = parsed
		}
	case "webhook_low_level_retries":
		if parsed, err := strconv.Atoi(value); err == nil {
			cfg.WebhookLowLevelRetries = parsed
		}
	case "webhook_bwlimit":
		cfg.WebhookBWLimit = value
	case "webhook_job_timeout":
		cfg.WebhookJobTimeout = value
	case "webhook_http_timeout":
		cfg.WebhookHTTPTimeout = value
	case "webhook_max_rclone_log_bytes":
		if parsed, err := strconv.Atoi(value); err == nil {
			cfg.WebhookMaxRcloneLogSize = parsed
		}
	case "webhook_allow_anonymous":
		cfg.WebhookAllowAnonymous = value == "true"
	case "webhook_allowed_callback_hosts":
		cfg.AllowedCallbackHosts = splitSettingList(value)
	case "webhook_allowed_curl_hosts":
		cfg.AllowedCurlHosts = splitSettingList(value)
	}
}

func splitSettingList(value string) []string {
	if strings.TrimSpace(value) == "" {
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

// requireTokenQuery middleware checks ?token= query param against configured API token.
// Returns 403 if token is set in config but doesn't match.
func requireTokenQuery(c *gin.Context) {
	if cfgGlobal.APIToken == "" {
		// Token protection is disabled
		c.Next()
		return
	}
	token := c.Query("token")
	if token != cfgGlobal.APIToken {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "invalid or missing token"})
		return
	}
	c.Next()
}

// getTokenInfo returns whether token protection is enabled and the token value (masked).
func getTokenInfo(c *gin.Context) {
	enabled := cfgGlobal.APIToken != ""
	masked := ""
	if enabled && len(cfgGlobal.APIToken) > 4 {
		masked = strings.Repeat("*", len(cfgGlobal.APIToken)-4) + cfgGlobal.APIToken[len(cfgGlobal.APIToken)-4:]
	}
	c.JSON(http.StatusOK, gin.H{
		"enabled": enabled,
		"token":   cfgGlobal.APIToken,
		"masked":  masked,
	})
}

// updateToken updates the API token in memory and persists it to SystemSetting table.
func updateToken(c *gin.Context) {
	var req struct {
		Token string `json:"token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	cfgGlobal.APIToken = req.Token
	// Persist to env/system setting for hot-reload awareness
	var setting models.SystemSetting
	db.Where("`key` = ?", "api_token").FirstOrCreate(&setting, models.SystemSetting{Key: "api_token"})
	setting.Value = req.Token
	db.Save(&setting)
	c.JSON(http.StatusOK, gin.H{"message": "token updated"})
}

// Auth handlers
func handleLogin(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var user models.User
	if err := db.Where("username = ?", req.Username).First(&user).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	// Verify password using bcrypt
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token": "demo-token",
		"user": gin.H{
			"id":       user.ID,
			"username": user.Username,
			"is_admin": user.IsAdmin,
		},
	})
}

func handleRegister(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user := models.User{
		Username: req.Username,
		Password: req.Password,
		IsAdmin:  false,
	}

	if err := db.Create(&user).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, user)
}

// Password change handler
func handleChangePassword(c *gin.Context) {
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Get current user from context (simplified - in production use JWT)
	var user models.User
	if err := db.Where("username = ?", "admin").First(&user).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	// Verify current password using bcrypt
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.CurrentPassword)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "current password is incorrect"})
		return
	}

	// Hash new password with bcrypt
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to hash password"})
		return
	}

	// Update password
	user.Password = string(hashedPassword)
	db.Save(&user)

	c.JSON(http.StatusOK, gin.H{"message": "password changed successfully"})
}

// clampRcloneParams enforces hard caps on memory-hungry flags so that a user
// cannot accidentally request enough RAM to OOM the host.
// Caps preserve the old max values; defaults in models.go are what actually drop RAM.
func clampRcloneParams(task *models.Task) {
	if task.Transfers <= 0 {
		task.Transfers = 8
	} else if task.Transfers > maxTransfers {
		task.Transfers = maxTransfers
	}
	if task.Checkers <= 0 {
		task.Checkers = 16
	} else if task.Checkers > maxCheckers {
		task.Checkers = maxCheckers
	}
	if task.DriveChunkSize == "" {
		task.DriveChunkSize = "64M"
	} else if parseSize(task.DriveChunkSize) > parseSize(maxDriveChunkSize) {
		task.DriveChunkSize = maxDriveChunkSize
	}
	if task.BufferSize == "" {
		task.BufferSize = "64M"
	} else if parseSize(task.BufferSize) > parseSize(maxBufferSize) {
		task.BufferSize = maxBufferSize
	}
}

// parseSize converts human-readable sizes like "512M" or "1G" to a comparable
// numeric value (megabytes).  Used only for clamping.
func parseSize(s string) int64 {
	s = strings.ToUpper(strings.TrimSpace(s))
	var mult int64 = 1
	if strings.HasSuffix(s, "G") {
		mult = 1024
		s = strings.TrimSuffix(s, "G")
	} else if strings.HasSuffix(s, "M") {
		s = strings.TrimSuffix(s, "M")
	} else if strings.HasSuffix(s, "K") {
		mult = 0
		s = strings.TrimSuffix(s, "K")
	}
	v, _ := strconv.ParseInt(s, 10, 64)
	return v * mult
}

// Task handlers
func applyRuntimeStatus(task *models.Task) {
	if task == nil {
		return
	}
	if executor.IsRunning(task.ID) {
		task.Status = "running"
		return
	}
	if task.Status == "paused" || task.Status == "canceled" {
		return
	}
	if task.LastError != "" {
		task.Status = "error"
		return
	}
	task.Status = "idle"
}

func listTasks(c *gin.Context) {
	tasks := make([]models.Task, 0)
	db.Where("is_quick_task = ?", false).Order("created_at desc").Find(&tasks)
	for i := range tasks {
		applyRuntimeStatus(&tasks[i])
	}
	c.JSON(http.StatusOK, tasks)
}

func listQuickTasks(c *gin.Context) {
	tasks := make([]models.Task, 0)
	db.Where("is_quick_task = ?", true).Order("created_at desc").Limit(50).Find(&tasks)
	for i := range tasks {
		applyRuntimeStatus(&tasks[i])
	}
	c.JSON(http.StatusOK, tasks)
}

func createTask(c *gin.Context) {
	var task models.Task
	if err := c.ShouldBindJSON(&task); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Enforce safe memory defaults / caps before the task ever reaches rclone.
	clampRcloneParams(&task)

	if task.MinAge == "" {
		task.MinAge = "10s"
	}
	if task.Retries == 0 {
		task.Retries = 3
	}
	// Trim trailing slash from OpenList URL to avoid double slashes
	if task.OpenlistURL != "" {
		task.OpenlistURL = strings.TrimRight(task.OpenlistURL, "/")
	}
	// Validate OpenList mapping JSON if provided
	if task.OpenlistMapping != "" {
		if !isValidJSON(task.OpenlistMapping) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "OpenList mapping must be a valid JSON object like '{\"op:s1\":\"/s2\"}'"})
			return
		}
	}
	if err := applyOpenlistConfigToTask(&task); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := db.Create(&task).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Start watcher if enabled
	if task.WatchEnabled {
		watch.StartTaskWatch(&task, executor)
	}
	if task.ScheduleEnabled {
		sched.AddTask(&task)
	}

	c.JSON(http.StatusCreated, task)
}

// createQuickTask creates a one-off task from file browser and starts it immediately.
func createQuickTask(c *gin.Context) {
	var req struct {
		Name               string `json:"name" binding:"required"`
		Source             string `json:"source" binding:"required"`
		SourceType         string `json:"source_type" binding:"required"`
		Dest               string `json:"dest" binding:"required"`
		DestType           string `json:"dest_type" binding:"required"`
		TransferMode       string `json:"transfer_mode"`
		OpenlistEnabled    bool   `json:"openlist_enabled"`
		OpenlistConfigID   uint   `json:"openlist_config_id"`
		OpenlistURL        string `json:"openlist_url"`
		OpenlistToken      string `json:"openlist_token"`
		OpenlistMapping    string `json:"openlist_mapping"`
		OpenlistRefreshDir string `json:"openlist_refresh_dir"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	mode := req.TransferMode
	if mode == "" {
		mode = "copy"
	}
	if req.OpenlistMapping != "" && !isValidJSON(req.OpenlistMapping) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "OpenList mapping must be a valid JSON object like '{\"op:s1\":\"/s2\"}'"})
		return
	}

	var sourceDir, sourceRemoteName string
	if req.SourceType == "remote" {
		parts := strings.SplitN(req.Source, ":", 2)
		if len(parts) == 2 {
			sourceRemoteName = parts[0]
			sourceDir = parts[1]
		} else {
			sourceDir = req.Source
		}
	} else {
		sourceDir = req.Source
	}

	var destDir, destRemoteName string
	if req.DestType == "remote" {
		parts := strings.SplitN(req.Dest, ":", 2)
		if len(parts) == 2 {
			destRemoteName = parts[0]
			destDir = parts[1]
		} else {
			destDir = req.Dest
		}
	} else {
		destDir = req.Dest
	}

	task := models.Task{
		Name:               req.Name,
		SourceType:         req.SourceType,
		SourceDir:          sourceDir,
		DestType:           req.DestType,
		RemoteName:         destRemoteName,
		RemoteDir:          destDir,
		TransferMode:       mode,
		Transfers:          8,
		Checkers:           16,
		MinAge:             "0s",
		DriveChunkSize:     "64M",
		BufferSize:         "64M",
		Retries:            3,
		Enabled:            true,
		AutoDedupe:         false,
		WatchEnabled:       false,
		ScheduleEnabled:    false,
		IsQuickTask:        true,
		OpenlistEnabled:    req.OpenlistEnabled,
		OpenlistConfigID:   req.OpenlistConfigID,
		OpenlistURL:        strings.TrimRight(req.OpenlistURL, "/"),
		OpenlistToken:      req.OpenlistToken,
		OpenlistMapping:    req.OpenlistMapping,
		OpenlistRefreshDir: req.OpenlistRefreshDir,
	}

	// If source is remote but we parsed the remote name, store full path in source_dir
	if req.SourceType == "remote" && sourceRemoteName != "" {
		task.SourceDir = sourceRemoteName + ":" + sourceDir
	}

	clampRcloneParams(&task)
	if err := applyOpenlistConfigToTask(&task); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := db.Create(&task).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Start immediately
	if err := executor.ExecuteMove(&task); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, task)
}

func getTask(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var task models.Task
	if err := db.First(&task, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	c.JSON(http.StatusOK, task)
}

func updateTask(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var task models.Task
	if err := db.First(&task, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}

	var updates models.Task
	if err := c.ShouldBindJSON(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Clamp memory params on updates as well
	clampRcloneParams(&updates)

	// Trim trailing slash from OpenList URL
	if updates.OpenlistURL != "" {
		updates.OpenlistURL = strings.TrimRight(updates.OpenlistURL, "/")
	}
	// Validate OpenList mapping JSON if provided
	if updates.OpenlistMapping != "" {
		if !isValidJSON(updates.OpenlistMapping) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "OpenList mapping must be a valid JSON object like '{\"op:s1\":\"/s2\"}'"})
			return
		}
	}
	if err := applyOpenlistConfigToTask(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Stop existing watchers/schedules
	watch.StopTaskWatch(uint(id))
	sched.RemoveTask(uint(id))

	// Update
	updates.ID = uint(id)
	if err := db.Model(&task).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// GORM struct updates ignore zero values. Persist OpenList fields explicitly so
	// disabling refresh, clearing mappings, or clearing config IDs works reliably.
	if err := db.Model(&task).Updates(map[string]interface{}{
		"openlist_enabled":     updates.OpenlistEnabled,
		"openlist_config_id":   updates.OpenlistConfigID,
		"openlist_url":         updates.OpenlistURL,
		"openlist_token":       updates.OpenlistToken,
		"openlist_mapping":     updates.OpenlistMapping,
		"openlist_refresh_dir": updates.OpenlistRefreshDir,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Restart if enabled
	if updates.WatchEnabled || task.WatchEnabled {
		db.First(&task, id)
		watch.StartTaskWatch(&task, executor)
	}
	if updates.ScheduleEnabled || task.ScheduleEnabled {
		db.First(&task, id)
		sched.AddTask(&task)
	}
	db.First(&task, id)

	c.JSON(http.StatusOK, task)
}

func deleteTask(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))

	watch.StopTaskWatch(uint(id))
	sched.RemoveTask(uint(id))
	executor.StopTask(uint(id))

	// GORM will CASCADE delete associated OutputLogs because of the
	// constraint:OnDelete:CASCADE tag on Task.OutputLogs.
	db.Delete(&models.Task{}, id)
	c.JSON(http.StatusOK, gin.H{"message": "task deleted"})
}

func startTask(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var task models.Task
	if err := db.First(&task, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	if task.IsQuickTask {
		if task.Status == "canceled" || (task.Status == "idle" && task.LastRun != nil && task.LastError == "") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "quick task already finished"})
			return
		}
		if task.Status != "paused" && task.Status != "error" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "quick task cannot continue in current state"})
			return
		}
	}

	if executor.IsRunning(uint(id)) {
		c.JSON(http.StatusConflict, gin.H{"error": "task already running"})
		return
	}

	if err := executor.ExecuteMove(&task); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// ExecuteMove now updates status / last_run internally (covers both
	// API-triggered and watcher/scheduler-triggered paths).
	c.JSON(http.StatusOK, gin.H{"message": "task started"})
}

func pauseTask(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var task models.Task
	if err := db.First(&task, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	if !task.IsQuickTask {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only quick task supports pause"})
		return
	}
	if !executor.IsRunning(uint(id)) {
		c.JSON(http.StatusConflict, gin.H{"error": "task is not running"})
		return
	}

	executor.StopTask(uint(id))
	db.Model(&task).Updates(map[string]interface{}{
		"status":     "paused",
		"last_error": "",
	})
	hub.Broadcast(fmt.Sprintf(`{"type":"task_stopped","task_id":%d}`, id))
	c.JSON(http.StatusOK, gin.H{"message": "task paused"})
}

func cancelTask(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var task models.Task
	if err := db.First(&task, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	if !task.IsQuickTask {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only quick task supports stop"})
		return
	}

	executor.StopTask(uint(id))
	db.Model(&task).Updates(map[string]interface{}{
		"status":     "canceled",
		"last_error": "已停止",
	})
	hub.Broadcast(fmt.Sprintf(`{"type":"task_stopped","task_id":%d}`, id))
	c.JSON(http.StatusOK, gin.H{"message": "task canceled"})
}

func stopTask(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	executor.StopTask(uint(id))

	var task models.Task
	db.First(&task, id)
	task.Status = "idle"
	task.LastError = ""
	db.Save(&task)

	hub.Broadcast(fmt.Sprintf(`{"type":"task_stopped","task_id":%d}`, id))

	c.JSON(http.StatusOK, gin.H{"message": "task stopped"})
}

func dedupeTask(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var task models.Task
	if err := db.First(&task, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}

	if err := executor.ExecuteDedupe(&task); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "dedupe started"})
}

func getTaskLogs(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	lines, _ := strconv.Atoi(c.DefaultQuery("lines", "100"))
	// Clamp lines to a sane range so a huge request doesn't OOM the backend.
	if lines <= 0 || lines > 5000 {
		lines = 100
	}

	logFile := fmt.Sprintf("task_%d.log", id)
	content, err := logger.ReadLog(logFile, lines)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"logs": []string{}})
		return
	}

	c.JSON(http.StatusOK, gin.H{"logs": content})
}

func getTaskStatus(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))

	var task models.Task
	db.First(&task, id)

	isRunning := executor.IsRunning(uint(id))
	status := "idle"
	if isRunning {
		status = "running"
	} else if task.Status == "paused" || task.Status == "canceled" {
		status = task.Status
	} else if task.LastError != "" {
		status = "error"
	}

	c.JSON(http.StatusOK, gin.H{
		"id":         task.ID,
		"status":     status,
		"running":    isRunning,
		"last_run":   task.LastRun,
		"last_error": task.LastError,
	})
}

// System handlers
func getSystemStats(c *gin.Context) {
	var taskCount, runningCount int64
	db.Model(&models.Task{}).Where("is_quick_task = ?", false).Count(&taskCount)

	var tasks []models.Task
	db.Where("is_quick_task = ?", false).Find(&tasks)
	for _, t := range tasks {
		if executor.IsRunning(t.ID) {
			runningCount++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"total_tasks":   taskCount,
		"running_tasks": runningCount,
		"timestamp":     time.Now(),
	})
}

func getRcloneStats(c *gin.Context) {
	stats, err := rclone.GetRcloneStats()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, stats)
}

func setLogLevel(c *gin.Context) {
	var req struct {
		Level string `json:"level"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := rclone.SetLogLevel(req.Level); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "log level updated", "level": req.Level})
}

func getSystemLogs(c *gin.Context) {
	lines, _ := strconv.Atoi(c.DefaultQuery("lines", "100"))
	// Clamp lines so a huge request doesn't OOM the backend.
	if lines <= 0 || lines > 5000 {
		lines = 100
	}
	logFile := c.DefaultQuery("file", "system.log")

	content, err := logger.ReadLog(logFile, lines)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"logs": []string{}})
		return
	}

	c.JSON(http.StatusOK, gin.H{"logs": content})
}

func cleanLogs(c *gin.Context) {
	logger.CleanLogs()
	c.JSON(http.StatusOK, gin.H{"message": "logs cleaned"})
}

// OpenList config handlers
func applyOpenlistConfigToTask(task *models.Task) error {
	if !task.OpenlistEnabled || task.OpenlistConfigID == 0 {
		return nil
	}
	var cfg models.OpenlistConfig
	if err := db.First(&cfg, task.OpenlistConfigID).Error; err != nil {
		return fmt.Errorf("OpenList config not found")
	}
	task.OpenlistURL = strings.TrimRight(cfg.URL, "/")
	task.OpenlistToken = cfg.Token
	return nil
}

func listOpenlistConfigs(c *gin.Context) {
	configs := make([]models.OpenlistConfig, 0)
	db.Order("created_at desc").Find(&configs)
	c.JSON(http.StatusOK, configs)
}

func createOpenlistConfig(c *gin.Context) {
	var cfg models.OpenlistConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	cfg.Name = strings.TrimSpace(cfg.Name)
	cfg.URL = strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	if cfg.Name == "" || cfg.URL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "配置名和 OpenList 地址不能为空"})
		return
	}
	if err := db.Create(&cfg).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, cfg)
}

func updateOpenlistConfig(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var cfg models.OpenlistConfig
	if err := db.First(&cfg, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "OpenList config not found"})
		return
	}
	var updates models.OpenlistConfig
	if err := c.ShouldBindJSON(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates.Name = strings.TrimSpace(updates.Name)
	updates.URL = strings.TrimRight(strings.TrimSpace(updates.URL), "/")
	if updates.Name == "" || updates.URL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "配置名和 OpenList 地址不能为空"})
		return
	}
	if err := db.Model(&cfg).Updates(map[string]interface{}{
		"name":  updates.Name,
		"url":   updates.URL,
		"token": updates.Token,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	db.First(&cfg, id)
	c.JSON(http.StatusOK, cfg)
}

func deleteOpenlistConfig(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	db.Delete(&models.OpenlistConfig{}, id)
	c.JSON(http.StatusOK, gin.H{"message": "OpenList config deleted"})
}

// Rclone handlers
func listRemotes(c *gin.Context) {
	configPath := "/root/.config/rclone/rclone.conf"
	content, err := os.ReadFile(configPath)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"remotes": []string{}})
		return
	}

	var remotes []string
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			remote := strings.TrimPrefix(strings.TrimSuffix(line, "]"), "[")
			remotes = append(remotes, remote)
		}
	}

	c.JSON(http.StatusOK, gin.H{"remotes": remotes})
}

func listRemoteDetails(c *gin.Context) {
	configPath := "/root/.config/rclone/rclone.conf"
	content, err := os.ReadFile(configPath)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"remotes": []map[string]string{}})
		return
	}

	type remoteDetail struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}

	var remotes []remoteDetail
	var current *remoteDetail
	lines := strings.Split(string(content), "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			remotes = append(remotes, remoteDetail{Name: name, Type: ""})
			current = &remotes[len(remotes)-1]
			continue
		}
		if current == nil {
			continue
		}
		if strings.HasPrefix(strings.ToLower(line), "type") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				current.Type = strings.TrimSpace(parts[1])
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"remotes": remotes})
}

// listRemoteDir lists a remote directory using rclone lsjson.
// Query params: remote (required), path (optional, defaults to "/")
// Returns an array of items with Name, Size, IsDir, ModTime.
func listRemoteDir(c *gin.Context) {
	remote := c.Query("remote")
	if remote == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "remote query param required"})
		return
	}
	path := c.Query("path")
	if path == "" {
		path = "/"
	}

	remotePath := fmt.Sprintf("%s:%s", remote, path)
	args := []string{"lsjson", remotePath, "--config", "/root/.config/rclone/rclone.conf"}

	cmd := exec.Command("rclone", args...)
	output, err := cmd.Output()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("rclone lsjson failed: %v", err)})
		return
	}

	var items []map[string]interface{}
	if err := json.Unmarshal(output, &items); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to parse rclone output: %v", err)})
		return
	}

	// Return simplified list: name, size, is_dir
	type DirItem struct {
		Name  string `json:"name"`
		Size  int64  `json:"size"`
		IsDir bool   `json:"is_dir"`
		Path  string `json:"path"`
	}
	var result []DirItem
	for _, item := range items {
		name, _ := item["Name"].(string)
		size, _ := item["Size"].(float64)
		isDir, _ := item["IsDir"].(bool)
		result = append(result, DirItem{
			Name:  name,
			Size:  int64(size),
			IsDir: isDir,
			Path:  strings.TrimRight(path, "/") + "/" + name,
		})
	}

	c.JSON(http.StatusOK, gin.H{"items": result})
}

func createRemoteDir(c *gin.Context) {
	var req struct {
		Remote string `json:"remote" binding:"required"`
		Path   string `json:"path" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	remote := strings.TrimSpace(req.Remote)
	path := strings.TrimSpace(req.Path)
	if remote == "" || path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "remote and path are required"})
		return
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	remotePath := fmt.Sprintf("%s:%s", remote, path)
	cmd := exec.Command("rclone", "mkdir", remotePath, "--config", "/root/.config/rclone/rclone.conf")
	if output, err := cmd.CombinedOutput(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("rclone mkdir failed: %v %s", err, strings.TrimSpace(string(output)))})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "directory created", "remote": remote, "path": path})
}

func getRcloneConfig(c *gin.Context) {
	configPath := "/root/.config/rclone/rclone.conf"
	content, err := os.ReadFile(configPath)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"content": ""})
		return
	}

	c.JSON(http.StatusOK, gin.H{"content": string(content)})
}

// File browser types
type FileItem struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	Path    string    `json:"path"`
	ModTime time.Time `json:"mod_time"`
}

// listLocalFiles lists a local directory.
func listLocalFiles(c *gin.Context) {
	path := c.Query("path")
	if path == "" {
		path = localBrowserStart
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(localBrowserRoot, path)
	}

	rootAbs, err := filepath.Abs(localBrowserRoot)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to resolve local root: %v", err)})
		return
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid path: %v", err)})
		return
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		c.JSON(http.StatusForbidden, gin.H{"error": fmt.Sprintf("local browsing is limited to %s", localBrowserRoot)})
		return
	}

	entries, err := os.ReadDir(pathAbs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to read directory: %v", err)})
		return
	}

	var items []FileItem
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		itemPath := filepath.Join(pathAbs, entry.Name())
		items = append(items, FileItem{
			Name:    entry.Name(),
			Size:    info.Size(),
			IsDir:   entry.IsDir(),
			Path:    itemPath,
			ModTime: info.ModTime(),
		})
	}

	c.JSON(http.StatusOK, gin.H{"path": pathAbs, "items": items})
}

// listRemoteFiles lists a remote directory via rclone lsjson.
func listRemoteFiles(c *gin.Context) {
	remote := c.Query("remote")
	if remote == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "remote query param required"})
		return
	}
	path := c.Query("path")
	if path == "" {
		path = "/"
	}

	remotePath := fmt.Sprintf("%s:%s", remote, path)
	args := []string{"lsjson", remotePath, "--config", "/root/.config/rclone/rclone.conf"}

	cmd := exec.Command("rclone", args...)
	output, err := cmd.Output()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("rclone lsjson failed: %v", err)})
		return
	}

	var rawItems []map[string]interface{}
	if err := json.Unmarshal(output, &rawItems); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to parse rclone output: %v", err)})
		return
	}

	var items []FileItem
	for _, item := range rawItems {
		name, _ := item["Name"].(string)
		size, _ := item["Size"].(float64)
		isDir, _ := item["IsDir"].(bool)
		modTimeStr, _ := item["ModTime"].(string)
		var modTime time.Time
		if modTimeStr != "" {
			modTime, _ = time.Parse(time.RFC3339, modTimeStr)
		}
		items = append(items, FileItem{
			Name:    name,
			Size:    int64(size),
			IsDir:   isDir,
			Path:    strings.TrimRight(path, "/") + "/" + name,
			ModTime: modTime,
		})
	}

	c.JSON(http.StatusOK, gin.H{"path": path, "remote": remote, "items": items})
}

// ============================
// Output logs handlers (persistent, stored in DB)
// ============================

// getOutputLogs returns paginated structured output logs from the database.
// Supports filtering by task_id via query parameter.
func getOutputLogs(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	taskIDStr := c.Query("task_id")

	var total int64
	query := db.Model(&models.OutputLog{}).Where("progress = ?", 100)
	if taskIDStr != "" {
		if taskID, err := strconv.Atoi(taskIDStr); err == nil {
			query = query.Where("task_id = ?", taskID)
		}
	}
	query.Count(&total)

	var logs []models.OutputLog
	query.Order("date DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&logs)

	msg := ""
	c.JSON(http.StatusOK, models.OutputLogResponse{
		Success: true,
		Message: &msg,
		Data: models.OutputLogData{
			List:  logs,
			Total: total,
		},
	})
}

// deleteOutputLog deletes a single output log entry by its ID.
func deleteOutputLog(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	if err := db.Delete(&models.OutputLog{}, id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "output log deleted"})
}

// cleanOutputLogs removes all output log entries (optionally filtered by task_id).
func cleanOutputLogs(c *gin.Context) {
	taskIDStr := c.Query("task_id")
	query := db.Model(&models.OutputLog{})
	if taskIDStr != "" {
		if taskID, err := strconv.Atoi(taskIDStr); err == nil {
			query = query.Where("task_id = ?", taskID)
		}
	}
	// GORM v2 refuses to execute DELETE without a WHERE clause unless
	// AllowGlobalUpdate is explicitly enabled. This was the root cause of
	// the "fake clear" bug: the endpoint returned 200 but deleted nothing.
	if err := query.Session(&gorm.Session{AllowGlobalUpdate: true}).
		Delete(&models.OutputLog{}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "output logs cleaned"})
}

// isValidJSON checks if a string is a valid JSON object.
func isValidJSON(s string) bool {
	var v map[string]interface{}
	return json.Unmarshal([]byte(s), &v) == nil
}
