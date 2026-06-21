package qbittorrent

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"rclone-manager/internal/logger"
	"rclone-manager/internal/models"
	"rclone-manager/internal/rclone"
)

type Watcher struct {
	executor *rclone.Executor
	mu       sync.Mutex
	stops    map[uint]chan struct{}
	seen     map[uint]map[string]bool
	queues   map[uint][]queuedTorrent
	active   map[uint]*queuedTorrent
	stats    map[uint]torrentStats
}

type queuedTorrent struct {
	Hash       string
	Name       string
	SourcePath string
}

type QueueItem struct {
	Hash       string `json:"hash"`
	Name       string `json:"name"`
	SourcePath string `json:"source_path"`
}

type Status struct {
	Enabled          bool        `json:"enabled"`
	Watching         bool        `json:"watching"`
	Running          bool        `json:"running"`
	Active           *QueueItem  `json:"active"`
	Waiting          []QueueItem `json:"waiting"`
	WaitingCount     int         `json:"waiting_count"`
	TotalTorrents    int         `json:"total_torrents"`
	CompletedCount   int         `json:"completed_count"`
	MatchedCompleted int         `json:"matched_completed"`
	PollInterval     int         `json:"poll_interval"`
	LastSync         *time.Time  `json:"last_sync"`
	LastError        string      `json:"last_error"`
}

type torrentStats struct {
	Total            int
	Completed        int
	MatchedCompleted int
	LastSync         time.Time
	LastError        string
}

type torrent struct {
	Hash        string  `json:"hash"`
	Name        string  `json:"name"`
	Progress    float64 `json:"progress"`
	State       string  `json:"state"`
	ContentPath string  `json:"content_path"`
	SavePath    string  `json:"save_path"`
}

func NewWatcher(executor *rclone.Executor) *Watcher {
	return &Watcher{
		executor: executor,
		stops:    make(map[uint]chan struct{}),
		seen:     make(map[uint]map[string]bool),
		queues:   make(map[uint][]queuedTorrent),
		active:   make(map[uint]*queuedTorrent),
		stats:    make(map[uint]torrentStats),
	}
}

func (w *Watcher) StartTaskWatch(task *models.Task) error {
	if task == nil || !task.QBEnabled {
		return nil
	}
	if task.ID == 0 {
		return fmt.Errorf("qBittorrent task id is empty")
	}
	if strings.TrimSpace(task.QBURL) == "" {
		return fmt.Errorf("qBittorrent 地址不能为空")
	}
	if task.SourceType == "remote" {
		return fmt.Errorf("qBittorrent 触发只支持本地源目录")
	}

	w.StopTaskWatch(task.ID)
	stop := make(chan struct{})
	w.mu.Lock()
	w.stops[task.ID] = stop
	w.seen[task.ID] = make(map[string]bool)
	w.queues[task.ID] = nil
	delete(w.active, task.ID)
	w.mu.Unlock()

	copied := *task
	go w.loop(&copied, stop)
	log.Printf("Started qBittorrent trigger for task %d", task.ID)
	return nil
}

func (w *Watcher) StopTaskWatch(taskID uint) {
	w.mu.Lock()
	if stop, exists := w.stops[taskID]; exists {
		close(stop)
		delete(w.stops, taskID)
	}
	delete(w.seen, taskID)
	delete(w.queues, taskID)
	delete(w.active, taskID)
	delete(w.stats, taskID)
	w.mu.Unlock()
}

func (w *Watcher) Status(task *models.Task) Status {
	status := Status{}
	if task == nil {
		return status
	}
	status.Enabled = task.QBEnabled
	status.PollInterval = task.QBPollInterval
	if status.PollInterval <= 0 {
		status.PollInterval = 60
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	_, status.Watching = w.stops[task.ID]
	if active := w.active[task.ID]; active != nil {
		item := active.toQueueItem()
		status.Active = &item
		status.Running = true
	}
	queue := w.queues[task.ID]
	status.WaitingCount = len(queue)
	status.Waiting = make([]QueueItem, 0, len(queue))
	for _, tor := range queue {
		status.Waiting = append(status.Waiting, tor.toQueueItem())
	}
	stats := w.stats[task.ID]
	status.TotalTorrents = stats.Total
	status.CompletedCount = stats.Completed
	status.MatchedCompleted = stats.MatchedCompleted
	status.LastError = stats.LastError
	if !stats.LastSync.IsZero() {
		lastSync := stats.LastSync
		status.LastSync = &lastSync
	}
	return status
}

func (w *Watcher) loop(task *models.Task, stop <-chan struct{}) {
	interval := time.Duration(task.QBPollInterval) * time.Second
	if interval <= 0 {
		interval = 60 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	w.check(task)
	for {
		select {
		case <-ticker.C:
			w.check(task)
		case <-stop:
			return
		}
	}
}

func (w *Watcher) check(task *models.Task) {
	if w.executor == nil {
		return
	}

	client, err := newClient(task)
	if err != nil {
		w.updateStatsError(task.ID, err)
		logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("qBittorrent 登录失败: %v", err))
		return
	}
	torrents, err := client.torrents()
	if err != nil {
		w.updateStatsError(task.ID, err)
		logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("qBittorrent 获取种子失败: %v", err))
		return
	}
	completed := completedTorrents(torrents)
	matchedCompleted := 0

	for _, tor := range completed {
		sourcePath, ok := w.torrentSourcePath(task, tor)
		if !ok {
			continue
		}
		matchedCompleted++
		if tor.Hash == "" || w.isSeen(task.ID, tor.Hash) {
			continue
		}
		w.markSeen(task.ID, tor.Hash)
		w.enqueue(task.ID, queuedTorrent{Hash: tor.Hash, Name: tor.Name, SourcePath: sourcePath})
		logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("qBittorrent 种子完成，已加入队列: %s (%s)", tor.Name, sourcePath))
	}
	w.updateStats(task.ID, len(torrents), len(completed), matchedCompleted)
	w.runNext(task)
}

func (w *Watcher) isSeen(taskID uint, hash string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen[taskID] != nil && w.seen[taskID][hash]
}

func (w *Watcher) markSeen(taskID uint, hash string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen[taskID] == nil {
		w.seen[taskID] = make(map[string]bool)
	}
	w.seen[taskID][hash] = true
}

func (w *Watcher) unmarkSeen(taskID uint, hash string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen[taskID] != nil {
		delete(w.seen[taskID], hash)
	}
}

func (w *Watcher) updateStats(taskID uint, total, completed, matchedCompleted int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats[taskID] = torrentStats{
		Total:            total,
		Completed:        completed,
		MatchedCompleted: matchedCompleted,
		LastSync:         time.Now(),
	}
}

func (w *Watcher) updateStatsError(taskID uint, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	stats := w.stats[taskID]
	if err != nil {
		stats.LastError = err.Error()
	}
	stats.LastSync = time.Now()
	w.stats[taskID] = stats
}

func (w *Watcher) enqueue(taskID uint, tor queuedTorrent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.queues[taskID] = append(w.queues[taskID], tor)
}

func (w *Watcher) popQueue(taskID uint) (queuedTorrent, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	queue := w.queues[taskID]
	if len(queue) == 0 {
		return queuedTorrent{}, false
	}
	tor := queue[0]
	w.queues[taskID] = queue[1:]
	return tor, true
}

func (w *Watcher) setActive(taskID uint, tor queuedTorrent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	copy := tor
	w.active[taskID] = &copy
}

func (w *Watcher) clearActive(taskID uint) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.active, taskID)
}

func (tor queuedTorrent) toQueueItem() QueueItem {
	return QueueItem{Hash: tor.Hash, Name: tor.Name, SourcePath: tor.SourcePath}
}

func (w *Watcher) runNext(task *models.Task) {
	if w.executor == nil || w.executor.IsRunning(task.ID) {
		return
	}
	tor, ok := w.popQueue(task.ID)
	if !ok {
		return
	}

	triggeredTask := *task
	triggeredTask.SourceDir = tor.SourcePath
	w.setActive(task.ID, tor)
	logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("qBittorrent 队列开始单独转移: %s (%s)", tor.Name, tor.SourcePath))
	if err := w.executor.ExecuteMoveWithCallback(&triggeredTask, func(success bool) {
		defer func() {
			w.clearActive(task.ID)
			w.runNext(task)
		}()
		if !success {
			w.unmarkSeen(task.ID, tor.Hash)
			return
		}
		client, err := newClient(task)
		if err != nil {
			logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("qBittorrent 登录失败，无法删除种子 [%s]: %v", tor.Name, err))
			return
		}
		if err := client.deleteTorrent(tor.Hash, task.QBDeleteFiles); err != nil {
			logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("qBittorrent 删除种子失败 [%s]: %v", tor.Name, err))
			return
		}
		logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("qBittorrent 已删除种子: %s", tor.Name))
	}); err != nil {
		w.clearActive(task.ID)
		logger.WriteLog(fmt.Sprintf("task_%d.log", task.ID), fmt.Sprintf("qBittorrent 触发转移失败 [%s]: %v", tor.Name, err))
		w.unmarkSeen(task.ID, tor.Hash)
	}
}

func (w *Watcher) torrentSourcePath(task *models.Task, tor torrent) (string, bool) {
	sourceAbs, err := filepath.Abs(filepath.Clean(task.SourceDir))
	if err != nil {
		return "", false
	}
	paths := []string{tor.ContentPath}
	if strings.TrimSpace(tor.ContentPath) == "" && strings.TrimSpace(tor.SavePath) != "" && strings.TrimSpace(tor.Name) != "" {
		paths = append(paths, filepath.Join(tor.SavePath, tor.Name))
	}
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		abs, err := filepath.Abs(filepath.Clean(p))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(sourceAbs, abs)
		if err == nil && (rel == "." || rel == "" || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))) {
			return abs, true
		}
	}
	return "", false
}

type client struct {
	base string
	http *http.Client
}

func newClient(task *models.Task) (*client, error) {
	jar, _ := cookiejar.New(nil)
	c := &client{
		base: strings.TrimRight(task.QBURL, "/"),
		http: &http.Client{Timeout: 15 * time.Second, Jar: jar},
	}
	if task.QBUsername == "" && task.QBPassword == "" {
		return c, nil
	}
	form := url.Values{}
	form.Set("username", task.QBUsername)
	form.Set("password", task.QBPassword)
	resp, err := c.http.PostForm(c.base+"/api/v2/auth/login", form)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "Ok." {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return c, nil
}

func (c *client) torrents() ([]torrent, error) {
	resp, err := c.http.Get(c.base + "/api/v2/torrents/info")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var torrents []torrent
	if err := json.Unmarshal(body, &torrents); err != nil {
		return nil, err
	}
	sort.SliceStable(torrents, func(i, j int) bool {
		return strings.ToLower(torrents[i].Name) < strings.ToLower(torrents[j].Name)
	})
	return torrents, nil
}

func completedTorrents(torrents []torrent) []torrent {
	result := make([]torrent, 0, len(torrents))
	for _, tor := range torrents {
		if tor.Progress >= 1 || strings.Contains(strings.ToLower(tor.State), "upload") || strings.Contains(strings.ToLower(tor.State), "stalledup") {
			result = append(result, tor)
		}
	}
	return result
}

func (c *client) deleteTorrent(hash string, deleteFiles bool) error {
	form := url.Values{}
	form.Set("hashes", hash)
	form.Set("deleteFiles", fmt.Sprintf("%t", deleteFiles))
	resp, err := c.http.PostForm(c.base+"/api/v2/torrents/delete", form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
