package proactive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"

	"rclone-manager/internal/models"
)

type ProbeUploadSpec struct {
	ConfigPath    string
	Remote        string
	ObjectPath    string
	ExpectedBytes int64
}

type ProbeObjectResult struct {
	Exists      bool
	Exact       bool
	ObjectCount int
	Object      RemoteObject
	Evidence    string
}

type ProbeCleanupResult struct {
	Absent   bool
	Evidence string
}

type ProbeRunner interface {
	StartProbeUpload(context.Context, ProbeUploadSpec) (ProcessHandle, error)
	VerifyProbeObject(context.Context, string, string, string, int64) (ProbeObjectResult, error)
	DeleteProbeObject(context.Context, string, string, string) (ProbeCleanupResult, error)
}

func (r ExecRunner) StartProbeUpload(ctx context.Context, spec ProbeUploadSpec) (ProcessHandle, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("quota probe runner requires Linux")
	}
	if spec.ConfigPath == "" || spec.Remote == "" || spec.ObjectPath == "" || spec.ExpectedBytes != models.ProbeExpectedBytes {
		return nil, errors.New("invalid quota probe upload specification")
	}
	binary := r.Binary
	if binary == "" {
		binary = "rclone"
	}
	target := spec.Remote + ":" + canonicalRemotePath(spec.ObjectPath)
	cmd := exec.CommandContext(ctx, binary, "--config", spec.ConfigPath, "rcat", "--size="+strconv.FormatInt(spec.ExpectedBytes, 10), target)
	cmd.Stdin = io.LimitReader(zeroReader{}, spec.ExpectedBytes)
	setProbePdeathsig(cmd)
	return startProbeProcess(r, cmd)
}

func (r ExecRunner) VerifyProbeObject(ctx context.Context, configPath, remote, objectPath string, expectedBytes int64) (ProbeObjectResult, error) {
	if configPath == "" || remote == "" || objectPath == "" || expectedBytes <= 0 {
		return ProbeObjectResult{}, errors.New("invalid quota probe verification specification")
	}
	binary := r.Binary
	if binary == "" {
		binary = "rclone"
	}
	target := remote + ":" + canonicalRemotePath(objectPath)
	cmd := exec.CommandContext(ctx, binary, "--config", configPath, "lsjson", "--stat", target)
	setProbePdeathsig(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if isDefiniteProbeObjectAbsence(output) {
			return ProbeObjectResult{Evidence: string(output)}, nil
		}
		return ProbeObjectResult{Evidence: string(output)}, err
	}
	objects, err := decodeProbeObjects(output)
	if err != nil {
		return ProbeObjectResult{Evidence: string(output)}, err
	}
	result := ProbeObjectResult{ObjectCount: len(objects), Exists: len(objects) > 0, Evidence: string(output)}
	if len(objects) != 1 {
		return result, nil
	}
	object := objects[0]
	result.Object = object
	requested := canonicalRemotePath(objectPath)
	actual := canonicalRemotePath(object.Path)
	result.Exact = !object.IsDir && object.Size == expectedBytes && actual == requested
	return result, nil
}

func (r ExecRunner) DeleteProbeObject(ctx context.Context, configPath, remote, objectPath string) (ProbeCleanupResult, error) {
	if configPath == "" || remote == "" || objectPath == "" {
		return ProbeCleanupResult{}, errors.New("invalid quota probe cleanup specification")
	}
	binary := r.Binary
	if binary == "" {
		binary = "rclone"
	}
	target := remote + ":" + canonicalRemotePath(objectPath)
	cmd := exec.CommandContext(ctx, binary, "--config", configPath, "deletefile", "--drive-use-trash=false", target)
	setProbePdeathsig(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		verification, verifyErr := r.VerifyProbeObject(ctx, configPath, remote, objectPath, models.ProbeExpectedBytes)
		if verifyErr == nil && !verification.Exists {
			return ProbeCleanupResult{Absent: true, Evidence: string(output) + "\nobject absent after delete error"}, nil
		}
		return ProbeCleanupResult{Evidence: string(output)}, err
	}
	verification, verifyErr := r.VerifyProbeObject(ctx, configPath, remote, objectPath, models.ProbeExpectedBytes)
	if verifyErr != nil {
		return ProbeCleanupResult{Evidence: string(output)}, verifyErr
	}
	if verification.Exists {
		return ProbeCleanupResult{Evidence: string(output) + "\nobject remains after delete"}, errors.New("probe object remains after delete")
	}
	return ProbeCleanupResult{Absent: true, Evidence: string(output) + "\nobject absence confirmed"}, nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func startProbeProcess(r ExecRunner, cmd *exec.Cmd) (ProcessHandle, error) {
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
		return nil, &StartedProcessIdentityError{PID: cmd.Process.Pid, Cause: errors.New("unable to read probe process identity"), Result: result, WaitErr: waitErr}
	}
	return &execProcess{cmd: cmd, stdout: &stdout, stderr: &stderr, token: token}, nil
}

func decodeProbeObjects(output []byte) ([]RemoteObject, error) {
	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var values []struct {
			Path  string `json:"Path"`
			Size  int64  `json:"Size"`
			IsDir bool   `json:"IsDir"`
		}
		if err := json.Unmarshal(trimmed, &values); err != nil {
			return nil, err
		}
		objects := make([]RemoteObject, 0, len(values))
		for _, value := range values {
			objects = append(objects, RemoteObject{Path: value.Path, Size: value.Size, IsDir: value.IsDir})
		}
		return objects, nil
	}
	var value struct {
		Path  string `json:"Path"`
		Size  int64  `json:"Size"`
		IsDir bool   `json:"IsDir"`
	}
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return nil, err
	}
	if value.Path == "" && value.Size == 0 && !value.IsDir {
		return nil, nil
	}
	return []RemoteObject{{Path: value.Path, Size: value.Size, IsDir: value.IsDir}}, nil
}

func probeObjectEvidence(result ProbeObjectResult) string {
	if result.Evidence != "" {
		return result.Evidence
	}
	return fmt.Sprintf("object_count=%d exists=%t exact=%t path=%q size=%d dir=%t", result.ObjectCount, result.Exists, result.Exact, path.Clean(result.Object.Path), result.Object.Size, result.Object.IsDir)
}

// rclone 1.74.4 reports a missing file passed to lsjson --stat as a command
// failure. Only the provider's object-absence phrases are accepted here; a
// generic command, authentication, or network error must remain retryable.
func isDefiniteProbeObjectAbsence(output []byte) bool {
	lower := strings.ToLower(string(output))
	return strings.Contains(lower, "directory not found") || strings.Contains(lower, "object not found")
}
