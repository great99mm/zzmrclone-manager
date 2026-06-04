package watcher

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"gorm.io/gorm"
	"rclone-manager/internal/logger"
	"rclone-manager/internal/models"
	"rclone-manager/internal/rclone"
)

type Watcher struct {
	watchers  map[uint]*fsnotify.Watcher
	logStops  map[uint]chan struct{}
	logPrimed map[uint]bool
	executors map[uint]*rclone.Executor
	tasks     map[uint]*models.Task
	db        *gorm.DB
	mu        sync.RWMutex
}

func NewWatcher(executor *rclone.Executor, database *gorm.DB) *Watcher {
	return &Watcher{
		watchers:  make(map[uint]*fsnotify.Watcher),
		logStops:  make(map[uint]chan struct{}),
		logPrimed: make(map[uint]bool),
		executors: make(map[uint]*rclone.Executor),
		tasks:     make(map[uint]*models.Task),
		db:        database,
	}
}

func (w *Watcher) StartTaskWatch(task *models.Task, executor *rclone.Executor) error {
	if !task.WatchEnabled {
		return nil
	}
	// Watching only works for local source directories
	if task.SourceType == "remote" {
		log.Printf("Skipping watch for task %d: source is remote", task.ID)
		return nil
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	// Watch source directory
	if err := watcher.Add(task.SourceDir); err != nil {
		watcher.Close()
		return err
	}

	// Also watch subdirectories
	filepath.Walk(task.SourceDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			watcher.Add(path)
		}
		return nil
	})

	w.mu.Lock()
	w.watchers[task.ID] = watcher
	w.executors[task.ID] = executor
	w.tasks[task.ID] = task
	w.mu.Unlock()

	go w.watchLoop(task.ID, watcher, executor)

	log.Printf("Started watching task %d: %s", task.ID, task.SourceDir)
	return nil
}

func (w *Watcher) StopTaskWatch(taskID uint) {
	w.mu.Lock()
	if watcher, exists := w.watchers[taskID]; exists {
		watcher.Close()
		delete(w.watchers, taskID)
	}
	if stop, exists := w.logStops[taskID]; exists {
		close(stop)
		delete(w.logStops, taskID)
		delete(w.logPrimed, taskID)
	}
	if _, exists := w.tasks[taskID]; exists {
		delete(w.executors, taskID)
		delete(w.tasks, taskID)
	}
	w.mu.Unlock()
	log.Printf("Stopped watching task %d", taskID)
}

func (w *Watcher) watchLoop(taskID uint, watcher *fsnotify.Watcher, executor *rclone.Executor) {
	debounceTimer := time.NewTimer(0)
	<-debounceTimer.C

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
				// Debounce: wait 10 seconds after last event before triggering
				debounceTimer.Stop()
				debounceTimer = time.NewTimer(10 * time.Second)

				go func() {
					<-debounceTimer.C

					w.mu.RLock()
					task := w.tasks[taskID]
					w.mu.RUnlock()

					if task != nil && !executor.IsRunning(taskID) {
						log.Printf("Directory change detected for task %d, triggering move", taskID)
						executor.ExecuteMove(task)
					}
				}()
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Watcher error for task %d: %v", taskID, err)
		}
	}
}

func (w *Watcher) RestartTaskWatch(task *models.Task, executor *rclone.Executor) error {
	w.StopTaskWatch(task.ID)
	if err := w.StartTaskWatch(task, executor); err != nil {
		return err
	}
	return w.StartInterfaceLogWatch(task, executor)
}

func (w *Watcher) StartInterfaceLogWatch(task *models.Task, executor *rclone.Executor) error {
	if !task.InterfaceLogEnabled {
		return nil
	}
	if !task.Enabled {
		return nil
	}
	if strings.TrimSpace(task.InterfaceLogURL) == "" {
		return fmt.Errorf("interface log URL is required")
	}
	interval := task.InterfaceLogInterval
	if interval <= 0 {
		interval = 30
	}
	if interval < 5 {
		interval = 5
	}

	stop := make(chan struct{})
	w.mu.Lock()
	if oldStop, exists := w.logStops[task.ID]; exists {
		close(oldStop)
	}
	w.logStops[task.ID] = stop
	w.logPrimed[task.ID] = false
	w.executors[task.ID] = executor
	w.tasks[task.ID] = task
	w.mu.Unlock()

	go w.interfaceLogLoop(task.ID, executor, time.Duration(interval)*time.Second, stop)
	log.Printf("Started interface log watch for task %d: %s", task.ID, task.InterfaceLogURL)
	return nil
}

func (w *Watcher) interfaceLogLoop(taskID uint, executor *rclone.Executor, interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	w.checkInterfaceLog(taskID, executor)
	for {
		select {
		case <-ticker.C:
			w.checkInterfaceLog(taskID, executor)
		case <-stop:
			return
		}
	}
}

type interfaceLogRecord struct {
	ID       int64
	Title    string
	Src      string
	Dest     string
	FilePath string
	Status   bool
}

func (w *Watcher) checkInterfaceLog(taskID uint, executor *rclone.Executor) {
	w.mu.RLock()
	task := w.tasks[taskID]
	w.mu.RUnlock()
	if task == nil || !task.InterfaceLogEnabled || !task.Enabled {
		return
	}

	records, err := fetchInterfaceLogRecords(task)
	if err != nil {
		logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("Interface log fetch failed: %v", err))
		return
	}
	w.mu.RLock()
	currentTask := w.tasks[taskID]
	w.mu.RUnlock()
	if currentTask == nil || currentTask != task || !currentTask.Enabled || !currentTask.InterfaceLogEnabled {
		return
	}
	firstPoll := !w.isInterfaceLogPrimed(task.ID) && task.InterfaceLogLastID == 0
	if len(records) == 0 {
		if firstPoll {
			w.setInterfaceLogPrimed(task.ID)
		}
		return
	}

	lastID := task.InterfaceLogLastID
	maxID := lastID
	trigger := false
	triggerPath := ""
	for _, record := range records {
		if record.ID > maxID {
			maxID = record.ID
		}
		if record.ID <= lastID || !record.Status {
			continue
		}
		matchedPath := matchingInterfaceLogPath(task, record)
		if matchedPath == "" {
			continue
		}
		trigger = true
		triggerPath = matchedPath
	}

	// First poll only establishes the baseline so old MoviePilot history does not
	// immediately trigger a full transfer when the listener is enabled.
	if firstPoll {
		w.advanceInterfaceLogLastID(task, lastID, maxID)
		w.setInterfaceLogPrimed(task.ID)
		logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("Interface log baseline set to ID %d", maxID))
		return
	}

	if !trigger {
		w.advanceInterfaceLogLastID(task, lastID, maxID)
		return
	}
	w.mu.RLock()
	currentTask = w.tasks[taskID]
	w.mu.RUnlock()
	if currentTask == nil || currentTask != task || !currentTask.Enabled || !currentTask.InterfaceLogEnabled {
		return
	}
	if executor.IsRunning(task.ID) {
		logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("Interface log matched [%s], but task is already running", triggerPath))
		return
	}
	w.advanceInterfaceLogLastID(task, lastID, maxID)
	logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("Interface log matched [%s], triggering transfer", triggerPath))
	if err := executor.ExecuteMove(task); err != nil {
		logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("Interface log trigger failed: %v", err))
	}
}

func (w *Watcher) advanceInterfaceLogLastID(task *models.Task, oldID, newID int64) {
	if newID <= oldID {
		return
	}
	task.InterfaceLogLastID = newID
	w.persistInterfaceLogLastID(task.ID, newID)
}

func matchingInterfaceLogPath(task *models.Task, record interfaceLogRecord) string {
	for _, path := range []string{record.FilePath, record.Dest, record.Src} {
		if matchesInterfaceLogPath(task, path) {
			return path
		}
	}
	return ""
}

func (w *Watcher) isInterfaceLogPrimed(taskID uint) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.logPrimed[taskID]
}

func (w *Watcher) setInterfaceLogPrimed(taskID uint) {
	w.mu.Lock()
	w.logPrimed[taskID] = true
	w.mu.Unlock()
}

func (w *Watcher) persistInterfaceLogLastID(taskID uint, lastID int64) {
	if w.db == nil {
		return
	}
	w.db.Model(&models.Task{}).Where("id = ?", taskID).Update("interface_log_last_id", lastID)
}

func fetchInterfaceLogRecords(task *models.Task) ([]interfaceLogRecord, error) {
	endpoint, err := buildInterfaceLogURL(task.InterfaceLogURL, task.InterfaceLogToken)
	if err != nil {
		return nil, err
	}
	client := http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	items := extractRecordItems(payload)
	records := make([]interfaceLogRecord, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		record := interfaceLogRecord{
			ID:       asInt64(m["id"]),
			Title:    asString(m["title"]),
			Src:      asString(m["src"]),
			Dest:     asString(m["dest"]),
			FilePath: firstNonEmpty(asString(m["file_path"]), asString(m["filePath"]), asString(m["path"])),
			Status:   asBool(m["status"], true),
		}
		if record.ID == 0 {
			record.ID = asInt64(m["record_id"])
		}
		if record.ID == 0 {
			continue
		}
		records = append(records, record)
	}
	return records, nil
}

func buildInterfaceLogURL(rawURL, token string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid interface log URL")
	}
	if !strings.Contains(parsed.Path, "/api/") {
		parsed = parsed.JoinPath("api", "v1", "history", "transfer")
	}
	query := parsed.Query()
	if strings.TrimSpace(token) != "" && query.Get("token") == "" {
		query.Set("token", strings.TrimSpace(token))
	}
	if query.Get("page") == "" {
		query.Set("page", "1")
	}
	if query.Get("count") == "" {
		query.Set("count", "50")
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func extractRecordItems(value interface{}) []interface{} {
	switch v := value.(type) {
	case []interface{}:
		return v
	case map[string]interface{}:
		if data, ok := v["data"]; ok {
			if items := extractRecordItems(data); len(items) > 0 {
				return items
			}
		}
		for _, key := range []string{"list", "records", "items", "logs"} {
			if arr, ok := v[key].([]interface{}); ok {
				return arr
			}
		}
	}
	return nil
}

func matchesInterfaceLogPath(task *models.Task, recordPath string) bool {
	recordPath = normalizePathForMatch(recordPath)
	if recordPath == "" {
		return false
	}
	matchPath := task.InterfaceLogMatchPath
	if strings.TrimSpace(matchPath) == "" {
		matchPath = task.SourceDir
	}
	matchPath = normalizePathForMatch(matchPath)
	if matchPath == "" {
		return true
	}
	return recordPath == matchPath || strings.HasPrefix(recordPath, strings.TrimRight(matchPath, "/")+"/")
}

func normalizePathForMatch(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.ToSlash(path)
	return strings.TrimRight(path, "/")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func asString(value interface{}) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case json.Number:
		return v.String()
	default:
		return ""
	}
}

func asInt64(value interface{}) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		parsed, _ := v.Int64()
		return parsed
	case string:
		parsed, _ := strconv.ParseInt(v, 10, 64)
		return parsed
	default:
		return 0
	}
}

func asBool(value interface{}, defaultValue bool) bool {
	switch v := value.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case int:
		return v != 0
	case string:
		parsed, err := strconv.ParseBool(v)
		if err == nil {
			return parsed
		}
		return v == "1" || strings.EqualFold(v, "success") || strings.EqualFold(v, "succeeded")
	default:
		return defaultValue
	}
}
