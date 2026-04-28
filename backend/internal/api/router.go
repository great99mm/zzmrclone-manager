package api

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"rclone-manager/internal/config"
	"rclone-manager/internal/logger"
	"rclone-manager/internal/models"
	"rclone-manager/internal/rclone"
	"rclone-manager/internal/scheduler"
	"rclone-manager/internal/watcher"
	"rclone-manager/internal/websocket"
)

var (
	executor  *rclone.Executor
	sched     *scheduler.Scheduler
	watch     *watcher.Watcher
	hub       *websocket.Hub
)

func SetupRouter(cfg *config.Config) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.Default()

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
	if err := InitDB(cfg.DataDir); err != nil {
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

		// Tasks
		tasks := api.Group("/tasks")
		{
			tasks.GET("", listTasks)
			tasks.POST("", createTask)
			tasks.GET("/:id", getTask)
			tasks.PUT("/:id", updateTask)
			tasks.DELETE("/:id", deleteTask)
			tasks.POST("/:id/start", startTask)
			tasks.POST("/:id/stop", stopTask)
			tasks.POST("/:id/dedupe", dedupeTask)
			tasks.GET("/:id/logs", getTaskLogs)
			tasks.GET("/:id/status", getTaskStatus)
			tasks.GET("/:id/output-logs", getTaskOutputLogs)
		}

		// System
		api.GET("/system/stats", getSystemStats)
		api.GET("/system/rclone-stats", getRcloneStats)
		api.POST("/system/log-level", setLogLevel)
		api.GET("/system/logs", getSystemLogs)
		api.POST("/system/logs/clean", cleanLogs)

		// Rclone config
		api.GET("/rclone/remotes", listRemotes)
		api.GET("/rclone/config", getRcloneConfig)

		// Output logs (structured persistent format)
		api.GET("/output-logs", getOutputLogs)
		api.DELETE("/output-logs/:id", deleteOutputLog)
		api.DELETE("/output-logs/clean", cleanOutputLogs)
	}

	// WebSocket
	router.GET("/ws", hub.HandleWebSocket)

	return router
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

// Task handlers
func listTasks(c *gin.Context) {
	var tasks []models.Task
	db.Find(&tasks)
	c.JSON(http.StatusOK, tasks)
}

func createTask(c *gin.Context) {
	var task models.Task
	if err := c.ShouldBindJSON(&task); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Set defaults
	if task.Transfers == 0 {
		task.Transfers = 16
	}
	if task.Checkers == 0 {
		task.Checkers = task.Transfers * 2
	}
	if task.MinAge == "" {
		task.MinAge = "10s"
	}
	if task.DriveChunkSize == "" {
		task.DriveChunkSize = "256M"
	}
	if task.BufferSize == "" {
		task.BufferSize = "512M"
	}
	if task.Retries == 0 {
		task.Retries = 3
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

	// Stop existing watchers/schedules
	watch.StopTaskWatch(uint(id))
	sched.RemoveTask(uint(id))

	// Update
	updates.ID = uint(id)
	db.Model(&task).Updates(updates)

	// Restart if enabled
	if updates.WatchEnabled || task.WatchEnabled {
		db.First(&task, id)
		watch.StartTaskWatch(&task, executor)
	}
	if updates.ScheduleEnabled || task.ScheduleEnabled {
		db.First(&task, id)
		sched.AddTask(&task)
	}

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

	if executor.IsRunning(uint(id)) {
		c.JSON(http.StatusConflict, gin.H{"error": "task already running"})
		return
	}

	if err := executor.ExecuteMove(&task); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	now := time.Now()
	task.Status = "running"
	task.LastRun = &now
	db.Save(&task)

	c.JSON(http.StatusOK, gin.H{"message": "task started"})
}

func stopTask(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	executor.StopTask(uint(id))

	var task models.Task
	db.First(&task, id)
	task.Status = "idle"
	db.Save(&task)

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
	db.Model(&models.Task{}).Count(&taskCount)

	var tasks []models.Task
	db.Find(&tasks)
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

	c.JSON(http.StatusOK, gin.H{"message": "log level updated"})
}

func getSystemLogs(c *gin.Context) {
	lines, _ := strconv.Atoi(c.DefaultQuery("lines", "100"))
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

func getRcloneConfig(c *gin.Context) {
	configPath := "/root/.config/rclone/rclone.conf"
	content, err := os.ReadFile(configPath)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"content": ""})
		return
	}

	c.JSON(http.StatusOK, gin.H{"content": string(content)})
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
	query := db.Model(&models.OutputLog{})
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

// getTaskOutputLogs returns paginated output logs for a specific task.
func getTaskOutputLogs(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	// Verify task exists
	var task models.Task
	if err := db.First(&task, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}

	var total int64
	db.Model(&models.OutputLog{}).Where("task_id = ?", id).Count(&total)

	var logs []models.OutputLog
	db.Where("task_id = ?", id).Order("date DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&logs)

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
	query := db
	if taskIDStr != "" {
		if taskID, err := strconv.Atoi(taskIDStr); err == nil {
			query = query.Where("task_id = ?", taskID)
		}
	}
	query.Delete(&models.OutputLog{})
	c.JSON(http.StatusOK, gin.H{"message": "output logs cleaned"})
}
