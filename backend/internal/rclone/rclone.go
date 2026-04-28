package rclone

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"rclone-manager/internal/logger"
	"rclone-manager/internal/models"
	"rclone-manager/internal/websocket"
)

const RcloneRCAddr = "http://127.0.0.1:5572"

type Executor struct {
	runningTasks map[uint]*exec.Cmd
	mu           sync.RWMutex
	hub          *websocket.Hub
}

func NewExecutor(hub *websocket.Hub) *Executor {
	return &Executor{
		runningTasks: make(map[uint]*exec.Cmd),
		hub:          hub,
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

	// Stream output to WebSocket and log file
	go e.streamOutput(task.ID, task.Name, stdout, f, "stdout")
	go e.streamOutput(task.ID, task.Name, stderr, f, "stderr")

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

		if err != nil {
			e.hub.Broadcast(fmt.Sprintf(`{"type":"task_error","task_id":%d,"error":"%s"}`, task.ID, err.Error()))
			logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("Task failed: %v", err))
		} else {
			e.hub.Broadcast(fmt.Sprintf(`{"type":"task_complete","task_id":%d}`, task.ID))
			logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), "Task completed successfully")
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

func (e *Executor) streamOutput(taskID uint, taskName string, reader io.Reader, logFile *os.File, streamType string) {
	buf := make([]byte, 1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			line := string(buf[:n])
			timestamp := time.Now().Format("2006-01-02 15:04:05")

			// Write to log file
			logFile.WriteString(fmt.Sprintf("[%s] %s", timestamp, line))

			// Send to WebSocket
			msg := fmt.Sprintf(`{"type":"log","task_id":%d,"task_name":"%s","stream":"%s","content":"%s","time":"%s"}`,
				taskID, taskName, streamType, strings.ReplaceAll(line, `"`, `\"`), timestamp)
			e.hub.Broadcast(msg)
		}
		if err != nil {
			break
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
