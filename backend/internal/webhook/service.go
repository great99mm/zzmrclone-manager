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
	StatusPending           = "pending"
	StatusRunning           = "running"
	StatusCopying           = "copying"
	StatusChecking          = "checking"
	StatusNotifyingCallback = "notifying_callback"
	StatusCallingCurlURL    = "calling_curl_url"
	StatusSuccess           = "success"
	StatusFailed            = "failed"
)

type CreateRequest struct {
	Path        string            `json:"path" binding:"required"`
	CallbackURL string            `json:"callback_url" binding:"required"`
	CurlURL     string            `json:"curl_url" binding:"required"`
	CurlHeaders map[string]string `json:"curl_headers"`
}

type Service struct {
	cfg        *config.Config
	db         *gorm.DB
	queue      chan string
	client     *http.Client
	jobTimeout time.Duration

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
	httpTimeout, err := parseDurationDefault(cfg.WebhookHTTPTimeout, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("invalid webhook http timeout: %w", err)
	}
	jobTimeout, err := parseDurationDefault(cfg.WebhookJobTimeout, 0)
	if err != nil {
		return nil, fmt.Errorf("invalid webhook job timeout: %w", err)
	}

	return &Service{
		cfg:        cfg,
		db:         db,
		queue:      make(chan string, queueSize),
		client:     &http.Client{Timeout: httpTimeout},
		jobTimeout: jobTimeout,
		active:     make(map[string]struct{}),
	}, nil
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
	return !s.cfg.WebhookAllowAnonymous && strings.TrimSpace(s.cfg.APIToken) != ""
}

func (s *Service) VerifyToken(token string) bool {
	if s.cfg.WebhookAllowAnonymous {
		return true
	}
	valid := strings.TrimSpace(s.cfg.APIToken)
	if valid == "" {
		return true
	}
	token = strings.TrimSpace(token)
	return subtle.ConstantTimeCompare([]byte(token), []byte(valid)) == 1
}

func (s *Service) CreateJob(ctx context.Context, req CreateRequest) (*models.WebhookJob, error) {
	if strings.TrimSpace(req.Path) == "" {
		return nil, errors.New("path is required")
	}
	if _, _, err := cleanRemotePath(req.Path); err != nil {
		return nil, err
	}
	if _, err := validateOutboundURL(req.CallbackURL, s.cfg.AllowedCallbackHosts); err != nil {
		return nil, fmt.Errorf("invalid callback_url: %w", err)
	}
	if _, err := validateOutboundURL(req.CurlURL, s.cfg.AllowedCurlHosts); err != nil {
		return nil, fmt.Errorf("invalid curl_url: %w", err)
	}
	curlHeaders, err := cleanHeaders(req.CurlHeaders)
	if err != nil {
		return nil, err
	}
	curlHeadersJSON, err := encodeHeaders(curlHeaders)
	if err != nil {
		return nil, err
	}

	jobID, err := newJobID()
	if err != nil {
		return nil, err
	}
	job := &models.WebhookJob{
		ID:          jobID,
		JobType:     "one_time",
		RemotePath:  req.Path,
		CallbackURL: req.CallbackURL,
		CurlURL:     req.CurlURL,
		CurlHeaders: curlHeadersJSON,
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
	if s.jobTimeout > 0 {
		jobCtx, cancel = context.WithTimeout(ctx, s.jobTimeout)
		defer cancel()
	}

	if err := s.setStatus(job.ID, StatusRunning); err != nil {
		return
	}
	cleanPath, relPath, err := cleanRemotePath(job.RemotePath)
	if err != nil {
		s.fail(job.ID, "validate path", err, job.RcloneLog)
		return
	}
	localPath, err := buildLocalPath(s.cfg.WebhookLocalBaseDir, relPath)
	if err != nil {
		s.fail(job.ID, "build local path", err, job.RcloneLog)
		return
	}
	remoteSpec, err := s.remoteSpec(cleanPath)
	if err != nil {
		s.fail(job.ID, "remote spec", err, job.RcloneLog)
		return
	}
	job.RemotePath = cleanPath
	job.LocalPath = localPath
	s.db.Model(job).Updates(map[string]interface{}{"remote_path": cleanPath, "local_path": localPath})

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

	if err := s.setStatus(job.ID, StatusCallingCurlURL); err != nil {
		return
	}
	curlHeaders, err := decodeHeaders(job.CurlHeaders)
	if err != nil {
		s.fail(job.ID, "curl_url headers", err, rcloneLog)
		return
	}
	if err := s.callCurlURL(jobCtx, job.CurlURL, curlHeaders); err != nil {
		if ctx.Err() != nil {
			return
		}
		s.fail(job.ID, "curl_url", err, rcloneLog)
		return
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
	return s.db.Model(&models.WebhookJob{}).Where("status IN ?", []string{StatusNotifyingCallback, StatusCallingCurlURL}).Updates(map[string]interface{}{
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

func (s *Service) remoteSpec(remotePath string) (string, error) {
	remote := strings.TrimSuffix(strings.TrimSpace(s.cfg.WebhookRcloneRemote), ":")
	if remote == "" {
		return "", errors.New("webhook rclone remote is not configured")
	}
	return remote + ":" + remotePath, nil
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
		"job_id":      job.ID,
		"status":      StatusSuccess,
		"remote_path": job.RemotePath,
		"local_path":  job.LocalPath,
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

func (s *Service) callCurlURL(ctx context.Context, raw string, headers map[string]string) error {
	curlURL, err := validateOutboundURL(raw, s.cfg.AllowedCurlHosts)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, curlURL.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "zzmrclone-manager/1.0")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return s.do2xx(req, "curl_url")
}

func cleanHeaders(headers map[string]string) (map[string]string, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	cleaned := make(map[string]string, len(headers))
	for key, value := range headers {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return nil, errors.New("curl_headers contains empty header name")
		}
		if strings.ContainsAny(key, "\r\n:") || strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("curl_headers contains invalid header %q", key)
		}
		cleaned[key] = value
	}
	return cleaned, nil
}

func encodeHeaders(headers map[string]string) (string, error) {
	if len(headers) == 0 {
		return "", nil
	}
	data, err := json.Marshal(headers)
	if err != nil {
		return "", fmt.Errorf("encode curl_headers: %w", err)
	}
	return string(data), nil
}

func decodeHeaders(encoded string) (map[string]string, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, nil
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(encoded), &headers); err != nil {
		return nil, fmt.Errorf("decode curl_headers: %w", err)
	}
	return cleanHeaders(headers)
}

func (s *Service) do2xx(req *http.Request, name string) error {
	resp, err := s.client.Do(req)
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

func buildLocalPath(baseDir, remoteRelativePath string) (string, error) {
	baseAbs, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	if evaluated, err := filepath.EvalSymlinks(baseAbs); err == nil {
		baseAbs = evaluated
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
	if err := rejectExistingSymlinkPath(baseAbs, targetAbs); err != nil {
		return "", err
	}
	return targetAbs, nil
}

func rejectExistingSymlinkPath(baseAbs, targetAbs string) error {
	rel, err := filepath.Rel(baseAbs, targetAbs)
	if err != nil || rel == "." {
		return err
	}
	current := baseAbs
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
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
