package proactive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"rclone-manager/internal/models"
)

func manifestBytes(files []models.RotationQuotaBatchFile) ([]byte, error) {
	ordered := append([]models.RotationQuotaBatchFile(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].RelativePath < ordered[j].RelativePath })
	var builder strings.Builder
	for _, file := range ordered {
		if err := validateManifestPath(file.RelativePath); err != nil {
			return nil, err
		}
		builder.WriteString(file.RelativePath)
		builder.WriteByte('\n')
	}
	return []byte(builder.String()), nil
}

func validateManifestPath(value string) error {
	if value == "" || strings.ContainsAny(value, "\r\n\x00") || filepath.IsAbs(value) {
		return fmt.Errorf("unsafe manifest path %q", value)
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	if clean != value || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("non-canonical manifest path %q", value)
	}
	return nil
}

func manifestHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type ManifestWriter struct {
	CreateTemp func(dir, pattern string) (*os.File, error)
	Rename     func(oldPath, newPath string) error
	SyncDir    func(dir string) error
}

func (w ManifestWriter) Write(dir string, batch models.RotationQuotaBatch, files []models.RotationQuotaBatchFile) (string, string, []byte, error) {
	if !models.IsValidOwnerToken(batch.OwnerToken) {
		return "", "", nil, fmt.Errorf("invalid batch owner token")
	}
	data, err := manifestBytes(files)
	if err != nil {
		return "", "", nil, err
	}
	hash := manifestHash(data)
	if batch.ManifestPath != "" {
		expectedDir, dirErr := filepath.Abs(dir)
		existingPath, pathErr := filepath.Abs(batch.ManifestPath)
		expectedName := fmt.Sprintf("batch-%d-%s.manifest", batch.ID, batch.OwnerToken)
		if dirErr != nil || pathErr != nil || filepath.Dir(existingPath) != expectedDir || filepath.Base(existingPath) != expectedName {
			return "", "", nil, fmt.Errorf("existing manifest is not owned by this batch")
		}
		current, readErr := os.ReadFile(batch.ManifestPath)
		if readErr == nil && string(current) == string(data) && manifestHash(current) == hash {
			return batch.ManifestPath, hash, data, nil
		}
		return "", "", nil, fmt.Errorf("existing manifest conflicts with batch bytes")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", "", nil, err
	}
	create := w.CreateTemp
	if create == nil {
		create = os.CreateTemp
	}
	temp, err := create(dir, ".quota-manifest-*")
	if err != nil {
		return "", "", nil, err
	}
	tempPath := temp.Name()
	cleanup := func() { _ = temp.Close(); _ = os.Remove(tempPath) }
	if err := temp.Chmod(0600); err != nil {
		cleanup()
		return "", "", nil, err
	}
	if _, err := temp.Write(data); err != nil {
		cleanup()
		return "", "", nil, err
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return "", "", nil, err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return "", "", nil, err
	}
	destination := filepath.Join(dir, fmt.Sprintf("batch-%d-%s.manifest", batch.ID, batch.OwnerToken))
	rename := w.Rename
	if rename == nil {
		rename = os.Rename
	}
	if err := rename(tempPath, destination); err != nil {
		_ = os.Remove(tempPath)
		return "", "", nil, err
	}
	syncDir := w.SyncDir
	if syncDir == nil {
		syncDir = syncDirectory
	}
	if err := syncDir(dir); err != nil {
		_ = os.Remove(destination)
		return "", "", nil, err
	}
	return destination, hash, data, nil
}

func syncDirectory(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Sync(); err != nil && err != syscall.EINVAL {
		return err
	}
	return nil
}
