package rclone

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/logger"
	"rclone-manager/internal/models"
	"rclone-manager/internal/websocket"
)

const RcloneRCAddr = "http://127.0.0.1:5572"

// fileLineRegex matches rclone per-file transfer log lines like:
//
//	INFO  : filename.mkv: Copied (new)
//	INFO  : filename.mkv: Copied (replaced existing)
//	INFO  : filename.mkv: Deleted
//	INFO  : filename.mkv: Moved
//	INFO  : filename.mkv: Checked (rclone already there)
var fileLineRegex = regexp.MustCompile(`INFO\s*:\s*(.+?)\s*:\s*(Copied|Deleted|Moved|Transferred|Checked)`)

// statsLineRegex matches rclone --stats output like:
//
//	Transferred:    1.234 GiB / 5.678 GiB, 22%, 10.234 MiB/s, ETA 4m32s
var statsLineRegex = regexp.MustCompile(`Transferred:\s+[^,]+,\s*([\d\.]+)%`)

// transferringLineRegex matches rclone per-file progress like:
//   - filename.mkv: 22% /5.678Gi, 10.234Mi/s, 4m32s
var transferringLineRegex = regexp.MustCompile(`\*\s+(.+?):\s*([\d\.]+)%`)

var rotationHTTPStatusRegex = regexp.MustCompile(`\b(403|429)\b`)

var errTaskStopped = errors.New("task stopped")

type runningTask struct {
	cmd        *exec.Cmd
	generation uint64
	canceled   bool
}

type CompletionCallback func(success bool)

type Executor struct {
	runningTasks      map[uint]*runningTask
	resumeTimers      map[uint]chan struct{}
	generationCounter uint64
	mu                sync.RWMutex
	hub               *websocket.Hub
	db                *gorm.DB
	logQueue          chan *models.OutputLog // async log persistence queue
	recentRefresh     map[string]time.Time   // dir -> last refresh time (dedup)
	refreshMu         sync.Mutex
}

type runObserver struct {
	mu         sync.Mutex
	limitError string
	cmd        *exec.Cmd
}

func (o *runObserver) setCmd(cmd *exec.Cmd) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.cmd = cmd
	o.mu.Unlock()
}

func (o *runObserver) observe(line string) {
	if o == nil || !isRotationLimitError(line) {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.limitError == "" {
		o.limitError = strings.TrimSpace(line)
		if o.cmd != nil && o.cmd.Process != nil {
			_ = o.cmd.Process.Kill()
		}
	}
}

func (o *runObserver) LimitError() string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.limitError
}

func NewExecutor(hub *websocket.Hub, database *gorm.DB) *Executor {
	e := &Executor{
		runningTasks:  make(map[uint]*runningTask),
		resumeTimers:  make(map[uint]chan struct{}),
		hub:           hub,
		db:            database,
		logQueue:      make(chan *models.OutputLog, 1000),
		recentRefresh: make(map[string]time.Time),
	}
	if database != nil {
		go e.logWorker()
	}
	return e
}

// shouldRefresh returns true if the given directory has not been refreshed
// in the last 5 seconds.  This prevents hammering the OpenList API when
// multiple files land in the same directory.
func (e *Executor) shouldRefresh(dir string) bool {
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()
	last, exists := e.recentRefresh[dir]
	if !exists || time.Since(last) > 5*time.Second {
		e.recentRefresh[dir] = time.Now()
		return true
	}
	return false
}

// logWorker batches log writes and triggers per-file OpenList refresh.
// Each successfully transferred file causes an immediate refresh of its
// parent directory (deduped to 1 refresh per 5 s per dir).  The refresh
// runs in its own goroutine so it never blocks the DB writer.
func (e *Executor) logWorker() {
	batch := make([]*models.OutputLog, 0, 50)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		// Cache task configs so we query each task only once per batch.
		taskCache := make(map[uint]*models.Task)
		for _, log := range batch {
			e.persistLog(log)
			// ---- per-file OpenList refresh ----
			if log.Dest == "" || !log.Status {
				continue // skip failed transfers or missing dest
			}
			task, ok := taskCache[log.TaskID]
			if !ok {
				var t models.Task
				if e.db.First(&t, log.TaskID).Error != nil {
					continue
				}
				task = &t
				taskCache[log.TaskID] = task
			}
			if !task.OpenlistEnabled || task.OpenlistURL == "" {
				continue
			}
			dir := extractOpenListDir(log.Dest, task.OpenlistMapping)
			if e.shouldRefresh(dir) {
				// Async — never block the writer on a network call.
				go func(url, d, tok string) {
					ok, msg := refreshOpenList(url, d, tok)
					if !ok {
						logger.WriteLog("openlist.log", fmt.Sprintf("refresh [%s] failed: %s", d, msg))
					}
				}(task.OpenlistURL, dir, task.OpenlistToken)
			}
		}
		batch = batch[:0]
	}

	for {
		select {
		case log, ok := <-e.logQueue:
			if !ok {
				flush()
				return
			}
			if log == nil {
				continue
			}
			batch = append(batch, log)
			if len(batch) >= 50 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// persistLog performs the actual upsert with 1-minute deduplication window.
func (e *Executor) persistLog(log *models.OutputLog) {
	if e.db == nil {
		return
	}

	var existing models.OutputLog
	recent := time.Now().Add(-1 * time.Minute)
	result := e.db.Where("task_id = ? AND file_name = ? AND date > ?", log.TaskID, log.FileName, recent).First(&existing)
	if result.Error != nil {
		e.db.Create(log)
	} else {
		existing.Mode = log.Mode
		existing.Status = log.Status
		existing.Errmsg = log.Errmsg
		existing.Date = time.Now()
		if log.FileSize > 0 {
			existing.FileSize = log.FileSize
		}
		if log.Dest != "" {
			existing.Dest = log.Dest
		}
		e.db.Save(&existing)
	}
}

func (e *Executor) IsRunning(taskID uint) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	running, exists := e.runningTasks[taskID]
	if !exists || running == nil || running.canceled {
		return false
	}
	cmd := running.cmd
	if cmd == nil {
		return true
	}
	// cmd.Process != nil only means the process object was created.
	// ProcessState is set after the process exits, so we also require
	// it to be nil to report "truly running".
	return cmd.Process != nil && cmd.ProcessState == nil
}

func (e *Executor) reserveTask(taskID uint) (uint64, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if running, exists := e.runningTasks[taskID]; exists && running != nil && !running.canceled {
		return 0, false
	}
	e.generationCounter++
	generation := e.generationCounter
	e.runningTasks[taskID] = &runningTask{generation: generation}
	return generation, true
}

func (e *Executor) startReservedTask(taskID uint, generation uint64, cmd *exec.Cmd) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	running, exists := e.runningTasks[taskID]
	if !exists || running == nil || running.generation != generation || running.canceled {
		return errTaskStopped
	}
	running.cmd = cmd
	if err := cmd.Start(); err != nil {
		delete(e.runningTasks, taskID)
		return err
	}
	return nil
}

func (e *Executor) clearReservedTask(taskID uint, generation uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if running, exists := e.runningTasks[taskID]; exists && running != nil && running.generation == generation {
		delete(e.runningTasks, taskID)
	}
}

func (e *Executor) hasRunningEntry(taskID uint, generation uint64) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	running, exists := e.runningTasks[taskID]
	return exists && running != nil && running.generation == generation && !running.canceled
}

// buildSourcePath returns the rclone source path based on task's source_type.
func buildSourcePath(task *models.Task) string {
	if task.SourceType == "remote" {
		return task.SourceDir // e.g. "op:/videos"
	}
	return task.SourceDir // local path
}

// buildDestPath returns the rclone destination path based on task's dest_type.
func buildDestPath(task *models.Task) string {
	if task.DestType == "local" {
		return task.RemoteDir // local path stored in remote_dir
	}
	// remote destination (default)
	return fmt.Sprintf("%s:%s", task.RemoteName, task.RemoteDir)
}

// transferMode returns the rclone subcommand: "move", "copy", or "sync".
func transferMode(task *models.Task) string {
	switch task.TransferMode {
	case "copy", "sync":
		return task.TransferMode
	default:
		return "move"
	}
}

func (e *Executor) ExecuteMove(task *models.Task) error {
	return e.ExecuteMoveWithCallback(task, nil)
}

func (e *Executor) ExecuteMoveWithCallback(task *models.Task, callback CompletionCallback) error {
	if strings.TrimSpace(task.TaskType) == "rotation" {
		strategy := strings.TrimSpace(task.RotationStrategy)
		if strategy == "" {
			strategy = "legacy_error"
		}
		if strategy != "legacy_error" {
			return fmt.Errorf("rotation strategy %q is unsupported or not yet enabled", strategy)
		}
		return e.ExecuteRotationWithCallback(task, callback)
	}

	if e.IsRunning(task.ID) {
		return fmt.Errorf("task %d is already running", task.ID)
	}
	generation, ok := e.reserveTask(task.ID)
	if !ok {
		return fmt.Errorf("task %d is already running", task.ID)
	}

	mode := transferMode(task)
	src := buildSourcePath(task)
	dst := buildDestPath(task)

	args := []string{
		mode,
		src,
		dst,
		"--config", getRcloneConfig(task),
		"--fast-list",
		"--min-age", task.MinAge,
		"--stats", "3s",
		"--log-level", "INFO",
		"--ignore-errors",
		"--transfers", strconv.Itoa(task.Transfers),
		"--checkers", strconv.Itoa(task.Checkers),
		"--drive-chunk-size", task.DriveChunkSize,
		"--buffer-size", task.BufferSize,
		"--retries", strconv.Itoa(task.Retries),
	}

	// move-specific flags
	if mode == "move" {
		args = append(args, "--delete-empty-src-dirs")
	}

	// use-mmap and no-traverse are safe for move and copy, but not sync
	if mode != "sync" {
		args = append(args, "--use-mmap", "--no-traverse")
	}

	if task.BindIP != "" {
		args = append(args, "--bind", task.BindIP)
	}

	// Create log file for this task
	logFile := filepath.Join(logger.GetLogDir(), fmt.Sprintf("task_%d.log", task.ID))

	cmd := exec.Command("rclone", args...)

	// Setup output pipes
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		e.clearReservedTask(task.ID, generation)
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		e.clearReservedTask(task.ID, generation)
		return err
	}

	// Log file
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		e.clearReservedTask(task.ID, generation)
		return err
	}

	// Read stdout and stderr in separate goroutines. io.MultiReader was tried
	// but causes a deadlock: rclone logs to stderr, and MultiReader blocks on
	// stdout EOF before ever reading stderr, so all log data piles up in the
	// pipe buffer and WebSocket / log file get nothing.
	// os.File.WriteString is concurrency-safe at the kernel level, so both
	// goroutines can write to the same log file safely.
	go e.streamOutput(task, stdout, f, "stdout", nil)
	go e.streamOutput(task, stderr, f, "stderr", nil)

	// Start progress polling goroutine
	stopProgress := make(chan struct{})
	go e.pollProgress(task, stopProgress)

	if err := e.startReservedTask(task.ID, generation, cmd); err != nil {
		close(stopProgress)
		f.Close()
		// Start failed — roll status back to error so the UI doesn’t
		// show "running" for a process that never launched.
		if e.db != nil && !errors.Is(err, errTaskStopped) {
			now := time.Now()
			task.LastRun = &now
			e.db.Model(task).Updates(map[string]interface{}{
				"status":     "error",
				"last_error": err.Error(),
			})
		}
		return err
	}

	// Process started successfully — commit "running" state so that
	// watcher / scheduler triggered tasks also show correctly.
	if e.db != nil {
		now := time.Now()
		task.LastRun = &now
		e.db.Model(task).Updates(map[string]interface{}{
			"status":     "running",
			"last_error": "",
		})
	}

	// Push real-time notification to all connected dashboards.
	e.hub.Broadcast(fmt.Sprintf(`{"type":"task_started","task_id":%d}`, task.ID))

	// Wait for completion
	go func() {
		err := cmd.Wait()
		success := err == nil
		f.Close()
		defer func() {
			if callback != nil {
				callback(success)
			}
		}()

		e.clearReservedTask(task.ID, generation)

		// Close progress polling
		close(stopProgress)

		if err != nil {
			e.hub.Broadcast(fmt.Sprintf(`{"type":"task_error","task_id":%d,"error":"%s"}`, task.ID, err.Error()))
			logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("Task failed: %v", err))
		} else {
			e.hub.Broadcast(fmt.Sprintf(`{"type":"task_complete","task_id":%d}`, task.ID))
			logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), "Task completed successfully")
		}

		// Update final status in DB.  We only touch the row if it is still
		// "running" so that a manual "stop" (which sets status to "idle")
		// is not overwritten back to "error" by the goroutine.
		if e.db != nil {
			var current models.Task
			e.db.First(&current, task.ID)
			if current.Status == "running" {
				if err != nil {
					e.db.Model(&current).Updates(map[string]interface{}{
						"status":     "error",
						"last_error": err.Error(),
					})
				} else {
					e.db.Model(&current).Updates(map[string]interface{}{
						"status":     "idle",
						"last_error": "",
					})
				}
			}
		}

		// Scan full log file to catch any lines we might have missed
		e.scanLogFileForTransfers(task)

		// Refresh OpenList directories after successful transfer
		if task.OpenlistEnabled && task.OpenlistURL != "" && err == nil {
			e.refreshOpenListForTask(task)
		}

		// Auto dedupe if enabled
		if task.AutoDedupe && err == nil {
			time.Sleep(2 * time.Second)
			e.ExecuteDedupe(task)
		}
	}()

	return nil
}

func (e *Executor) ExecuteRotation(task *models.Task) error {
	return e.ExecuteRotationWithCallback(task, nil)
}

func (e *Executor) ExecuteRotationWithCallback(task *models.Task, callback CompletionCallback) error {
	if e.IsRunning(task.ID) {
		return fmt.Errorf("task %d is already running", task.ID)
	}
	if e.db == nil {
		return fmt.Errorf("rotation task requires database")
	}
	var current models.Task
	if err := e.db.First(&current, task.ID).Error; err != nil {
		return err
	}
	task = &current
	strategy := strings.TrimSpace(task.RotationStrategy)
	if strategy == "" {
		strategy = "legacy_error"
	}
	if strategy != "legacy_error" {
		return fmt.Errorf("rotation strategy %q is unsupported or not yet enabled", strategy)
	}
	if task.DestType == "local" {
		return fmt.Errorf("rotation task only supports remote destination")
	}

	remotes := models.ParseRotationRemotes(task.RotationRemotes)
	if len(remotes) == 0 {
		return fmt.Errorf("rotation remotes are empty")
	}
	if task.RotationMaxRounds <= 0 {
		task.RotationMaxRounds = 3
	}
	if task.RotationResumeTime == "" {
		task.RotationResumeTime = "01:00"
	}

	now := time.Now()
	if task.RotationPausedUntil != nil && task.RotationPausedUntil.After(now) {
		return fmt.Errorf("rotation task paused until %s", task.RotationPausedUntil.Format("2006-01-02 15:04"))
	}

	if task.RotationCurrentIndex < 0 || task.RotationCurrentIndex >= len(remotes) {
		task.RotationCurrentIndex = 0
	}
	if task.RotationCurrentRound < 0 || task.RotationCurrentRound >= task.RotationMaxRounds {
		task.RotationCurrentRound = 0
	}
	task.RemoteName = remotes[task.RotationCurrentIndex]

	generation, ok := e.reserveTask(task.ID)
	if !ok {
		return fmt.Errorf("task %d is already running", task.ID)
	}

	e.db.Model(task).Updates(map[string]interface{}{
		"status":                 "running",
		"last_error":             "",
		"last_run":               now,
		"remote_name":            task.RemoteName,
		"rotation_current_index": task.RotationCurrentIndex,
		"rotation_current_round": task.RotationCurrentRound,
		"rotation_paused_until":  nil,
	})
	e.hub.Broadcast(fmt.Sprintf(`{"type":"task_started","task_id":%d}`, task.ID))

	go e.runRotation(task.ID, generation, callback)
	return nil
}

func (e *Executor) runRotation(taskID uint, generation uint64, callback CompletionCallback) {
	defer e.clearReservedTask(taskID, generation)

	for {
		if !e.hasRunningEntry(taskID, generation) {
			return
		}

		var task models.Task
		if err := e.db.First(&task, taskID).Error; err != nil {
			return
		}

		remotes := models.ParseRotationRemotes(task.RotationRemotes)
		if len(remotes) == 0 {
			e.finishRotationWithError(&task, "轮转网盘为空")
			return
		}
		if task.RotationMaxRounds <= 0 {
			task.RotationMaxRounds = 3
		}
		if task.RotationCurrentIndex < 0 || task.RotationCurrentIndex >= len(remotes) {
			task.RotationCurrentIndex = 0
		}
		if task.RotationCurrentRound < 0 || task.RotationCurrentRound >= task.RotationMaxRounds {
			task.RotationCurrentRound = 0
		}

		remote := remotes[task.RotationCurrentIndex]
		task.RemoteName = remote
		e.db.Model(&task).Updates(map[string]interface{}{
			"status":                 "running",
			"last_error":             "",
			"remote_name":            remote,
			"rotation_current_index": task.RotationCurrentIndex,
			"rotation_current_round": task.RotationCurrentRound,
		})

		logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("轮转传输：第 %d/%d 轮，使用 %s", task.RotationCurrentRound+1, task.RotationMaxRounds, remote))

		observer, err := e.runRcloneBlocking(&task, generation)
		if !e.hasRunningEntry(taskID, generation) || errors.Is(err, errTaskStopped) {
			return
		}

		limitError := observer.LimitError()
		if limitError != "" {
			err = rotationLimitErr{message: limitError}
		}

		if err == nil {
			e.finishRotationSuccess(&task)
			if callback != nil {
				callback(true)
			}
			return
		}

		if isRotationLimitFailure(err) {
			message := err.Error()
			if message == "" {
				message = "当前账号触发 403/429 限制"
			}
			logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("%s 触发限制，切换下一个网盘：%s", remote, message))
			if !e.advanceRotation(&task, len(remotes), message) {
				return
			}
			continue
		}

		e.finishRotationWithError(&task, err.Error())
		if callback != nil {
			callback(false)
		}
		return
	}
}

func (e *Executor) runRcloneBlocking(task *models.Task, generation uint64) (*runObserver, error) {
	observer := &runObserver{}
	if !e.hasRunningEntry(task.ID, generation) {
		return observer, errTaskStopped
	}
	mode := transferMode(task)
	src := buildSourcePath(task)
	dst := buildDestPath(task)

	args := []string{
		mode,
		src,
		dst,
		"--config", getRcloneConfig(task),
		"--fast-list",
		"--min-age", task.MinAge,
		"--stats", "3s",
		"--log-level", "INFO",
		"--ignore-errors",
		"--transfers", strconv.Itoa(task.Transfers),
		"--checkers", strconv.Itoa(task.Checkers),
		"--drive-chunk-size", task.DriveChunkSize,
		"--buffer-size", task.BufferSize,
		"--retries", strconv.Itoa(task.Retries),
	}
	if mode == "move" {
		args = append(args, "--delete-empty-src-dirs")
	}
	if mode != "sync" {
		args = append(args, "--use-mmap", "--no-traverse")
	}
	if task.BindIP != "" {
		args = append(args, "--bind", task.BindIP)
	}

	logFile := filepath.Join(logger.GetLogDir(), fmt.Sprintf("task_%d.log", task.ID))
	cmd := exec.Command("rclone", args...)
	observer.setCmd(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return observer, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return observer, err
	}
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return observer, err
	}
	defer f.Close()

	go e.streamOutput(task, stdout, f, "stdout", observer)
	go e.streamOutput(task, stderr, f, "stderr", observer)

	stopProgress := make(chan struct{})
	go e.pollProgress(task, stopProgress)

	if err := e.startReservedTask(task.ID, generation, cmd); err != nil {
		close(stopProgress)
		return observer, err
	}

	err = cmd.Wait()
	close(stopProgress)

	if e.hasRunningEntry(task.ID, generation) {
		e.mu.Lock()
		if running, exists := e.runningTasks[task.ID]; exists && running != nil && running.generation == generation {
			running.cmd = nil
		}
		e.mu.Unlock()
	}

	if task.OpenlistEnabled && task.OpenlistURL != "" && err == nil {
		e.refreshOpenListForTask(task)
	}
	if task.AutoDedupe && err == nil {
		time.Sleep(2 * time.Second)
		e.ExecuteDedupe(task)
	}
	return observer, err
}

func (e *Executor) finishRotationSuccess(task *models.Task) {
	e.db.Model(task).Updates(map[string]interface{}{
		"status":                "idle",
		"last_error":            "",
		"rotation_paused_until": nil,
	})
	e.hub.Broadcast(fmt.Sprintf(`{"type":"task_complete","task_id":%d}`, task.ID))
	logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), "轮转传输完成")
}

func (e *Executor) finishRotationWithError(task *models.Task, message string) {
	e.db.Model(task).Updates(map[string]interface{}{
		"status":     "error",
		"last_error": message,
	})
	e.hub.Broadcast(fmt.Sprintf(`{"type":"task_error","task_id":%d,"error":"%s"}`, task.ID, escapeJSON(message)))
	logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("轮转传输失败: %s", message))
}

func (e *Executor) advanceRotation(task *models.Task, remoteCount int, reason string) bool {
	nextIndex := task.RotationCurrentIndex + 1
	nextRound := task.RotationCurrentRound
	if nextIndex >= remoteCount {
		nextIndex = 0
		nextRound++
	}

	if nextRound >= task.RotationMaxRounds {
		pausedUntil := nextRotationResumeAt(task.RotationResumeTime, time.Now())
		message := fmt.Sprintf("已连续轮转 %d 轮仍触发限制，暂停至 %s 后自动恢复", task.RotationMaxRounds, pausedUntil.Format("2006-01-02 15:04"))
		e.db.Model(task).Updates(map[string]interface{}{
			"status":                 "paused",
			"last_error":             message,
			"rotation_current_index": 0,
			"rotation_current_round": 0,
			"rotation_paused_until":  pausedUntil,
		})
		e.hub.Broadcast(fmt.Sprintf(`{"type":"task_error","task_id":%d,"error":"%s"}`, task.ID, escapeJSON(message)))
		logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("%s；最后错误：%s", message, reason))
		e.ScheduleRotationResume(task.ID, pausedUntil)
		return false
	}

	e.db.Model(task).Updates(map[string]interface{}{
		"rotation_current_index": nextIndex,
		"rotation_current_round": nextRound,
		"last_error":             "",
	})
	return true
}

func (e *Executor) ScheduleRotationResume(taskID uint, resumeAt time.Time) {
	e.CancelRotationResume(taskID)
	done := make(chan struct{})
	e.mu.Lock()
	e.resumeTimers[taskID] = done
	e.mu.Unlock()

	go func() {
		delay := time.Until(resumeAt)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-done:
			return
		}
		e.mu.Lock()
		if current, ok := e.resumeTimers[taskID]; ok && current == done {
			delete(e.resumeTimers, taskID)
		}
		e.mu.Unlock()
		if e.db == nil {
			return
		}
		var task models.Task
		if err := e.db.First(&task, taskID).Error; err != nil {
			return
		}
		strategy := strings.TrimSpace(task.RotationStrategy)
		if strategy == "" {
			strategy = "legacy_error"
		}
		if strategy != "legacy_error" {
			e.mu.Lock()
			if current, ok := e.resumeTimers[taskID]; ok && current == done {
				delete(e.resumeTimers, taskID)
			}
			e.mu.Unlock()
			return
		}
		if task.TaskType != "rotation" || !task.Enabled || task.Status != "paused" {
			return
		}
		if task.RotationPausedUntil != nil && task.RotationPausedUntil.After(time.Now()) {
			return
		}
		result := e.db.Model(&models.Task{}).
			Where("id = ? AND task_type = ? AND status = ? AND (rotation_strategy = '' OR rotation_strategy = ?)", task.ID, "rotation", "paused", "legacy_error").
			Updates(map[string]interface{}{
			"status":                 "idle",
			"last_error":             "",
			"rotation_current_index": 0,
			"rotation_current_round": 0,
			"rotation_paused_until":  nil,
		})
		if result.Error != nil || result.RowsAffected != 1 {
			return
		}
		task.Status = "idle"
		task.LastError = ""
		task.RotationCurrentIndex = 0
		task.RotationCurrentRound = 0
		task.RotationPausedUntil = nil
		if err := e.ExecuteMove(&task); err != nil {
			logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("轮转恢复启动失败: %v", err))
		}
	}()
}

func (e *Executor) CancelRotationResume(taskID uint) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if done, exists := e.resumeTimers[taskID]; exists {
		delete(e.resumeTimers, taskID)
		close(done)
	}
}

func nextRotationResumeAt(value string, now time.Time) time.Time {
	parts := strings.Split(strings.TrimSpace(value), ":")
	hour, minute := 1, 0
	if len(parts) >= 2 {
		if parsedHour, err := strconv.Atoi(parts[0]); err == nil && parsedHour >= 0 && parsedHour <= 23 {
			hour = parsedHour
		}
		if parsedMinute, err := strconv.Atoi(parts[1]); err == nil && parsedMinute >= 0 && parsedMinute <= 59 {
			minute = parsedMinute
		}
	}
	resume := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !resume.After(now) {
		resume = resume.Add(24 * time.Hour)
	}
	return resume
}

type rotationLimitErr struct {
	message string
}

func (e rotationLimitErr) Error() string {
	return e.message
}

func isRotationLimitFailure(err error) bool {
	if err == nil {
		return false
	}
	var limit rotationLimitErr
	if errors.As(err, &limit) {
		return true
	}
	return isRotationLimitError(err.Error())
}

func isRotationLimitError(text string) bool {
	if isSuccessfulInfoTransferLine(text) {
		return false
	}
	lower := strings.ToLower(text)
	phrasePatterns := []string{
		"too many requests",
		"rate limit",
		"ratelimit",
		"user rate limit",
		"quota exceeded",
		"daily limit",
		"download quota",
		"upload limit",
		"storage quota",
	}
	for _, pattern := range phrasePatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	contextualError := strings.Contains(lower, "error") ||
		strings.Contains(lower, "failed") ||
		strings.Contains(lower, "fatal") ||
		strings.Contains(lower, "googleapi") ||
		strings.Contains(lower, "http") ||
		strings.Contains(lower, "status code") ||
		strings.Contains(lower, "response")
	return contextualError && rotationHTTPStatusRegex.MatchString(lower)
}

func isSuccessfulInfoTransferLine(text string) bool {
	line := strings.TrimSpace(text)
	if !strings.HasPrefix(line, "INFO") {
		return false
	}
	return fileLineRegex.MatchString(line) &&
		!strings.Contains(line, "ERROR") &&
		!strings.Contains(line, "Failed") &&
		!strings.Contains(line, "failed")
}

func escapeJSON(value string) string {
	data, _ := json.Marshal(value)
	if len(data) < 2 {
		return strings.ReplaceAll(value, `"`, `\"`)
	}
	return string(data[1 : len(data)-1])
}

func (e *Executor) ExecuteDedupe(task *models.Task) error {
	// Dedupe only makes sense for remote destinations
	if task.DestType == "local" {
		return nil
	}

	args := []string{
		"dedupe",
		fmt.Sprintf("%s:%s", task.RemoteName, task.RemoteDir),
		"--config", getRcloneConfig(task),
		"--dedupe-mode", "newest",
		"-q",
	}

	cmd := exec.Command("rclone", args...)
	output, err := cmd.CombinedOutput()

	if err != nil {
		logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("Dedupe failed: %v - %s", err, string(output)))
		return err
	}

	logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), "Dedupe completed")
	return nil
}

func (e *Executor) StopTask(taskID uint) error {
	e.mu.Lock()
	running, exists := e.runningTasks[taskID]
	if exists && running != nil {
		running.canceled = true
		if running.cmd != nil && running.cmd.Process != nil {
			_ = running.cmd.Process.Kill()
		}
	}
	delete(e.runningTasks, taskID)
	e.mu.Unlock()
	e.CancelRotationResume(taskID)
	if e.db != nil {
		e.db.Model(&models.Task{}).Where("id = ?", taskID).Updates(map[string]interface{}{
			"status":     "idle",
			"last_error": "",
		})
	}
	return nil
}

// streamOutput reads from the pipe line-by-line (using bufio.Scanner) and
// forwards each complete line to the log file, WebSocket and database queue.
//
// FIX: set a max token size (64KB) so rclone output lines that contain
// extremely long pathnames don't cause the Scanner to auto-grow its buffer
// into multi-megabyte territory.
func (e *Executor) streamOutput(task *models.Task, reader io.Reader, logFile *os.File, streamType string, observer *runObserver) {
	scanner := bufio.NewScanner(reader)
	// Cap individual line buffer at 64KB.  This prevents unbounded memory
	// growth when rclone prints very long single-line JSON / path output.
	const maxScanTokenSize = 64 * 1024
	scanBuf := make([]byte, 4096) // initial 4KB buffer
	scanner.Buffer(scanBuf, maxScanTokenSize)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if observer != nil && streamType == "stderr" {
			observer.observe(line)
		}

		timestamp := time.Now().Format("2006-01-02 15:04:05")

		// Write to log file
		logFile.WriteString(fmt.Sprintf("[%s] %s\n", timestamp, line))

		// Send to WebSocket
		msg := fmt.Sprintf(`{"type":"log","task_id":%d,"task_name":"%s","stream":"%s","content":"%s","time":"%s"}`,
			task.ID, task.Name, streamType, strings.ReplaceAll(line, `"`, `\"`), timestamp)
		e.hub.Broadcast(msg)

		// Parse and enqueue structured output log for async persistence
		e.parseAndSaveLog(task, line)

		// Parse stats progress from stderr and broadcast via WebSocket
		// (rclone --stats output goes to stderr)
		if streamType == "stderr" {
			e.parseStatsProgress(task, line)
		}
	}
}

// parseStatsProgress extracts transfer percentage from rclone --stats output
// and broadcasts file_progress WebSocket messages.
func (e *Executor) parseStatsProgress(task *models.Task, line string) {
	// Try overall progress: "Transferred: 1.234 GiB / 5.678 GiB, 22%, ..."
	if matches := statsLineRegex.FindStringSubmatch(line); len(matches) >= 2 {
		percentage, _ := strconv.ParseFloat(matches[1], 64)
		msg := fmt.Sprintf(`{"type":"file_progress","task_id":%d,"file_name":"%s","progress":%.1f,"bytes":0,"size":0,"speed":0}`,
			task.ID, "", percentage)
		e.hub.Broadcast(msg)
		return
	}

	// Try per-file progress: "* filename.mkv: 22% /5.678Gi, 10.234Mi/s, 4m32s"
	if matches := transferringLineRegex.FindStringSubmatch(line); len(matches) >= 3 {
		percentage, _ := strconv.ParseFloat(matches[2], 64)
		fileName := strings.TrimSpace(matches[1])
		msg := fmt.Sprintf(`{"type":"file_progress","task_id":%d,"file_name":"%s","progress":%.1f,"bytes":0,"size":0,"speed":0}`,
			task.ID, strings.ReplaceAll(fileName, `"`, `\"`), percentage)
		e.hub.Broadcast(msg)
	}
}

// parseAndSaveLog parses a single log line and enqueues it for async persistence.
func (e *Executor) parseAndSaveLog(task *models.Task, line string) {
	if e.db == nil {
		return
	}

	line = strings.TrimSpace(line)
	if line == "" {
		return
	}

	// Try to match file transfer lines like:
	// INFO  : filename.mkv: Copied (new)
	// INFO  : filename.mkv: Deleted
	matches := fileLineRegex.FindStringSubmatch(line)
	if len(matches) >= 3 {
		fileName := strings.TrimSpace(matches[1])
		action := matches[2]

		// Resolve full source path based on source_type
		var srcPath string
		var srcStorage string
		var fileSize int64
		if task.SourceType == "remote" {
			// source_dir is "remote:/path" — file is relative to remote dir
			srcPath = fmt.Sprintf("%s/%s", strings.TrimRight(task.SourceDir, "/"), fileName)
			// Extract remote name for storage field
			if idx := strings.Index(task.SourceDir, ":"); idx >= 0 {
				srcStorage = task.SourceDir[:idx]
			} else {
				srcStorage = "remote"
			}
		} else {
			if filepath.IsAbs(fileName) {
				srcPath = fileName
			} else {
				srcPath = filepath.Join(task.SourceDir, fileName)
			}
			srcStorage = "local"
			// Get file size if source file still exists (local only)
			if info, err := os.Stat(srcPath); err == nil {
				fileSize = info.Size()
			}
		}

		// Resolve dest path based on dest_type
		var destPath string
		var destStorage string
		if task.DestType == "local" {
			destPath = filepath.Join(task.RemoteDir, fileName) // local dest
			destStorage = "local"
		} else {
			destPath = fmt.Sprintf("%s:%s/%s", task.RemoteName, strings.TrimSuffix(task.RemoteDir, "/"), fileName)
			destStorage = task.RemoteName
		}

		fileExt := strings.TrimPrefix(filepath.Ext(fileName), ".")
		status := true
		errmsg := ""

		// If the line contains error indicators, mark as failed
		if strings.Contains(line, "ERROR") || strings.Contains(line, "Failed") || strings.Contains(line, "failed") {
			status = false
			errmsg = line
		}

		log := &models.OutputLog{
			TaskID:      task.ID,
			Src:         srcPath,
			SrcStorage:  srcStorage,
			Dest:        destPath,
			DestStorage: destStorage,
			Mode:        action,
			FileName:    fileName,
			FileSize:    fileSize,
			FileExt:     fileExt,
			Status:      status,
			Progress:    100,
			Errmsg:      errmsg,
			Date:        time.Now(),
		}

		// Non-blocking send to queue. If the queue is full we drop the log
		// rather than stall the rclone pipe reader. In practice with a 1000
		// slot buffer and a fast serial writer this should never happen.
		select {
		case e.logQueue <- log:
		default:
			// Queue full, drop the log to keep rclone running smoothly
		}
	}
}

// scanLogFileForTransfers reads the task log file line-by-line after completion
// and inserts any transfer lines that were missed during streaming.
//
// FIX: the old implementation used os.ReadFile which loads the ENTIRE file
// into memory.  For large transfer jobs the log file can be hundreds of MB,
// causing a sharp post-completion memory spike.  Now we use bufio.Scanner
// which uses a fixed ~4KB buffer regardless of file size.
func (e *Executor) scanLogFileForTransfers(task *models.Task) {
	if e.db == nil {
		return
	}

	logFilePath := filepath.Join(logger.GetLogDir(), fmt.Sprintf("task_%d.log", task.ID))
	f, err := os.Open(logFilePath)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Same 64KB cap as streamOutput for consistency.
	const maxScanTokenSize = 64 * 1024
	scanBuf := make([]byte, 4096)
	scanner.Buffer(scanBuf, maxScanTokenSize)

	for scanner.Scan() {
		e.parseAndSaveLog(task, scanner.Text())
	}
}

// pollProgress periodically queries rclone core/stats and broadcasts file transfer progress via WebSocket.
func (e *Executor) pollProgress(task *models.Task, stop <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			stats, err := GetRcloneStats()
			if err != nil {
				continue
			}
			transferring, ok := stats["transferring"].([]interface{})
			if !ok || len(transferring) == 0 {
				continue
			}
			for _, t := range transferring {
				item, ok := t.(map[string]interface{})
				if !ok {
					continue
				}
				name, _ := item["name"].(string)
				percentage, _ := item["percentage"].(float64)
				bytesDone, _ := item["bytes"].(float64)
				size, _ := item["size"].(float64)
				speed, _ := item["speed"].(float64)
				if name == "" {
					continue
				}
				msg := fmt.Sprintf(`{"type":"file_progress","task_id":%d,"file_name":"%s","progress":%.1f,"bytes":%.0f,"size":%.0f,"speed":%.0f}`,
					task.ID, strings.ReplaceAll(name, `"`, `\"`), percentage, bytesDone, size, speed)
				e.hub.Broadcast(msg)
			}
		}
	}
}

func getRcloneConfig(task *models.Task) string {
	if task.RcloneConfig != "" {
		return task.RcloneConfig
	}
	return models.DefaultRcloneConfigPath
}

// RC API helpers
func RCCall(endpoint string, params map[string]interface{}) (map[string]interface{}, error) {
	url := fmt.Sprintf("%s/%s", RcloneRCAddr, endpoint)

	jsonData, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}

	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	return result, nil
}

func GetRcloneStats() (map[string]interface{}, error) {
	return RCCall("core/stats", nil)
}

func SetLogLevel(level string) error {
	_, err := RCCall("options/set", map[string]interface{}{
		"main": map[string]interface{}{
			"LogLevel": level,
		},
	})
	return err
}

// extractOpenListDir extracts the directory path from rclone dest path,
// then applies any configured path mapping for OpenList refresh.
// e.g., "op:/s1/a.txt" -> "/s1", "op:/s1/sub/b.txt" -> "/s1/sub"
// With mapping {"op:s1":"/s2"}, "op:s1/a.txt" -> "/s2"
func extractOpenListDir(destPath, mappingJSON string) string {
	// destPath format: "remote_name:remote_dir/filename"
	// Remove the remote_name: prefix
	parts := strings.SplitN(destPath, ":", 2)
	if len(parts) < 2 {
		return "/"
	}
	// parts[1] is like "/s1/a.txt" or "s1/a.txt"
	dir := filepath.Dir(parts[1])
	// Ensure Unix-style path
	dir = filepath.ToSlash(dir)
	if dir == "." {
		dir = "/"
	}

	// Apply path mapping if configured
	if mappingJSON != "" {
		var mappings map[string]string
		if err := json.Unmarshal([]byte(mappingJSON), &mappings); err == nil {
			dir = applyOpenListMapping(destPath, dir, mappings)
		}
	}

	return dir
}

// applyOpenListMapping applies configured path mappings to the OpenList directory.
// Mapping key format: "op:s1" or "op:/s1", value format: "/s2"
// The remote_name prefix is stripped before matching.
func applyOpenListMapping(destPath, dir string, mappings map[string]string) string {
	// destPath format: "remote_name:remote_dir/filename"
	parts := strings.SplitN(destPath, ":", 2)
	if len(parts) < 2 {
		return dir
	}
	// remotePath is like "/s1/a.txt" or "s1/a.txt" (without remote_name)
	remotePath := parts[1]
	remotePath = filepath.ToSlash(remotePath)

	for key, val := range mappings {
		// Normalize key: "op:s1" -> "s1" (strip remote prefix)
		keyPath := key
		if idx := strings.Index(key, ":"); idx >= 0 {
			keyPath = key[idx+1:]
		}
		keyPath = filepath.ToSlash(keyPath)
		// Ensure key path starts with /
		if !strings.HasPrefix(keyPath, "/") {
			keyPath = "/" + keyPath
		}

		// Check if remotePath starts with keyPath
		dirPart := filepath.ToSlash(filepath.Dir(remotePath))
		if dirPart == keyPath || strings.HasPrefix(dirPart, keyPath+"/") {
			// Replace matched prefix with mapped value
			newDir := strings.Replace(dirPart, keyPath, val, 1)
			return newDir
		}
	}

	return dir
}

// refreshOpenList calls the OpenList API to refresh the specified directory.
func refreshOpenList(openlistURL, dir, token string) (bool, string) {
	if openlistURL == "" {
		return false, "OpenList URL not configured"
	}

	apiURL, err := url.Parse(openlistURL)
	if err != nil {
		return false, fmt.Sprintf("Invalid OpenList URL: %v", err)
	}

	// Append /api/fs/list to the base URL
	apiURL = apiURL.JoinPath("api", "fs", "list")

	payload := map[string]interface{}{
		"path":     dir,
		"refresh":  true,
		"page":     1,
		"per_page": 0,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Sprintf("Failed to marshal request: %v", err)
	}

	req, err := http.NewRequest("POST", apiURL.String(), bytes.NewBuffer(jsonData))
	if err != nil {
		return false, fmt.Sprintf("Failed to create request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, fmt.Sprintf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Sprintf("Failed to read response: %v", err)
	}

	// Parse response (Alist/OpenList style)
	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		// Non-JSON response, treat as success if HTTP status is OK
		if resp.StatusCode == http.StatusOK {
			return true, string(body)
		}
		return false, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	if result.Code != 200 {
		return false, fmt.Sprintf("API error (code=%d): %s", result.Code, result.Message)
	}

	return true, "Refresh succeeded"
}

// updateOutputLogOpenListStatus updates the OpenList refresh status for matching output log records.
func (e *Executor) updateOutputLogOpenListStatus(taskID uint, fileName string, status bool, msg string) {
	if e.db == nil {
		return
	}
	e.db.Model(&models.OutputLog{}).
		Where("task_id = ? AND file_name = ?", taskID, fileName).
		Updates(map[string]interface{}{
			"openlist_status": fmt.Sprintf("%t", status),
			"openlist_msg":    msg,
		})
}

// refreshOpenListForTask refreshes OpenList directories for all successful transfers
// of the given task. It reads actual file destinations from OutputLog records
// and calls the OpenList refresh API for each file's directory.
func (e *Executor) refreshOpenListForTask(task *models.Task) {
	if e.db == nil || task.OpenlistURL == "" {
		return
	}

	// OpenList refresh only makes sense for remote destinations
	if task.DestType == "local" {
		return
	}

	// Use a recent time window to capture only transfers from this run
	recent := time.Now().Add(-5 * time.Minute)
	var logs []models.OutputLog
	e.db.Where("task_id = ? AND status = ? AND date > ?", task.ID, true, recent).Find(&logs)

	logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("[DEBUG] OpenList refresh found %d output log records", len(logs)))

	// If explicit refresh dir is set, use it directly
	if task.OpenlistRefreshDir != "" {
		dir := task.OpenlistRefreshDir
		if task.OpenlistMapping != "" {
			var mappings map[string]string
			if err := json.Unmarshal([]byte(task.OpenlistMapping), &mappings); err == nil {
				dir = applyOpenListMapping(task.RemoteName+":"+task.RemoteDir, dir, mappings)
			}
		}
		success, msg := refreshOpenList(task.OpenlistURL, dir, task.OpenlistToken)
		if success {
			logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("OpenList refresh [%s]: %s", dir, msg))
		} else {
			logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("OpenList refresh [%s] failed: %s", dir, msg))
		}
		return
	}

	if len(logs) == 0 {
		logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), "[DEBUG] No output logs found for OpenList refresh, falling back to task remote dir")
		// Fallback: use task remote dir
		dir := extractOpenListDir(fmt.Sprintf("%s:%s", task.RemoteName, task.RemoteDir), task.OpenlistMapping)
		success, msg := refreshOpenList(task.OpenlistURL, dir, task.OpenlistToken)
		if success {
			logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("OpenList refresh [%s]: %s", dir, msg))
		} else {
			logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("OpenList refresh [%s] failed: %s", dir, msg))
		}
		return
	}

	// Refresh each file's directory individually (no deduplication)
	for _, log := range logs {
		logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("[DEBUG] OutputLog ID=%d Dest=%q Mapping=%q", log.ID, log.Dest, task.OpenlistMapping))
		if log.Dest == "" {
			logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("[DEBUG] Skipping log ID=%d, empty Dest", log.ID))
			continue
		}
		dir := extractOpenListDir(log.Dest, task.OpenlistMapping)
		logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("[DEBUG] Extracted dir: %s from Dest: %s", dir, log.Dest))
		success, msg := refreshOpenList(task.OpenlistURL, dir, task.OpenlistToken)
		if success {
			logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("OpenList refresh [%s]: %s", dir, msg))
		} else {
			logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("OpenList refresh [%s] failed: %s", dir, msg))
		}

		// Update individual output log with refresh status
		e.db.Model(&models.OutputLog{}).
			Where("id = ?", log.ID).
			Updates(map[string]interface{}{
				"openlist_status": fmt.Sprintf("%t", success),
				"openlist_msg":    msg,
			})
	}
}
