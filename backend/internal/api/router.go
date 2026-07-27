package api

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/gin-gonic/gin/binding"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"rclone-manager/internal/auth"
	"rclone-manager/internal/config"
	"rclone-manager/internal/logger"
	"rclone-manager/internal/manualtransfer"
	"rclone-manager/internal/models"
	mountsvc "rclone-manager/internal/mounts"
	"rclone-manager/internal/proactive"
	qbsvc "rclone-manager/internal/qbittorrent"
	"rclone-manager/internal/rclone"
	"rclone-manager/internal/scheduler"
	"rclone-manager/internal/taskdispatch"
	"rclone-manager/internal/watcher"
	"rclone-manager/internal/websocket"
)

var (
	executor              *rclone.Executor
	proactiveDispatcher   *proactive.Dispatcher
	taskRunner            *taskdispatch.TaskDispatcher
	wakeConsumer          *taskdispatch.WakeConsumer
	sched                 *scheduler.Scheduler
	watch                 *watcher.Watcher
	qbWatch               *qbsvc.Watcher
	hub                   *websocket.Hub
	cfgGlobal             *config.Config
	mountMgr              *mountsvc.Manager
	manualTransferService *manualtransfer.Service
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
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	// Init database
	if wakeConsumer != nil {
		wakeConsumer.Stop()
		wakeConsumer = nil
	}
	if manualTransferService != nil {
		manualTransferService.Stop()
	}
	if err := InitDB(cfg.DataDir); err != nil {
		panic(err)
	}

	// Init WebSocket hub
	hub = websocket.NewHub()
	go hub.Run()

	// Init executor (pass db so rclone can persist structured logs)
	executor = rclone.NewExecutor(hub, db)

	// Rotation execution is disabled. Keep the generic runner for normal tasks;
	// persisted rotation rows are skipped below and never reach this launch path.
	proactiveDispatcher = nil
	taskRunner = taskdispatch.New(db, executor, nil)
	executor.LaunchFence = taskRunner
	manualTransferService = manualtransfer.NewService(db)
	manualTransferService.TaskFence = taskRunner
	manualTransferService.Runner = proactive.ExecRunner{}
	manualTransferService.LogDir = filepath.Join(cfg.DataDir, "manual-worker-logs")
	manualTransferService.ManifestDir = filepath.Join(cfg.DataDir, "manual-worker-manifests")
	manualTransferService.StageDir = filepath.Join(cfg.DataDir, "manual-worker-stage")
	if err := manualTransferService.RecoverAnalyzing(); err != nil {
		panic(err)
	}
	if err := manualTransferService.RecoverWorkers(); err != nil {
		panic(err)
	}
	manualTransferService.Start()

	// Scheduler and watcher register only after migration and recovery succeed.
	sched = scheduler.NewScheduler(taskRunner)
	sched.Start()
	watch = watcher.NewWatcher(taskRunner)
	qbWatch = qbsvc.NewWatcher(executor, db)

	// Init mount manager and auto-mount enabled mount configs
	mountMgr = mountsvc.NewManager(db, cfg.MountRoot, cfg.DataDir)
	go mountMgr.RestoreAndStartEnabled()

	// Load existing tasks and start watchers/schedules
	var tasks []models.Task
	db.Where("enabled = ?", true).Find(&tasks)
	for _, task := range tasks {
		normalizeTaskDefaults(&task)
		if task.TaskType == models.TaskTypeManual || task.TaskType == "rotation" {
			continue
		}
		startupError := ""
		if task.Status == "running" {
			startupError = "服务重启中断了任务运行，任务未自动恢复"
		}
		if startupError != "" {
			task.Status = "error"
			task.LastError = startupError
			if err := db.Model(&task).Updates(map[string]interface{}{
				"status":     task.Status,
				"last_error": task.LastError,
			}).Error; err != nil {
				_ = logger.WriteLog("system.log", fmt.Sprintf("failed to mark task %d as startup error: %v", task.ID, err))
				continue
			}
			continue
		}
		if task.WatchEnabled {
			watch.StartTaskWatch(&task, executor)
		}
		if task.ScheduleEnabled {
			sched.AddTask(&task)
		}
		if task.QBEnabled {
			qbWatch.StartTaskWatch(&task)
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
			tasks.GET("/:id/manual-accounts", requireAdminStrictTokenOrSession, listManualTaskAccounts)
			tasks.PUT("/:id/manual-accounts", requireAdminStrictTokenOrSession, updateManualTaskAccounts)
			tasks.POST("/:id/manual-runs/analyze", requireAdminStrictTokenOrSession, analyzeManualRun)
			tasks.GET("/:id/manual-runs", requireAdminStrictTokenOrSession, listManualRuns)
		}
		manualRuns := api.Group("/manual-runs")
		{
			manualRuns.POST("/:id/allocate", requireAdminStrictTokenOrSession, allocateManualRun)
			manualRuns.POST("/:id/start", requireAdminStrictTokenOrSession, startManualRun)
			manualRuns.GET("/:id/workers", requireAdminStrictTokenOrSession, listManualRunWorkers)
			manualRuns.GET("/:id/files", requireAdminStrictTokenOrSession, listManualRunFiles)
			manualRuns.GET("/:id/allocations", requireAdminStrictTokenOrSession, listManualRunFiles)
			manualRuns.GET("/:id/allocation", requireAdminStrictTokenOrSession, getManualRunAllocationSummary)
			manualRuns.GET("/:id/allocation-summary", requireAdminStrictTokenOrSession, getManualRunAllocationSummary)
			manualRuns.GET("/:id/summary", requireAdminStrictTokenOrSession, getManualRunAllocationSummary)
			manualRuns.GET("/:id/accounts", requireAdminStrictTokenOrSession, listManualRunAccounts)
			manualRuns.GET("/:id/status", requireAdminStrictTokenOrSession, getManualRunStatus)
			manualRuns.GET("/:id", requireAdminStrictTokenOrSession, getManualRun)
		}
		manualWorkers := api.Group("/manual-workers")
		{
			manualWorkers.GET("/:id", requireAdminStrictTokenOrSession, getManualWorker)
			manualWorkers.POST("/:id/cancel", requireAdminStrictTokenOrSession, cancelManualWorker)
			manualWorkers.POST("/:id/retry", requireAdminStrictTokenOrSession, retryManualWorker)
			manualWorkers.GET("/:id/logs", requireAdminStrictTokenOrSession, getManualWorkerLogs)
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
	}

	// WebSocket
	router.GET("/ws", hub.HandleWebSocket)

	return router
}

// ShutdownBackgroundServices stops durable consumers before the HTTP server exits.
func ShutdownBackgroundServices() {
	if wakeConsumer != nil {
		wakeConsumer.Stop()
	}
	if sched != nil {
		sched.Stop()
	}
	if manualTransferService != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_ = manualTransferService.StopContext(ctx)
		cancel()
	}
}

// requireStrictTokenOrSession protects mutation endpoints even when the
// optional API token is not configured. A valid browser session is required in
// that case; an arbitrary Bearer value is never sufficient.
func requireStrictTokenOrSession(c *gin.Context) {
	if cfgGlobal != nil && cfgGlobal.APIToken != "" && c.Query("token") == cfgGlobal.APIToken {
		c.Next()
		return
	}
	authorization := strings.TrimSpace(c.GetHeader("Authorization"))
	if strings.HasPrefix(strings.ToLower(authorization), "bearer ") && auth.VerifyToken(strings.TrimSpace(authorization[len("Bearer "):])) {
		c.Next()
		return
	}
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "authenticated session or exact API token required"})
}

func requireAdminStrictTokenOrSession(c *gin.Context) {
	if cfgGlobal != nil && cfgGlobal.APIToken != "" && c.Query("token") == cfgGlobal.APIToken {
		c.Set("quota_reset_actor_identity", "configured-api-token")
		c.Set("quota_reset_actor_type", models.QuotaManualResetActorPrivilegedToken)
		c.Set("manual_transfer_actor_identity", "configured-api-token")
		c.Set("manual_transfer_actor_type", "privileged_token")
		c.Next()
		return
	}
	authorization := strings.TrimSpace(c.GetHeader("Authorization"))
	if strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
		claims, ok := auth.ClaimsForToken(strings.TrimSpace(authorization[len("Bearer "):]))
		if ok && claims.IsAdmin {
			c.Set("quota_reset_actor_identity", claims.Username)
			c.Set("quota_reset_actor_type", models.QuotaManualResetActorAdminSession)
			c.Set("manual_transfer_actor_identity", claims.Username)
			c.Set("manual_transfer_actor_type", "admin_session")
			c.Next()
			return
		}
		if ok {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "administrator privileges are required"})
			return
		}
	}
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "administrator session or exact privileged API token required"})
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

// requireTokenOrSession protects read-only status access for both existing
// API-token clients and the browser's authenticated Bearer session. The
// latter follows the existing AuthMiddleware contract: a non-empty Bearer
// credential is an authenticated session, while configured query-token
// checks remain exact and unchanged for external clients.
func requireTokenOrSession(c *gin.Context) {
	if cfgGlobal == nil || cfgGlobal.APIToken == "" {
		c.Next()
		return
	}
	if c.Query("token") == cfgGlobal.APIToken {
		c.Next()
		return
	}
	authorization := strings.TrimSpace(c.GetHeader("Authorization"))
	if strings.HasPrefix(strings.ToLower(authorization), "bearer ") && auth.VerifyToken(strings.TrimSpace(authorization[len("Bearer "):])) {
		c.Next()
		return
	}
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "invalid or missing token"})
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
		"token": auth.IssueToken(user.Username, user.IsAdmin),
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

func normalizeTaskDefaults(task *models.Task) {
	if strings.TrimSpace(task.TaskType) == "" {
		task.TaskType = "normal"
	}
	if task.TaskType == models.TaskTypeManual && strings.TrimSpace(task.ManualStrategy) == "" {
		task.ManualStrategy = models.ManualStrategyAllocation
	}
	if task.ManualAccountRevision < 1 {
		task.ManualAccountRevision = 1
	}
	if task.ManualInputRevision < 1 {
		task.ManualInputRevision = 1
	}
	if task.MinAge == "" {
		task.MinAge = "10s"
	}
	if task.Retries == 0 {
		task.Retries = 3
	}
	if task.QBPollInterval <= 0 {
		task.QBPollInterval = 60
	}
}

func validateAndNormalizeTask(task *models.Task) error {
	normalizeTaskDefaults(task)
	if task.TaskType != "normal" && task.TaskType != models.TaskTypeManual {
		return fmt.Errorf("任务类型无效")
	}
	if task.TaskType == models.TaskTypeManual {
		if task.ManualStrategy != models.ManualStrategyAllocation {
			return fmt.Errorf("手动任务策略无效")
		}
		if task.SourceType != "local" || task.DestType != "remote" {
			return fmt.Errorf("手动分配任务只支持本地源和远程目标")
		}
		if strings.TrimSpace(task.SourceDir) == "" || !filepath.IsAbs(task.SourceDir) || filepath.Clean(task.SourceDir) != task.SourceDir {
			return fmt.Errorf("手动分配任务需要有效的本地源路径")
		}
		if task.TransferMode != models.TransferModeCopy && task.TransferMode != models.TransferModeMove {
			return fmt.Errorf("手动分配任务只支持复制或移动模式")
		}
		task.WatchEnabled = false
		task.ScheduleEnabled = false
		task.QBEnabled = false
		task.AutoDedupe = false
		return nil
	}
	if task.OpenlistURL != "" {
		task.OpenlistURL = strings.TrimRight(task.OpenlistURL, "/")
	}
	if task.OpenlistMapping != "" && !isValidJSON(task.OpenlistMapping) {
		return fmt.Errorf("OpenList mapping must be a valid JSON object like '{\"op:s1\":\"/s2\"}'")
	}
	if err := applyOpenlistConfigToTask(task); err != nil {
		return err
	}

	if task.QBEnabled {
		return validateQBTask(task)
	}
	return nil
}

func nextManualInputRevision(current, updates models.Task) int64 {
	revision := current.ManualInputRevision
	if revision < 1 {
		revision = 1
	}
	currentConfig := strings.TrimSpace(current.RcloneConfig)
	updatedConfig := strings.TrimSpace(updates.RcloneConfig)
	if currentConfig == "" {
		currentConfig = models.DefaultRcloneConfigPath
	}
	if updatedConfig == "" {
		updatedConfig = models.DefaultRcloneConfigPath
	}
	changed := current.TaskType != updates.TaskType || current.SourceDir != updates.SourceDir || current.RemoteName != updates.RemoteName || current.RemoteDir != updates.RemoteDir || current.TransferMode != updates.TransferMode || currentConfig != updatedConfig
	if (current.TaskType == models.TaskTypeManual || updates.TaskType == models.TaskTypeManual) && changed {
		return revision + 1
	}
	return revision
}

func proactiveMoveEnabled() bool {
	if db == nil {
		return false
	}
	var setting models.SystemSetting
	if err := db.Where("`key` = ?", models.ProactiveMoveSettingKey).First(&setting).Error; err != nil {
		return false
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(setting.Value))
	return err == nil && enabled
}

func validateQBTask(task *models.Task) error {
	if task.SourceType == "remote" {
		return fmt.Errorf("qBittorrent 完成触发只支持本地源目录")
	}
	if strings.TrimSpace(task.QBURL) == "" {
		return fmt.Errorf("启用 qBittorrent 触发时，请填写 qBittorrent 地址")
	}
	task.QBURL = strings.TrimRight(strings.TrimSpace(task.QBURL), "/")
	task.QBUsername = strings.TrimSpace(task.QBUsername)
	if task.QBPollInterval <= 0 {
		task.QBPollInterval = 60
	}
	return nil
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
	if err := c.ShouldBindBodyWith(&task, binding.JSON); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// AutoDedupe is retained only for database compatibility; dedupe requires
	// an explicit manual endpoint click.
	task.AutoDedupe = false

	// Enforce safe memory defaults / caps before the task ever reaches rclone.
	clampRcloneParams(&task)
	if err := validateAndNormalizeTask(&task); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := db.Create(&task).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if task.TaskType == models.TaskTypeManual {
		c.JSON(http.StatusCreated, task)
		return
	}

	// Start watcher if enabled
	if task.WatchEnabled {
		watch.StartTaskWatch(&task, executor)
	}
	if task.ScheduleEnabled {
		sched.AddTask(&task)
	}
	if task.QBEnabled {
		qbWatch.StartTaskWatch(&task)
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

func proactiveMutationBlocked(tasks ...models.Task) (bool, error) {
	if proactiveDispatcher == nil {
		return false, nil
	}
	for _, task := range tasks {
		blocked, err := proactiveDispatcher.MutationBlocked(task)
		if err != nil {
			return false, err
		}
		if blocked {
			return true, nil
		}
	}
	return false, nil
}

func updateTask(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	if err := taskRunner.WithTaskExclusive(c.Request.Context(), uint(id), func(_ *models.Task) error { updateTaskUnsafe(c); return nil }); err != nil {
		if errors.Is(err, taskdispatch.ErrTaskActive) {
			c.JSON(http.StatusConflict, gin.H{"error": "任务正在运行，无法更新任务配置；请先停止任务"})
			return
		}
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	}
}

func updateTaskUnsafe(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var task models.Task
	if err := db.First(&task, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	if blocked, err := proactiveMutationBlocked(task); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check proactive maintenance fence"})
		return
	} else if blocked {
		c.JSON(http.StatusConflict, gin.H{"error": "task destination scope is owned by an active manual merge"})
		return
	}

	if taskRunner.IsActive(uint(id)) {
		c.JSON(http.StatusConflict, gin.H{"error": "任务正在运行，无法更新任务配置；请先停止任务"})
		return
	}
	var updates models.Task
	if err := c.ShouldBindBodyWith(&updates, binding.JSON); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates.AutoDedupe = false

	// Clamp memory params on updates as well
	clampRcloneParams(&updates)
	if err := validateAndNormalizeTask(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if updates.TaskType == models.TaskTypeManual {
		updates.WatchEnabled = false
		updates.ScheduleEnabled = false
		updates.QBEnabled = false
		updates.AutoDedupe = false
	}
	candidate := task
	candidate.RcloneConfig = strings.TrimSpace(updates.RcloneConfig)
	if candidate.RcloneConfig == "" {
		candidate.RcloneConfig = models.DefaultRcloneConfigPath
	}
	candidate.RemoteDir = strings.TrimSpace(updates.RemoteDir)
	if blocked, err := proactiveMutationBlocked(task, candidate); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check proactive maintenance fence"})
		return
	} else if blocked {
		c.JSON(http.StatusConflict, gin.H{"error": "task destination scope is owned by an active manual merge"})
		return
	}

	// Stop existing watchers/schedules
	watch.StopTaskWatch(uint(id))
	sched.RemoveTask(uint(id))
	qbWatch.StopTaskWatch(uint(id))

	status := task.Status
	lastError := task.LastError
	if task.TaskType != updates.TaskType {
		status = "idle"
		lastError = ""
	}

	if err := db.Model(&task).Updates(map[string]interface{}{
		"name":                  updates.Name,
		"source_type":           updates.SourceType,
		"source_dir":            updates.SourceDir,
		"dest_type":             updates.DestType,
		"remote_name":           updates.RemoteName,
		"remote_dir":            updates.RemoteDir,
		"transfer_mode":         updates.TransferMode,
		"transfers":             updates.Transfers,
		"checkers":              updates.Checkers,
		"bind_ip":               updates.BindIP,
		"rclone_config":         updates.RcloneConfig,
		"enabled":               updates.Enabled,
		"auto_dedupe":           false,
		"min_age":               updates.MinAge,
		"drive_chunk_size":      updates.DriveChunkSize,
		"buffer_size":           updates.BufferSize,
		"retries":               updates.Retries,
		"schedule_enabled":      updates.ScheduleEnabled,
		"schedule_interval":     updates.ScheduleInterval,
		"watch_enabled":         updates.WatchEnabled,
		"qb_enabled":            updates.QBEnabled,
		"qb_url":                updates.QBURL,
		"qb_username":           updates.QBUsername,
		"qb_password":           updates.QBPassword,
		"qb_poll_interval":      updates.QBPollInterval,
		"qb_delete_files":       updates.QBDeleteFiles,
		"status":                status,
		"last_error":            lastError,
		"openlist_enabled":      updates.OpenlistEnabled,
		"openlist_config_id":    updates.OpenlistConfigID,
		"openlist_url":          updates.OpenlistURL,
		"openlist_token":        updates.OpenlistToken,
		"openlist_mapping":      updates.OpenlistMapping,
		"openlist_refresh_dir":  updates.OpenlistRefreshDir,
		"task_type":             updates.TaskType,
		"manual_strategy":       updates.ManualStrategy,
		"manual_input_revision": nextManualInputRevision(task, updates),
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Restart if enabled
	if updates.TaskType != models.TaskTypeManual && task.TaskType != models.TaskTypeManual && (updates.WatchEnabled || task.WatchEnabled) {
		db.First(&task, id)
		watch.StartTaskWatch(&task, executor)
	}
	if updates.TaskType != models.TaskTypeManual && task.TaskType != models.TaskTypeManual && (updates.ScheduleEnabled || task.ScheduleEnabled) {
		db.First(&task, id)
		sched.AddTask(&task)
	}
	if updates.TaskType != models.TaskTypeManual && task.TaskType != models.TaskTypeManual && (updates.QBEnabled || task.QBEnabled) {
		db.First(&task, id)
		qbWatch.StartTaskWatch(&task)
	}
	db.First(&task, id)

	c.JSON(http.StatusOK, task)
}

func deleteTask(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var existing models.Task
	if err := db.First(&existing, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	if blocked, err := proactiveMutationBlocked(existing); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check proactive maintenance fence"})
		return
	} else if blocked {
		c.JSON(http.StatusConflict, gin.H{"error": "task destination scope is owned by an active manual merge"})
		return
	}
	if err := taskRunner.WithTaskExclusive(c.Request.Context(), uint(id), func(_ *models.Task) error {
		var current models.Task
		if err := db.First(&current, id).Error; err != nil {
			return err
		}
		blocked, err := proactiveMutationBlocked(current)
		if err != nil {
			return err
		}
		if blocked {
			return proactive.ErrManualMergeConflict
		}
		watch.StopTaskWatch(uint(id))
		sched.RemoveTask(uint(id))
		qbWatch.StopTaskWatch(uint(id))
		executor.StopTask(uint(id))
		return db.Delete(&models.Task{}, id).Error
	}); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "task deleted"})
}

func startTask(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var task models.Task
	if err := db.First(&task, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	if task.TaskType == models.TaskTypeManual {
		c.JSON(http.StatusConflict, gin.H{"error": "manual tasks use analyze and allocate; they cannot be started directly"})
		return
	}
	if task.TaskType == "rotation" {
		c.JSON(http.StatusConflict, gin.H{"error": "rotation tasks are no longer supported"})
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

	if blocked, err := proactiveMutationBlocked(task); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check proactive maintenance fence"})
		return
	} else if blocked {
		c.JSON(http.StatusConflict, gin.H{"error": "task destination scope is owned by an active manual merge"})
		return
	}

	if err := taskRunner.Trigger(c.Request.Context(), uint(id), "start"); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, taskdispatch.ErrTaskActive) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
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
	if task.TaskType == models.TaskTypeManual {
		c.JSON(http.StatusConflict, gin.H{"error": "manual tasks use analyze and allocate; legacy pause is unavailable"})
		return
	}
	if task.TaskType == "rotation" {
		c.JSON(http.StatusConflict, gin.H{"error": "rotation tasks are no longer supported"})
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
	if task.TaskType == models.TaskTypeManual {
		c.JSON(http.StatusConflict, gin.H{"error": "manual tasks use analyze and allocate; legacy cancel is unavailable"})
		return
	}
	if task.TaskType == "rotation" {
		c.JSON(http.StatusConflict, gin.H{"error": "rotation tasks are no longer supported"})
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
	var task models.Task
	if err := db.First(&task, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	if task.TaskType == models.TaskTypeManual {
		c.JSON(http.StatusConflict, gin.H{"error": "manual tasks use analyze and allocate; legacy stop is unavailable"})
		return
	}
	if task.TaskType == "rotation" {
		c.JSON(http.StatusConflict, gin.H{"error": "rotation tasks are no longer supported"})
		return
	}
	executor.StopTask(uint(id))
	db.Model(&task).Updates(map[string]interface{}{
		"status":     "idle",
		"last_error": "",
	})

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
	if task.TaskType == models.TaskTypeManual {
		c.JSON(http.StatusConflict, gin.H{"error": "manual tasks do not use legacy dedupe"})
		return
	}
	if task.TaskType == "rotation" {
		c.JSON(http.StatusConflict, gin.H{"error": "rotation tasks are no longer supported"})
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

	isRunning := taskRunner.IsActive(uint(id))
	status := "idle"
	if isRunning {
		status = "running"
	} else if task.Status == "paused" || task.Status == "canceled" {
		status = task.Status
	} else if task.LastError != "" {
		status = "error"
	}

	response := gin.H{
		"id":                     task.ID,
		"status":                 status,
		"running":                isRunning,
		"last_run":               task.LastRun,
		"last_error":             task.LastError,
		"remote_name":            task.RemoteName,
		"rotation_current_index": task.RotationCurrentIndex,
		"rotation_current_round": task.RotationCurrentRound,
		"rotation_paused_until":  task.RotationPausedUntil,
	}
	if qbWatch != nil && task.QBEnabled {
		response["qb_status"] = qbWatch.Status(&task)
	}
	c.JSON(http.StatusOK, response)
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
