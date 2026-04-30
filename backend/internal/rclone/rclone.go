package rclone

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
}

func NewExecutor(hub *websocket.Hub, database *gorm.DB) *Executor {
	return &Executor{
		runningTasks: make(map[uint]*exec.Cmd),
		hub:          hub,
		db:           database,
	}
}

func (e *Executor) IsRunning(taskID uint) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	cmd, exists := e.runningTasks[taskID]
	if !exists || cmd == nil {
		return false
	}
	return cmd.Process != nil
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
		"--stats", "15s",
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

	// Stream output to WebSocket, log file and database (line-by-line)
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
		return err
	}

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
			// Mark any pending logs for this task as failed
			if e.db != nil {
				e.db.Model(&models.OutputLog{}).Where("task_id = ? AND status = ?", task.ID, true).Update("status", false)
			}
		} else {
			e.hub.Broadcast(fmt.Sprintf(`{"type":"task_complete","task_id":%d}`, task.ID))
			logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), "Task completed successfully")
		}

		// Scan full log file to catch any lines we might have missed
		e.scanLogFileForTransfers(task)

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
// forwards each complete line to the log file, WebSocket and database.
func (e *Executor) streamOutput(task *models.Task, reader io.Reader, logFile *os.File, streamType string) {
	scanner := bufio.NewScanner(reader)
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

		// Parse and persist structured output log
		e.parseAndSaveLog(task, line)
	}
}

// parseAndSaveLog parses a single log line and saves a structured OutputLog record.
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

		log := models.OutputLog{
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

		// Upsert: update if same file exists within 1 minute, otherwise create new
		var existing models.OutputLog
		recent := time.Now().Add(-1 * time.Minute)
		result := e.db.Where("task_id = ? AND file_name = ? AND date > ?", task.ID, fileName, recent).First(&existing)
		if result.Error != nil {
			e.db.Create(&log)
		} else {
			existing.Mode = action
			existing.Status = status
			existing.Errmsg = errmsg
			existing.Date = time.Now()
			e.db.Save(&existing)
		}
		return
	}
}

// scanLogFileForTransfers reads the entire task log file after completion
// and inserts any transfer lines that were missed during streaming.
func (e *Executor) scanLogFileForTransfers(task *models.Task) {
	if e.db == nil {
		return
	}

	logFilePath := filepath.Join(logger.GetLogDir(), fmt.Sprintf("task_%d.log", task.ID))
	content, err := os.ReadFile(logFilePath)
	if err != nil {
		return
	}

	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		e.parseAndSaveLog(task, line)
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
