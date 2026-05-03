package rclone

import (
	"bufio"
	"bytes"
	"encoding/json"
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
//   INFO  : filename.mkv: Copied (new)
//   INFO  : filename.mkv: Copied (replaced existing)
//   INFO  : filename.mkv: Deleted
//   INFO  : filename.mkv: Moved
//   INFO  : filename.mkv: Checked (rclone already there)
var fileLineRegex = regexp.MustCompile(`INFO\s*:\s*(.+?)\s*:\s*(Copied|Deleted|Moved|Transferred|Checked)`)

type Executor struct {
	runningTasks map[uint]*exec.Cmd
	mu           sync.RWMutex
	hub          *websocket.Hub
	db           *gorm.DB
	logQueue     chan *models.OutputLog // async log persistence queue
}

func NewExecutor(hub *websocket.Hub, database *gorm.DB) *Executor {
	e := &Executor{
		runningTasks: make(map[uint]*exec.Cmd),
		hub:          hub,
		db:           database,
		logQueue:     make(chan *models.OutputLog, 1000),
	}
	if database != nil {
		go e.logWorker()
	}
	return e
}

// logWorker serializes all database write operations to eliminate lock contention.
// SQLite with MaxOpenConns=1 no longer has multi-connection races, but a single
// writer goroutine also batches back-pressure and keeps rclone stdout/stderr
// readers from blocking on DB I/O.
func (e *Executor) logWorker() {
	for log := range e.logQueue {
		if log == nil {
			continue
		}
		e.persistLog(log)
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
	cmd, exists := e.runningTasks[taskID]
	if !exists || cmd == nil {
		return false
	}
	// cmd.Process != nil only means the process object was created.
	// ProcessState is set after the process exits, so we also require
	// it to be nil to report "truly running".
	return cmd.Process != nil && cmd.ProcessState == nil
}

func (e *Executor) ExecuteMove(task *models.Task) error {
	if e.IsRunning(task.ID) {
		return fmt.Errorf("task %d is already running", task.ID)
	}

	// Build rclone move command
	args := []string{
		"move",
		task.SourceDir,
		fmt.Sprintf("%s:%s", task.RemoteName, task.RemoteDir),
		"--config", getRcloneConfig(task),
		"--fast-list",
		"--min-age", task.MinAge,
		"--stats", "3s",
		"--log-level", "INFO",
		"--ignore-errors",
		"--delete-empty-src-dirs",
		"--use-mmap",
		"--no-traverse",
		"--transfers", strconv.Itoa(task.Transfers),
		"--checkers", strconv.Itoa(task.Checkers),
		"--drive-chunk-size", task.DriveChunkSize,
		"--buffer-size", task.BufferSize,
		"--retries", strconv.Itoa(task.Retries),
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
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	// Log file
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	// Read stdout and stderr in separate goroutines. io.MultiReader was tried
	// but causes a deadlock: rclone logs to stderr, and MultiReader blocks on
	// stdout EOF before ever reading stderr, so all log data piles up in the
	// pipe buffer and WebSocket / log file get nothing.
	// os.File.WriteString is concurrency-safe at the kernel level, so both
	// goroutines can write to the same log file safely.
	go e.streamOutput(task, stdout, f, "stdout")
	go e.streamOutput(task, stderr, f, "stderr")

	// Start progress polling goroutine
	stopProgress := make(chan struct{})
	go e.pollProgress(task, stopProgress)

	e.mu.Lock()
	e.runningTasks[task.ID] = cmd
	e.mu.Unlock()

	err = cmd.Start()
	if err != nil {
		f.Close()
		e.mu.Lock()
		delete(e.runningTasks, task.ID)
		e.mu.Unlock()
		// Start failed — roll status back to error so the UI doesn’t
		// show "running" for a process that never launched.
		if e.db != nil {
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
		f.Close()

		e.mu.Lock()
		delete(e.runningTasks, task.ID)
		e.mu.Unlock()

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

func (e *Executor) ExecuteDedupe(task *models.Task) error {
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
	cmd, exists := e.runningTasks[taskID]
	if exists && cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
	delete(e.runningTasks, taskID)
	e.mu.Unlock()
	return nil
}

// streamOutput reads from the pipe line-by-line (using bufio.Scanner) and
// forwards each complete line to the log file, WebSocket and database queue.
//
// FIX: set a max token size (64KB) so rclone output lines that contain
// extremely long pathnames don't cause the Scanner to auto-grow its buffer
// into multi-megabyte territory.
func (e *Executor) streamOutput(task *models.Task, reader io.Reader, logFile *os.File, streamType string) {
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

		timestamp := time.Now().Format("2006-01-02 15:04:05")

		// Write to log file
		logFile.WriteString(fmt.Sprintf("[%s] %s\n", timestamp, line))

		// Send to WebSocket
		msg := fmt.Sprintf(`{"type":"log","task_id":%d,"task_name":"%s","stream":"%s","content":"%s","time":"%s"}`,
			task.ID, task.Name, streamType, strings.ReplaceAll(line, `"`, `\"`), timestamp)
		e.hub.Broadcast(msg)

		// Parse and enqueue structured output log for async persistence
		e.parseAndSaveLog(task, line)
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

		// Resolve full source path
		var srcPath string
		if filepath.IsAbs(fileName) {
			srcPath = fileName
		} else {
			srcPath = filepath.Join(task.SourceDir, fileName)
		}
		destPath := fmt.Sprintf("%s:%s/%s", task.RemoteName, strings.TrimSuffix(task.RemoteDir, "/"), fileName)

		// Get file size if source file still exists
		var fileSize int64
		if info, err := os.Stat(srcPath); err == nil {
			fileSize = info.Size()
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
			SrcStorage:  "local",
			Dest:        destPath,
			DestStorage: task.RemoteName,
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
	return "/root/.config/rclone/rclone.conf"
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

	// Use a recent time window to capture only transfers from this run
	recent := time.Now().Add(-5 * time.Minute)
	var logs []models.OutputLog
	e.db.Where("task_id = ? AND status = ? AND date > ?", task.ID, true, recent).Find(&logs)

	logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("[DEBUG] OpenList refresh found %d output log records", len(logs)))

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
