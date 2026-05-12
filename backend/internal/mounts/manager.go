package mounts

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/logger"
	"rclone-manager/internal/models"
)

type runtimeMount struct {
	cmd  *exec.Cmd
	done chan error
}

type SupportInfo struct {
	Supported            bool     `json:"supported"`
	FuseDevice           bool     `json:"fuse_device"`
	Fusermount           bool     `json:"fusermount"`
	RcloneBinary         bool     `json:"rclone_binary"`
	AllowOtherConfigured bool     `json:"allow_other_configured"`
	MountRoot            string   `json:"mount_root"`
	Notes                []string `json:"notes"`
}

type Manager struct {
	db        *gorm.DB
	mountRoot string
	dataDir   string

	mu      sync.RWMutex
	running map[uint]*runtimeMount
}

func NewManager(db *gorm.DB, mountRoot string, dataDir string) *Manager {
	mountRoot = strings.TrimSpace(mountRoot)
	return &Manager{
		db:        db,
		mountRoot: normalizeOptionalMountRoot(mountRoot),
		dataDir:   dataDir,
		running:   make(map[uint]*runtimeMount),
	}
}

func (m *Manager) RestoreAndStartEnabled() {
	if m.db == nil {
		return
	}

	if m.mountRoot != "" {
		_ = os.MkdirAll(m.mountRoot, 0755)
	}
	m.updateFieldsByQuery(m.db.Model(&models.MountConfig{}), map[string]interface{}{
		"status": "stopped",
	}, "status IN ?", []string{"starting", "mounted", "stopping"})

	var configs []models.MountConfig
	if err := m.db.Where("enabled = ?", true).Order("id asc").Find(&configs).Error; err != nil {
		return
	}

	for i := range configs {
		if err := m.Start(&configs[i]); err != nil {
			logger.WriteLog("mounts.log", fmt.Sprintf("自动挂载 [%s] 失败: %v", configs[i].Name, err))
		}
	}
}

func (m *Manager) SupportInfo() SupportInfo {
	info := SupportInfo{
		MountRoot: m.mountRoot,
	}

	if _, err := os.Stat("/dev/fuse"); err == nil {
		info.FuseDevice = true
	} else {
		info.Notes = append(info.Notes, "未检测到 /dev/fuse")
	}

	if _, err := exec.LookPath("rclone"); err == nil {
		info.RcloneBinary = true
	} else {
		info.Notes = append(info.Notes, "未找到 rclone 可执行文件")
	}

	if _, err := exec.LookPath("fusermount3"); err == nil {
		info.Fusermount = true
	} else if _, err := exec.LookPath("fusermount"); err == nil {
		info.Fusermount = true
	} else {
		info.Notes = append(info.Notes, "未找到 fusermount/fusermount3，无法卸载挂载点")
	}

	if content, err := os.ReadFile("/etc/fuse.conf"); err == nil {
		for _, line := range strings.Split(string(content), "\n") {
			line = strings.TrimSpace(line)
			if line == "user_allow_other" {
				info.AllowOtherConfigured = true
				break
			}
		}
	}
	if !info.AllowOtherConfigured {
		info.Notes = append(info.Notes, "未启用 user_allow_other，allow_other 可能不可用")
	}

	if m.mountRoot == "" {
		info.Notes = append(info.Notes, "未配置默认挂载根目录")
	} else {
		info.Notes = append(info.Notes, fmt.Sprintf("当前限制挂载目录位于 %s 之下", m.mountRoot))
	}
	info.Notes = append(info.Notes, "宿主机可见挂载需配置 bind mount 与 rshared 传播")
	info.Supported = info.FuseDevice && info.RcloneBinary && info.Fusermount
	return info
}

func (m *Manager) ApplyRuntimeStatus(cfg *models.MountConfig) {
	if cfg == nil {
		return
	}
	if m.IsProcessRunning(cfg.ID) || isMountActive(cfg.MountPath) {
		cfg.Status = "mounted"
		cfg.LastError = ""
		return
	}
	if cfg.Status == "starting" || cfg.Status == "stopping" || cfg.Status == "error" {
		return
	}
	cfg.Status = "stopped"
}

func (m *Manager) NormalizeAndValidate(cfg *models.MountConfig, currentID uint) error {
	if cfg == nil {
		return fmt.Errorf("挂载配置不能为空")
	}

	cfg.Name = strings.TrimSpace(cfg.Name)
	cfg.RemoteName = strings.TrimSpace(cfg.RemoteName)
	cfg.RemotePath = normalizeRemotePath(cfg.RemotePath)
	cfg.RcloneConfig = strings.TrimSpace(cfg.RcloneConfig)
	cfg.DirCacheTime = strings.TrimSpace(cfg.DirCacheTime)
	cfg.PollInterval = strings.TrimSpace(cfg.PollInterval)
	cfg.VFSCacheMode = strings.ToLower(strings.TrimSpace(cfg.VFSCacheMode))
	cfg.ExtraArgs = strings.TrimSpace(cfg.ExtraArgs)

	if cfg.Name == "" {
		return fmt.Errorf("挂载名称不能为空")
	}
	if cfg.RemoteName == "" {
		return fmt.Errorf("远程盘符不能为空")
	}
	if cfg.VFSCacheMode == "" {
		cfg.VFSCacheMode = "writes"
	}
	if cfg.DirCacheTime == "" {
		cfg.DirCacheTime = "5m"
	}
	if cfg.PollInterval == "" {
		cfg.PollInterval = "1m"
	}
	if cfg.UID < 0 || cfg.GID < 0 {
		return fmt.Errorf("UID/GID 不能为负数")
	}

	if strings.TrimSpace(cfg.MountPath) == "" && m.mountRoot != "" {
		cfg.MountPath = filepath.Join(m.mountRoot, sanitizeSegment(cfg.Name))
	}
	cfg.MountPath = m.normalizeMountPath(cfg.MountPath)
	if strings.TrimSpace(cfg.MountPath) == "" {
		return fmt.Errorf("挂载目录不能为空，请填写完整容器内路径")
	}
	if m.mountRoot == "" && !filepath.IsAbs(cfg.MountPath) {
		return fmt.Errorf("未设置默认挂载根目录时，挂载目录必须填写绝对路径")
	}

	allowedModes := map[string]bool{
		"off":     true,
		"minimal": true,
		"writes":  true,
		"full":    true,
	}
	if !allowedModes[cfg.VFSCacheMode] {
		return fmt.Errorf("缓存模式仅支持 off/minimal/writes/full")
	}

	if err := m.ensureWithinMountRoot(cfg.MountPath); err != nil {
		return err
	}
	if m.mountRoot != "" && samePath(cfg.MountPath, m.mountRoot) {
		return fmt.Errorf("挂载目录不能直接使用根目录 %s", m.mountRoot)
	}

	if m.db != nil {
		var count int64
		query := m.db.Model(&models.MountConfig{}).Where("mount_path = ?", cfg.MountPath)
		if currentID != 0 {
			query = query.Where("id <> ?", currentID)
		}
		if err := query.Count(&count).Error; err == nil && count > 0 {
			return fmt.Errorf("挂载目录已被其他挂载配置占用")
		}
	}

	return nil
}

func (m *Manager) Start(cfg *models.MountConfig) error {
	if cfg == nil {
		return fmt.Errorf("挂载配置不存在")
	}
	if err := m.NormalizeAndValidate(cfg, cfg.ID); err != nil {
		m.updateError(cfg.ID, err)
		return err
	}

	info := m.SupportInfo()
	if !info.Supported {
		err := fmt.Errorf(strings.Join(info.Notes, "；"))
		m.updateError(cfg.ID, err)
		return err
	}

	if m.IsProcessRunning(cfg.ID) || isMountActive(cfg.MountPath) {
		now := time.Now()
		m.updateFields(cfg.ID, map[string]interface{}{
			"status":          "mounted",
			"last_error":      "",
			"last_mounted_at": &now,
		})
		return nil
	}

	if err := os.MkdirAll(cfg.MountPath, 0755); err != nil {
		m.updateError(cfg.ID, err)
		return fmt.Errorf("创建挂载目录失败: %w", err)
	}

	empty, err := dirIsEmpty(cfg.MountPath)
	if err != nil {
		m.updateError(cfg.ID, err)
		return fmt.Errorf("检查挂载目录失败: %w", err)
	}
	if !empty {
		err = fmt.Errorf("挂载目录必须为空: %s", cfg.MountPath)
		m.updateError(cfg.ID, err)
		return err
	}

	cacheDir := filepath.Join(m.dataDir, "mount-cache", fmt.Sprintf("mount_%d", cfg.ID))
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		m.updateError(cfg.ID, err)
		return fmt.Errorf("创建缓存目录失败: %w", err)
	}

	logFilePath := filepath.Join(logger.GetLogDir(), fmt.Sprintf("mount_%d.log", cfg.ID))
	logFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		m.updateError(cfg.ID, err)
		return fmt.Errorf("打开挂载日志失败: %w", err)
	}

	args := m.buildArgs(cfg, cacheDir)
	cmd := exec.Command("rclone", args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	m.updateFields(cfg.ID, map[string]interface{}{
		"status":     "starting",
		"last_error": "",
	})

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		m.updateError(cfg.ID, err)
		return fmt.Errorf("启动挂载失败: %w", err)
	}

	state := &runtimeMount{cmd: cmd, done: make(chan error, 1)}
	m.mu.Lock()
	m.running[cfg.ID] = state
	m.mu.Unlock()

	logger.WriteLog("mounts.log", fmt.Sprintf("开始挂载 [%s] %s -> %s", cfg.Name, buildRemote(cfg), cfg.MountPath))
	go m.waitForExit(cfg.ID, cfg.Name, cfg.MountPath, logFile, state)

	if err := waitUntilMounted(cfg.MountPath, state.done, 12*time.Second); err != nil {
		_ = m.Stop(cfg.ID)
		wrapped := fmt.Errorf("挂载未就绪: %w", err)
		m.updateError(cfg.ID, wrapped)
		return wrapped
	}

	now := time.Now()
	m.updateFields(cfg.ID, map[string]interface{}{
		"status":          "mounted",
		"last_error":      "",
		"last_mounted_at": &now,
	})
	return nil
}

func (m *Manager) Stop(id uint) error {
	if id == 0 {
		return fmt.Errorf("挂载 ID 无效")
	}

	var cfg models.MountConfig
	if m.db != nil {
		if err := m.db.First(&cfg, id).Error; err != nil {
			return err
		}
	}

	m.updateFields(id, map[string]interface{}{
		"status":     "stopping",
		"last_error": "",
	})

	if isMountActive(cfg.MountPath) {
		if err := unmountPath(cfg.MountPath); err != nil {
			m.updateError(id, err)
			return err
		}
		if err := waitUntilUnmounted(cfg.MountPath, 8*time.Second); err != nil {
			m.updateError(id, err)
			return err
		}
	}

	m.mu.RLock()
	state := m.running[id]
	m.mu.RUnlock()
	if state != nil && state.cmd != nil && state.cmd.Process != nil && state.cmd.ProcessState == nil {
		_ = state.cmd.Process.Kill()
	}

	m.updateFields(id, map[string]interface{}{
		"status":     "stopped",
		"last_error": "",
	})
	logger.WriteLog("mounts.log", fmt.Sprintf("挂载 [%s] 已卸载", cfg.Name))
	return nil
}

func (m *Manager) IsProcessRunning(id uint) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	state := m.running[id]
	if state == nil || state.cmd == nil || state.cmd.Process == nil {
		return false
	}
	return state.cmd.ProcessState == nil
}

func (m *Manager) waitForExit(id uint, name string, mountPath string, logFile *os.File, state *runtimeMount) {
	err := state.cmd.Wait()
	_ = logFile.Close()

	m.mu.Lock()
	if current, ok := m.running[id]; ok && current == state {
		delete(m.running, id)
	}
	m.mu.Unlock()

	state.done <- err
	close(state.done)

	if m.db == nil {
		return
	}

	var cfg models.MountConfig
	if m.db.First(&cfg, id).Error != nil {
		return
	}

	if cfg.Status == "stopping" || cfg.Status == "stopped" {
		m.updateFields(id, map[string]interface{}{
			"status":     "stopped",
			"last_error": "",
		})
		return
	}

	if isMountActive(mountPath) {
		now := time.Now()
		m.updateFields(id, map[string]interface{}{
			"status":          "mounted",
			"last_error":      "",
			"last_mounted_at": &now,
		})
		return
	}

	if err != nil {
		m.updateFields(id, map[string]interface{}{
			"status":     "error",
			"last_error": err.Error(),
		})
		logger.WriteLog("mounts.log", fmt.Sprintf("挂载 [%s] 异常退出: %v", name, err))
		return
	}

	m.updateFields(id, map[string]interface{}{
		"status":     "stopped",
		"last_error": "",
	})
}

func (m *Manager) buildArgs(cfg *models.MountConfig, cacheDir string) []string {
	args := []string{
		"mount",
		buildRemote(cfg),
		cfg.MountPath,
		"--config", m.rcloneConfig(cfg),
		"--cache-dir", cacheDir,
		"--vfs-cache-mode", cfg.VFSCacheMode,
		"--dir-cache-time", cfg.DirCacheTime,
		"--poll-interval", cfg.PollInterval,
		"--uid", strconv.Itoa(cfg.UID),
		"--gid", strconv.Itoa(cfg.GID),
		"--umask", "022",
		"--attr-timeout", "1s",
		"--log-level", "INFO",
	}
	if cfg.AllowOther {
		args = append(args, "--allow-other")
	}
	if cfg.ReadOnly {
		args = append(args, "--read-only")
	}
	if cfg.ExtraArgs != "" {
		args = append(args, strings.Fields(cfg.ExtraArgs)...)
	}
	return args
}

func (m *Manager) rcloneConfig(cfg *models.MountConfig) string {
	if cfg != nil && strings.TrimSpace(cfg.RcloneConfig) != "" {
		return strings.TrimSpace(cfg.RcloneConfig)
	}
	return "/root/.config/rclone/rclone.conf"
}

func (m *Manager) normalizeMountPath(input string) string {
	path := strings.TrimSpace(input)
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		if m.mountRoot == "" {
			return path
		}
		path = filepath.Join(m.mountRoot, path)
	}
	return filepath.Clean(path)
}

func (m *Manager) ensureWithinMountRoot(target string) error {
	if m.mountRoot == "" {
		return nil
	}
	rootAbs, err := filepath.Abs(m.mountRoot)
	if err != nil {
		return fmt.Errorf("解析挂载根目录失败: %w", err)
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("解析挂载目录失败: %w", err)
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("挂载目录必须位于 %s 之下", m.mountRoot)
	}
	return nil
}

func normalizeOptionalMountRoot(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

func (m *Manager) updateFields(id uint, updates map[string]interface{}) {
	if m.db == nil || id == 0 {
		return
	}
	_ = m.db.Model(&models.MountConfig{}).Where("id = ?", id).Updates(updates).Error
}

func (m *Manager) updateFieldsByQuery(query *gorm.DB, updates map[string]interface{}, cond string, args ...interface{}) {
	if query == nil {
		return
	}
	_ = query.Where(cond, args...).Updates(updates).Error
}

func (m *Manager) updateError(id uint, err error) {
	if err == nil {
		return
	}
	m.updateFields(id, map[string]interface{}{
		"status":     "error",
		"last_error": err.Error(),
	})
	logger.WriteLog("mounts.log", err.Error())
}

func buildRemote(cfg *models.MountConfig) string {
	if cfg == nil {
		return ""
	}
	return fmt.Sprintf("%s:%s", strings.TrimSpace(cfg.RemoteName), normalizeRemotePath(cfg.RemotePath))
}

func normalizeRemotePath(input string) string {
	cleaned := pathpkg.Clean("/" + strings.TrimSpace(input))
	if cleaned == "." || cleaned == "" {
		return "/"
	}
	return cleaned
}

func sanitizeSegment(input string) string {
	input = strings.TrimSpace(strings.ToLower(input))
	if input == "" {
		return "mount"
	}
	var b strings.Builder
	lastDash := false
	for _, r := range input {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '-', r == '_':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	result := strings.Trim(b.String(), "-_")
	if result == "" {
		return "mount"
	}
	return result
}

func dirIsEmpty(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	_, err = f.Readdirnames(1)
	if err == io.EOF {
		return true, nil
	}
	return false, err
}

func samePath(a string, b string) bool {
	aAbs, errA := filepath.Abs(a)
	bAbs, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return filepath.Clean(aAbs) == filepath.Clean(bAbs)
}

func isMountActive(target string) bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	target = filepath.Clean(target)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mountPoint := unescapeMountField(fields[4])
		if filepath.Clean(mountPoint) == target {
			return true
		}
	}
	return false
}

func unescapeMountField(s string) string {
	replacer := strings.NewReplacer(
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
		`\134`, `\\`,
	)
	return replacer.Replace(s)
}

func waitUntilMounted(mountPath string, done <-chan error, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isMountActive(mountPath) {
			return nil
		}
		select {
		case err, ok := <-done:
			if !ok {
				return fmt.Errorf("挂载进程提前退出")
			}
			if err == nil && isMountActive(mountPath) {
				return nil
			}
			if err == nil {
				return fmt.Errorf("挂载进程提前退出")
			}
			return err
		default:
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("等待挂载超时")
}

func waitUntilUnmounted(mountPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !isMountActive(mountPath) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("等待卸载超时")
}

func unmountPath(mountPath string) error {
	if !isMountActive(mountPath) {
		return nil
	}

	cmdCandidates := [][]string{}
	if path, err := exec.LookPath("fusermount3"); err == nil {
		cmdCandidates = append(cmdCandidates, []string{path, "-uz", mountPath})
	}
	if path, err := exec.LookPath("fusermount"); err == nil {
		cmdCandidates = append(cmdCandidates, []string{path, "-uz", mountPath})
	}
	if path, err := exec.LookPath("umount"); err == nil {
		cmdCandidates = append(cmdCandidates, []string{path, "-l", mountPath})
	}

	var lastErr error
	for _, candidate := range cmdCandidates {
		cmd := exec.Command(candidate[0], candidate[1:]...)
		if output, err := cmd.CombinedOutput(); err == nil {
			return nil
		} else {
			trimmed := strings.TrimSpace(string(output))
			if trimmed != "" {
				lastErr = fmt.Errorf("%s: %s", err.Error(), trimmed)
			} else {
				lastErr = err
			}
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的卸载命令")
	}
	return fmt.Errorf("卸载挂载点失败: %w", lastErr)
}
