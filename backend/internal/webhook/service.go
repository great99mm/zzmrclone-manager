package webhook

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/config"
	"rclone-manager/internal/models"
)

const (
	StatusPending            = "pending"
	StatusRunning            = "running"
	StatusCopying            = "copying"
	StatusChecking           = "checking"
	StatusNotifyingCallback  = "notifying_callback"
	StatusNotifyingSmartStrm = "notifying_smartstrm"
	StatusSuccess            = "success"
	StatusFailed             = "failed"
)

type CreateRequest struct {
	Path        string `json:"path" binding:"required"`
	Tag         string `json:"tag" binding:"required"`
	CallbackURL string `json:"callback_url" binding:"required"`
}

type Service struct {
	cfg   *config.Config
	db    *gorm.DB
	queue chan string

	mu     sync.Mutex
	active map[string]struct{}
}

func NewService(cfg *config.Config, db *gorm.DB) (*Service, error) {
	if strings.TrimSpace(cfg.WebhookLocalBaseDir) == "" {
		return nil, errors.New("webhook local base dir is required")
	}
	if err := os.MkdirAll(cfg.WebhookLocalBaseDir, 0o755); err != nil {
		return nil, fmt.Errorf("create webhook local base dir: %w", err)
	}

	queueSize := cfg.WebhookQueueSize
	if queueSize <= 0 {
		queueSize = 100
	}
	return &Service{
		cfg:    cfg,
		db:     db,
		queue:  make(chan string, queueSize),
		active: make(map[string]struct{}),
	}, nil
}

func (s *Service) ReloadConfig() error {
	if strings.TrimSpace(s.cfg.WebhookLocalBaseDir) == "" {
		return errors.New("webhook local base dir is required")
	}
	if err := os.MkdirAll(s.cfg.WebhookLocalBaseDir, 0o755); err != nil {
		return fmt.Errorf("create webhook local base dir: %w", err)
	}
	if _, err := parseDurationDefault(s.cfg.WebhookHTTPTimeout, 30*time.Second); err != nil {
		return fmt.Errorf("invalid webhook http timeout: %w", err)
	}
	if _, err := parseDurationDefault(s.cfg.WebhookJobTimeout, 0); err != nil {
		return fmt.Errorf("invalid webhook job timeout: %w", err)
	}
	return nil
}

func (s *Service) Start(ctx context.Context) error {
	workers := s.cfg.WebhookWorkers
	if workers <= 0 {
		workers = 2
	}
	for i := 0; i < workers; i++ {
		go s.worker(ctx)
	}

	if err := s.failInterruptedNotificationJobs(); err != nil {
		return err
	}
	return s.recoverJobs(ctx)
}

func (s *Service) AuthEnabled() bool {
	return strings.TrimSpace(s.cfg.APIToken) != ""
}

func (s *Service) VerifyToken(token string) bool {
	valid := strings.TrimSpace(s.cfg.APIToken)
	if valid == "" {
		return false
	}
	token = strings.TrimSpace(token)
	return subtle.ConstantTimeCompare([]byte(token), []byte(valid)) == 1
}

func (s *Service) CreateJob(ctx context.Context, req CreateRequest) (*models.WebhookJob, error) {
	remoteName, err := cleanRemoteName(s.cfg.WebhookRcloneRemote)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Path) == "" {
		return nil, errors.New("path is required")
	}
	_, relPath, err := cleanRemotePath(req.Path)
	if err != nil {
		return nil, err
	}
	if _, err := remotePathLeaf(relPath); err != nil {
		return nil, err
	}
	tag, _, err := s.resolveTagDir(req.Tag)
	if err != nil {
		return nil, err
	}
	if _, err := validateOutboundURL(req.CallbackURL, s.cfg.AllowedCallbackHosts); err != nil {
		return nil, fmt.Errorf("invalid callback_url: %w", err)
	}

	jobID, err := newJobID()
	if err != nil {
		return nil, err
	}
	job := &models.WebhookJob{
		ID:          jobID,
		JobType:     "one_time",
		RemoteName:  remoteName,
		Tag:         tag,
		RemotePath:  req.Path,
		CallbackURL: req.CallbackURL,
		Status:      StatusPending,
	}
	if err := s.db.WithContext(ctx).Create(job).Error; err != nil {
		return nil, fmt.Errorf("create webhook job: %w", err)
	}
	if err := s.Enqueue(ctx, job.ID); err != nil {
		now := time.Now()
		s.db.Model(job).Updates(map[string]interface{}{
			"status":      StatusFailed,
			"error":       err.Error(),
			"finished_at": &now,
		})
		return nil, err
	}
	return job, nil
}

func (s *Service) Enqueue(ctx context.Context, jobID string) error {
	s.mu.Lock()
	if _, exists := s.active[jobID]; exists {
		s.mu.Unlock()
		return nil
	}
	s.active[jobID] = struct{}{}
	s.mu.Unlock()

	select {
	case s.queue <- jobID:
		return nil
	case <-ctx.Done():
		s.forget(jobID)
		return ctx.Err()
	}
}

func (s *Service) GetJob(ctx context.Context, id string) (*models.WebhookJob, error) {
	var job models.WebhookJob
	if err := s.db.WithContext(ctx).First(&job, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

func (s *Service) ListJobs(ctx context.Context, limit int) ([]models.WebhookJob, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	jobs := make([]models.WebhookJob, 0)
	if err := s.db.WithContext(ctx).Order("created_at desc").Limit(limit).Find(&jobs).Error; err != nil {
		return nil, err
	}
	return jobs, nil
}

func (s *Service) RetryJob(ctx context.Context, id string) (*models.WebhookJob, error) {
	var job models.WebhookJob
	if err := s.db.WithContext(ctx).First(&job, "id = ?", id).Error; err != nil {
		return nil, err
	}
	if job.Status != StatusFailed {
		return nil, errors.New("only failed jobs can be retried")
	}
	if err := s.db.WithContext(ctx).Model(&job).Updates(map[string]interface{}{
		"status":      StatusPending,
		"error":       "",
		"rclone_log":  "",
		"finished_at": nil,
	}).Error; err != nil {
		return nil, err
	}
	job.Status = StatusPending
	job.Error = ""
	job.RcloneLog = ""
	job.FinishedAt = nil
	return &job, s.Enqueue(ctx, job.ID)
}

func (s *Service) DeleteJob(ctx context.Context, id string) error {
	var job models.WebhookJob
	if err := s.db.WithContext(ctx).First(&job, "id = ?", id).Error; err != nil {
		return err
	}
	if job.Status != StatusSuccess && job.Status != StatusFailed {
		return errors.New("only success or failed jobs can be deleted")
	}
	return s.db.WithContext(ctx).Delete(&job).Error
}

func (s *Service) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case jobID := <-s.queue:
			s.process(ctx, jobID)
		}
	}
}

func (s *Service) process(ctx context.Context, jobID string) {
	defer s.forget(jobID)

	job, err := s.GetJob(ctx, jobID)
	if err != nil || job.Status == StatusSuccess {
		return
	}

	jobCtx := ctx
	var cancel context.CancelFunc
	jobTimeout, err := s.currentJobTimeout()
	if err != nil {
		s.fail(job.ID, "parse job timeout", err, job.RcloneLog)
		return
	}
	if jobTimeout > 0 {
		jobCtx, cancel = context.WithTimeout(ctx, jobTimeout)
		defer cancel()
	}

	if err := s.setStatus(job.ID, StatusRunning); err != nil {
		return
	}
	remoteName, err := cleanRemoteName(job.RemoteName)
	if err != nil {
		s.fail(job.ID, "validate remote", err, job.RcloneLog)
		return
	}
	cleanPath, relPath, err := cleanRemotePath(job.RemotePath)
	if err != nil {
		s.fail(job.ID, "validate path", err, job.RcloneLog)
		return
	}
	tag, tagDir, err := s.resolveTagDir(job.Tag)
	if err != nil {
		s.fail(job.ID, "resolve tag", err, job.RcloneLog)
		return
	}
	leafName, err := remotePathLeaf(relPath)
	if err != nil {
		s.fail(job.ID, "validate path", err, job.RcloneLog)
		return
	}
	localPath, err := buildLocalPath(tagDir, leafName)
	if err != nil {
		s.fail(job.ID, "build local path", err, job.RcloneLog)
		return
	}
	smartStrmPath := s.mapSmartStrmPath(localPath)
	remoteSpec := remoteSpec(remoteName, cleanPath)
	job.RemoteName = remoteName
	job.Tag = tag
	job.RemotePath = cleanPath
	job.LocalPath = localPath
	job.SmartStrmPath = smartStrmPath
	s.db.Model(job).Updates(map[string]interface{}{"remote_name": remoteName, "tag": tag, "remote_path": cleanPath, "local_path": localPath, "smartstrm_path": smartStrmPath})

	rcloneLog := job.RcloneLog
	if err := s.setStatus(job.ID, StatusCopying); err != nil {
		return
	}
	copyOutput, err := s.runRclone(jobCtx, "copy", remoteSpec, localPath)
	rcloneLog = appendLogSection(rcloneLog, "rclone copy", copyOutput, s.cfg.WebhookMaxRcloneLogSize)
	s.db.Model(job).Update("rclone_log", rcloneLog)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.fail(job.ID, "rclone copy", err, rcloneLog)
		return
	}

	if err := s.setStatus(job.ID, StatusChecking); err != nil {
		return
	}
	checkOutput, err := s.runRclone(jobCtx, "check", remoteSpec, localPath, "--size-only", "--one-way")
	rcloneLog = appendLogSection(rcloneLog, "rclone check", checkOutput, s.cfg.WebhookMaxRcloneLogSize)
	s.db.Model(job).Update("rclone_log", rcloneLog)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.fail(job.ID, "rclone check", err, rcloneLog)
		return
	}

	if err := s.setStatus(job.ID, StatusNotifyingCallback); err != nil {
		return
	}
	if err := s.postCallback(jobCtx, job); err != nil {
		if ctx.Err() != nil {
			return
		}
		s.fail(job.ID, "callback", err, rcloneLog)
		return
	}

	if strings.TrimSpace(s.cfg.SmartStrmWebhookURL) != "" {
		if err := s.setStatus(job.ID, StatusNotifyingSmartStrm); err != nil {
			return
		}
		if err := s.callSmartStrmWebhook(jobCtx, job); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.fail(job.ID, "smartstrm webhook", err, rcloneLog)
			return
		}
	}

	now := time.Now()
	s.db.Model(job).Updates(map[string]interface{}{
		"status":      StatusSuccess,
		"error":       "",
		"rclone_log":  rcloneLog,
		"finished_at": &now,
	})
}

func (s *Service) recoverJobs(ctx context.Context) error {
	var jobs []models.WebhookJob
	if err := s.db.WithContext(ctx).Where("status IN ?", []string{StatusPending, StatusRunning, StatusCopying, StatusChecking}).Order("created_at asc").Find(&jobs).Error; err != nil {
		return err
	}
	for _, job := range jobs {
		if err := s.Enqueue(ctx, job.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) failInterruptedNotificationJobs() error {
	now := time.Now()
	return s.db.Model(&models.WebhookJob{}).Where("status IN ?", []string{StatusNotifyingCallback, StatusNotifyingSmartStrm}).Updates(map[string]interface{}{
		"status":      StatusFailed,
		"error":       "service stopped during notification stage; retry manually if needed",
		"finished_at": &now,
	}).Error
}

func (s *Service) forget(jobID string) {
	s.mu.Lock()
	delete(s.active, jobID)
	s.mu.Unlock()
}

func (s *Service) setStatus(jobID, status string) error {
	return s.db.Model(&models.WebhookJob{}).Where("id = ?", jobID).Update("status", status).Error
}

func (s *Service) fail(jobID, step string, err error, rcloneLog string) {
	now := time.Now()
	s.db.Model(&models.WebhookJob{}).Where("id = ?", jobID).Updates(map[string]interface{}{
		"status":      StatusFailed,
		"error":       fmt.Sprintf("%s failed: %v", step, err),
		"rclone_log":  rcloneLog,
		"finished_at": &now,
	})
}

func remoteSpec(remoteName, remotePath string) string {
	return remoteName + ":" + remotePath
}

func (s *Service) mapSmartStrmPath(localPath string) string {
	cleanLocal := filepath.Clean(strings.TrimSpace(localPath))
	if cleanLocal == "." {
		return strings.TrimSpace(localPath)
	}
	bestFrom := ""
	bestTo := ""
	for _, mapping := range s.cfg.SmartStrmPathMappings {
		from := filepath.Clean(strings.TrimSpace(mapping.From))
		to := strings.TrimSpace(mapping.To)
		if from == "." || to == "" {
			continue
		}
		if cleanLocal == from || strings.HasPrefix(cleanLocal, from+string(os.PathSeparator)) {
			if len(from) > len(bestFrom) {
				bestFrom = from
				bestTo = to
			}
		}
	}
	if bestFrom == "" {
		return cleanLocal
	}
	rel := strings.TrimPrefix(cleanLocal, bestFrom)
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "/")
	base := strings.TrimRight(bestTo, "/")
	if bestTo == "/" {
		base = "/"
	}
	if rel == "" {
		return base
	}
	if base == "" || base == "/" {
		return "/" + rel
	}
	return base + "/" + rel
}

func (s *Service) runRclone(ctx context.Context, subcommand string, args ...string) (string, error) {
	baseArgs := []string{subcommand}
	baseArgs = append(baseArgs, args...)
	baseArgs = append(baseArgs,
		"--config", s.cfg.RcloneConfig,
		"--transfers", strconv.Itoa(maxInt(s.cfg.WebhookTransfers, 1)),
		"--checkers", strconv.Itoa(maxInt(s.cfg.WebhookCheckers, 1)),
		"--retries", strconv.Itoa(maxInt(s.cfg.WebhookRetries, 0)),
		"--low-level-retries", strconv.Itoa(maxInt(s.cfg.WebhookLowLevelRetries, 0)),
	)
	if strings.TrimSpace(s.cfg.WebhookBWLimit) != "" {
		baseArgs = append(baseArgs, "--bwlimit", s.cfg.WebhookBWLimit)
	}

	cmd := exec.CommandContext(ctx, s.cfg.WebhookRclonePath, baseArgs...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return truncateString(output.String(), s.cfg.WebhookMaxRcloneLogSize), err
	}
	return truncateString(output.String(), s.cfg.WebhookMaxRcloneLogSize), nil
}

func (s *Service) postCallback(ctx context.Context, job *models.WebhookJob) error {
	callbackURL, err := validateOutboundURL(job.CallbackURL, s.cfg.AllowedCallbackHosts)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{
		"job_id":         job.ID,
		"status":         StatusSuccess,
		"remote":         job.RemoteName,
		"tag":            job.Tag,
		"remote_path":    job.RemotePath,
		"local_path":     job.LocalPath,
		"smartstrm_path": job.SmartStrmPath,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callbackURL.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "zzmrclone-manager/1.0")
	return s.do2xx(req, "callback")
}

func (s *Service) callSmartStrmWebhook(ctx context.Context, job *models.WebhookJob) error {
	webhookURL, err := validateOutboundURL(s.cfg.SmartStrmWebhookURL, s.cfg.AllowedSmartStrmHosts)
	if err != nil {
		return err
	}
	taskName := strings.TrimSpace(s.cfg.SmartStrmTaskName)
	if taskName == "" {
		return errors.New("smartstrm task name is required")
	}
	storagePath := strings.TrimSpace(job.SmartStrmPath)
	if storagePath == "" {
		storagePath = s.mapSmartStrmPath(job.LocalPath)
	}
	body, err := json.Marshal(map[string]interface{}{
		"event": "a_task",
		"task": map[string]interface{}{
			"name":         taskName,
			"storage_path": storagePath,
		},
		"delay": 0,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "zzmrclone-manager/1.0")
	return s.do2xx(req, "smartstrm webhook")
}

func (s *Service) do2xx(req *http.Request, name string) error {
	httpClient, err := s.currentHTTPClient()
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s request failed: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s returned %s: %s", name, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func (s *Service) currentHTTPClient() (*http.Client, error) {
	httpTimeout, err := parseDurationDefault(s.cfg.WebhookHTTPTimeout, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("invalid webhook http timeout: %w", err)
	}
	return &http.Client{Timeout: httpTimeout}, nil
}

func (s *Service) currentJobTimeout() (time.Duration, error) {
	return parseDurationDefault(s.cfg.WebhookJobTimeout, 0)
}

func cleanRemotePath(raw string) (cleaned string, relative string, err error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", "", errors.New("path is required")
	}
	if strings.Contains(value, "\x00") {
		return "", "", errors.New("path contains NUL byte")
	}
	if strings.Contains(value, "\\") {
		return "", "", errors.New("path must use '/' separators")
	}
	if strings.Contains(value, "..") {
		return "", "", errors.New("path must not contain '..'")
	}
	cleaned = path.Clean("/" + strings.TrimPrefix(value, "/"))
	relative = strings.TrimPrefix(cleaned, "/")
	return cleaned, relative, nil
}

func remotePathLeaf(relative string) (string, error) {
	leafName := path.Base(relative)
	if leafName == "" || leafName == "." || leafName == "/" {
		return "", errors.New("path must include a final file or directory name")
	}
	return leafName, nil
}

func cleanRemoteName(raw string) (string, error) {
	value := strings.TrimSuffix(strings.TrimSpace(raw), ":")
	if value == "" {
		return "", errors.New("remote is required")
	}
	if strings.ContainsAny(value, "/\\\x00\r\n") || strings.Contains(value, "..") {
		return "", errors.New("remote contains invalid characters")
	}
	return value, nil
}

func (s *Service) resolveTagDir(raw string) (string, string, error) {
	tag := strings.TrimSpace(raw)
	if tag == "" {
		return "", "", errors.New("tag is required")
	}
	dir, ok := s.cfg.WebhookTagDirs[tag]
	if !ok || strings.TrimSpace(dir) == "" {
		return "", "", fmt.Errorf("tag %q is not configured", tag)
	}
	dir = strings.TrimSpace(dir)
	if strings.ContainsAny(dir, "\x00\r\n") {
		return "", "", fmt.Errorf("tag %q directory contains invalid characters", tag)
	}
	if !filepath.IsAbs(dir) {
		return "", "", fmt.Errorf("tag %q directory must be absolute", tag)
	}
	dir, err := ensureDirectoryNoSymlink(dir)
	if err != nil {
		return "", "", err
	}
	return tag, dir, nil
}

func buildLocalPath(baseDir, remoteRelativePath string) (string, error) {
	baseAbs, err := ensureDirectoryNoSymlink(baseDir)
	if err != nil {
		return "", err
	}
	targetAbs, err := filepath.Abs(filepath.Join(baseAbs, filepath.FromSlash(remoteRelativePath)))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(baseAbs, targetAbs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("local path escapes local base dir")
	}
	if err := rejectExistingSymlinkComponents(targetAbs); err != nil {
		return "", err
	}
	return targetAbs, nil
}

func ensureDirectoryNoSymlink(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := rejectExistingSymlinkComponents(abs); err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	if err := rejectExistingSymlinkComponents(abs); err != nil {
		return "", err
	}
	return abs, nil
}

func rejectExistingSymlinkComponents(absPath string) error {
	absPath = filepath.Clean(absPath)
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

func validateOutboundURL(raw string, allowedHosts []string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("url scheme must be http or https")
	}
	if parsed.Hostname() == "" {
		return nil, errors.New("url host is required")
	}
	if !hostAllowed(parsed.Hostname(), allowedHosts) {
		return nil, fmt.Errorf("url host %q is not allowed", parsed.Hostname())
	}
	return parsed, nil
}

func ValidateOutboundURLForConfig(raw string, allowedHosts []string) (*url.URL, error) {
	return validateOutboundURL(raw, allowedHosts)
}

func hostAllowed(host string, allowedHosts []string) bool {
	if len(allowedHosts) == 0 {
		return true
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, allowed := range allowedHosts {
		pattern := normalizeAllowedHost(allowed)
		if pattern == "*" {
			return true
		}
		if strings.HasPrefix(pattern, "*.") {
			suffix := strings.TrimPrefix(pattern, "*.")
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == pattern {
			return true
		}
	}
	return false
}

func normalizeAllowedHost(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if strings.Contains(value, "://") {
		if parsed, err := url.Parse(value); err == nil {
			value = parsed.Hostname()
		}
	} else if !strings.HasPrefix(value, "*.") {
		if host, _, err := net.SplitHostPort(value); err == nil {
			value = host
		}
	}
	return strings.TrimSuffix(value, ".")
}

func appendLogSection(existing, title, output string, maxBytes int) string {
	if strings.TrimSpace(output) == "" {
		return truncateString(existing, maxBytes)
	}
	section := fmt.Sprintf("\n===== %s %s =====\n%s", time.Now().UTC().Format(time.RFC3339), title, output)
	return truncateString(existing+section, maxBytes)
}

func truncateString(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	marker := "\n...[truncated]...\n"
	if maxBytes <= len(marker) {
		return value[len(value)-maxBytes:]
	}
	keep := maxBytes - len(marker)
	return marker + value[len(value)-keep:]
}

func parseDurationDefault(value string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	return time.ParseDuration(value)
}

func newJobID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return "job_" + hex.EncodeToString(data[:]), nil
}

func maxInt(value, fallback int) int {
	if value < fallback {
		return fallback
	}
	return value
}
