package proactive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
)

type CopySpec struct {
	ConfigPath        string
	ManifestPath      string
	SourceRoot        *os.File
	DestinationRemote string
	DestinationPath   string
	Transfers         int
}

type MoveSpec struct {
	ConfigPath        string
	ManifestPath      string
	SourceRoot        *os.File
	DestinationRemote string
	DestinationPath   string
	Transfers         int
}

type DedupeSpec struct {
	ConfigPath      string
	Remote          string
	DestinationPath string
}

type RemoteObject struct {
	Path  string
	Size  int64
	IsDir bool
}
type ProcessResult struct {
	ExitCode          int
	Stdout            string
	Stderr            string
	PID               int
	ProcessStartToken string
}

type ProcessHandle interface {
	Wait() (ProcessResult, error)
	Stop() error
	PID() int
	StartToken() string
}

type CommandRunner interface {
	StartCopy(context.Context, CopySpec) (ProcessHandle, error)
	StatRemote(context.Context, string, string, string, string) (RemoteObject, error)
}

type MoveRunner interface {
	StartMove(context.Context, MoveSpec) (ProcessHandle, error)
}

type DedupeRunner interface {
	StartDedupe(context.Context, DedupeSpec) (ProcessHandle, error)
}

type ExecRunner struct {
	Binary            string
	ProcessStartToken func(int) string
}

type StartedProcessIdentityError struct {
	PID     int
	Cause   error
	Result  ProcessResult
	WaitErr error
}

func (e *StartedProcessIdentityError) Error() string {
	return fmt.Sprintf("process started but identity unavailable (pid=%d): %v", e.PID, e.Cause)
}
func (e *StartedProcessIdentityError) Unwrap() error { return e.Cause }

func (r ExecRunner) StartCopy(ctx context.Context, spec CopySpec) (ProcessHandle, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("proactive copy runner requires Linux descriptor-relative source")
	}
	if spec.SourceRoot == nil || spec.ConfigPath == "" || spec.ManifestPath == "" || spec.DestinationRemote == "" {
		return nil, errors.New("invalid copy specification")
	}
	destination := spec.DestinationRemote + ":" + canonicalRemotePath(spec.DestinationPath)
	binary := r.Binary
	if binary == "" {
		binary = "rclone"
	}
	cmd := exec.CommandContext(ctx, binary, "--config", spec.ConfigPath, "copy", "--transfers", strconv.Itoa(normalizeTransfers(spec.Transfers)), "--files-from-raw", spec.ManifestPath, "--no-traverse", "--drive-stop-on-upload-limit", "--stats-log-level", "INFO", "/proc/self/fd/3", destination)
	cmd.ExtraFiles = []*os.File{spec.SourceRoot}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	readToken := r.ProcessStartToken
	if readToken == nil {
		readToken = processStartToken
	}
	token := readToken(cmd.Process.Pid)
	if cmd.Process.Pid <= 0 || token == "" {
		_ = cmd.Process.Kill()
		waitErr := error(nil)
		waitErr = cmd.Wait()
		result := ProcessResult{PID: cmd.Process.Pid, ProcessStartToken: token, Stdout: stdout.String(), Stderr: stderr.String()}
		if cmd.ProcessState != nil {
			result.ExitCode = cmd.ProcessState.ExitCode()
		}
		return nil, &StartedProcessIdentityError{PID: cmd.Process.Pid, Cause: errors.New("unable to read rclone process identity"), Result: result, WaitErr: waitErr}
	}
	return &execProcess{cmd: cmd, stdout: &stdout, stderr: &stderr, token: token}, nil
}

func (r ExecRunner) StartMove(ctx context.Context, spec MoveSpec) (ProcessHandle, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("proactive move runner requires Linux descriptor-relative source")
	}
	if spec.SourceRoot == nil || spec.ConfigPath == "" || spec.ManifestPath == "" || spec.DestinationRemote == "" {
		return nil, errors.New("invalid move specification")
	}
	destination := spec.DestinationRemote + ":" + canonicalRemotePath(spec.DestinationPath)
	binary := r.Binary
	if binary == "" {
		binary = "rclone"
	}
	cmd := exec.CommandContext(ctx, binary, "--config", spec.ConfigPath, "move", "--transfers", strconv.Itoa(normalizeTransfers(spec.Transfers)), "--files-from-raw", spec.ManifestPath, "--no-traverse", "--drive-stop-on-upload-limit", "--stats-log-level", "INFO", "/proc/self/fd/3", destination)
	cmd.ExtraFiles = []*os.File{spec.SourceRoot}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	readToken := r.ProcessStartToken
	if readToken == nil {
		readToken = processStartToken
	}
	token := readToken(cmd.Process.Pid)
	if cmd.Process.Pid <= 0 || token == "" {
		_ = cmd.Process.Kill()
		waitErr := cmd.Wait()
		result := ProcessResult{PID: cmd.Process.Pid, ProcessStartToken: token, Stdout: stdout.String(), Stderr: stderr.String()}
		if cmd.ProcessState != nil {
			result.ExitCode = cmd.ProcessState.ExitCode()
		}
		return nil, &StartedProcessIdentityError{PID: cmd.Process.Pid, Cause: errors.New("unable to read rclone process identity"), Result: result, WaitErr: waitErr}
	}
	return &execProcess{cmd: cmd, stdout: &stdout, stderr: &stderr, token: token}, nil
}

func normalizeTransfers(value int) int {
	if value <= 0 {
		return 4
	}
	return value
}

func (r ExecRunner) StartDedupe(ctx context.Context, spec DedupeSpec) (ProcessHandle, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("proactive dedupe runner requires Linux")
	}
	if spec.ConfigPath == "" || spec.Remote == "" {
		return nil, errors.New("invalid dedupe specification")
	}
	if info, err := os.Stat(spec.ConfigPath); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("dedupe config preflight failed")
	}
	binary := r.Binary
	if binary == "" {
		binary = "rclone"
	}
	target := spec.Remote + ":" + canonicalRemotePath(spec.DestinationPath)
	cmd := exec.CommandContext(ctx, binary, "--config", spec.ConfigPath, "dedupe", target, "--dedupe-mode", "newest", "-v")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	readToken := r.ProcessStartToken
	if readToken == nil {
		readToken = processStartToken
	}
	token := readToken(cmd.Process.Pid)
	if cmd.Process.Pid <= 0 || token == "" {
		_ = cmd.Process.Kill()
		waitErr := cmd.Wait()
		result := ProcessResult{PID: cmd.Process.Pid, ProcessStartToken: token, Stdout: stdout.String(), Stderr: stderr.String()}
		if cmd.ProcessState != nil {
			result.ExitCode = cmd.ProcessState.ExitCode()
		}
		return nil, &StartedProcessIdentityError{PID: cmd.Process.Pid, Cause: errors.New("unable to read dedupe process identity"), Result: result, WaitErr: waitErr}
	}
	return &execProcess{cmd: cmd, stdout: &stdout, stderr: &stderr, token: token}, nil
}

func (r ExecRunner) StatRemote(ctx context.Context, configPath, remote, destination, relative string) (RemoteObject, error) {
	binary := r.Binary
	if binary == "" {
		binary = "rclone"
	}
	target := remote + ":" + canonicalRemotePath(destination) + "/" + canonicalRemotePath(relative)
	output, err := exec.CommandContext(ctx, binary, "lsjson", "--stat", "--config", configPath, target).Output()
	if err != nil {
		return RemoteObject{}, err
	}
	var object struct {
		Path  string `json:"Path"`
		Size  int64  `json:"Size"`
		IsDir bool   `json:"IsDir"`
	}
	if err := json.Unmarshal(output, &object); err != nil {
		return RemoteObject{}, err
	}
	requested := canonicalRemotePath(relative)
	if object.IsDir || object.Path == "" || strings.ContainsAny(object.Path, "/\\") || path.Base(requested) != object.Path {
		return RemoteObject{}, fmt.Errorf("remote stat did not confirm requested object %q", requested)
	}
	return RemoteObject{Path: requested, Size: object.Size, IsDir: false}, nil
}

type execProcess struct {
	cmd            *exec.Cmd
	stdout, stderr *bytes.Buffer
	token          string
}

func (p *execProcess) PID() int           { return p.cmd.Process.Pid }
func (p *execProcess) StartToken() string { return p.token }
func (p *execProcess) Wait() (ProcessResult, error) {
	err := p.cmd.Wait()
	result := ProcessResult{PID: p.cmd.Process.Pid, ProcessStartToken: p.token, Stdout: p.stdout.String(), Stderr: p.stderr.String()}
	if p.cmd.ProcessState != nil {
		result.ExitCode = p.cmd.ProcessState.ExitCode()
	}
	return result, err
}
func (p *execProcess) Stop() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

func processStartToken(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	text := string(data)
	end := strings.LastIndex(text, ") ")
	if end < 0 {
		return ""
	}
	fields := strings.Fields(text[end+2:])
	if len(fields) <= 19 {
		return ""
	}
	return strconv.Itoa(pid) + ":" + fields[19]
}

func canonicalRemotePath(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	value = path.Clean("/" + value)
	return strings.TrimPrefix(value, "/")
}

type UploadLimitMarker struct {
	Detected bool
	Text     string
}

func DetectUploadLimit(output string) UploadLimitMarker {
	lower := strings.ToLower(output)
	for _, marker := range []string{"upload limit", "uploadlimit", "drive upload limit", "rate limit exceeded"} {
		if strings.Contains(lower, marker) {
			return UploadLimitMarker{Detected: true, Text: marker}
		}
	}
	return UploadLimitMarker{}
}
