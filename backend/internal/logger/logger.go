package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	globalLogDir string
	logMutex     sync.RWMutex
)

func Init(logDir string) {
	globalLogDir = logDir
	os.MkdirAll(logDir, 0755)
}

func GetLogDir() string {
	logMutex.RLock()
	defer logMutex.RUnlock()
	return globalLogDir
}

func WriteLog(filename string, content string) error {
	logMutex.RLock()
	dir := globalLogDir
	logMutex.RUnlock()

	if dir == "" {
		return fmt.Errorf("log directory not initialized")
	}

	path := filepath.Join(dir, filename)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	_, err = f.WriteString(fmt.Sprintf("[%s] %s\n", timestamp, content))
	return err
}

// ReadLog returns the LAST `lines` lines of the log file.
// This avoids the old implementation's behavior of reading the ENTIRE file
// into a single string, which caused massive memory spikes when log files
// grew to hundreds of megabytes.
//
// Algorithm: read file backwards in 8KB chunks, collecting complete lines.
// Memory usage is bounded by ~8KB regardless of file size.
func ReadLog(filename string, lines int) ([]string, error) {
	logMutex.RLock()
	dir := globalLogDir
	logMutex.RUnlock()

	if dir == "" {
		return nil, fmt.Errorf("log directory not initialized")
	}
	if lines <= 0 {
		return []string{}, nil
	}

	path := filepath.Join(dir, filename)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Get file size
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := stat.Size()
	if size == 0 {
		return []string{}, nil
	}

	// Fast path: if file is small enough, just read it normally
	if size < 8192 {
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, err
		}
		allLines := strings.Split(string(data), "\n")
		return trimEmptyTail(tailLines(allLines, lines)), nil
	}

	// Read backwards in 8KB chunks
	const chunkSize = 8192
	var result []string
	var buf []byte
	remaining := int64(size)
	incompleteLine := ""

	for remaining > 0 && len(result) < lines {
		// Determine chunk bounds
		var readSize int64 = chunkSize
		if remaining < chunkSize {
			readSize = remaining
		}
		offset := remaining - readSize

		// Read chunk
		chunk := make([]byte, readSize)
		_, err := f.ReadAt(chunk, offset)
		if err != nil && err != io.EOF {
			return nil, err
		}

		// Prepend to buffer
		buf = append(chunk, buf...)
		remaining = offset

		// Extract lines from buffer
		bufferStr := string(buf)
		bufferLines := strings.Split(bufferStr, "\n")

		// Last element may be incomplete (no trailing newline)
		// First element may be incomplete (was split across chunks)
		for i := len(bufferLines) - 1; i >= 0; i-- {
			line := bufferLines[i]
			if i == 0 && remaining > 0 {
				// First line of buffer might be incomplete (continues in earlier chunk)
				// Keep it for next iteration
				incompleteLine = line
				continue
			}
			if i == len(bufferLines)-1 && line == "" {
				// Skip trailing empty line from final newline
				continue
			}
			if incompleteLine != "" {
				line = line + incompleteLine
				incompleteLine = ""
			}
			result = append([]string{line}, result...)
			if len(result) >= lines {
				break
			}
		}

		// Reset buffer to just the incomplete prefix for next iteration
		if incompleteLine != "" {
			buf = []byte(incompleteLine)
		} else {
			buf = nil
		}
	}

	return trimEmptyTail(result), nil
}

// tailLines returns the last n elements of a slice.
func tailLines(lines []string, n int) []string {
	if n >= len(lines) {
		return lines
	}
	return lines[len(lines)-n:]
}

// trimEmptyTail removes trailing empty strings.
func trimEmptyTail(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// CleanLogs truncates all log files but DOES NOT delete them.
// Files are kept so that watchers / APIs that expect the file path still work.
func CleanLogs() error {
	logMutex.RLock()
	dir := globalLogDir
	logMutex.RUnlock()

	if dir == "" {
		return fmt.Errorf("log directory not initialized")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			os.Truncate(filepath.Join(dir, entry.Name()), 0)
		}
	}
	return nil
}

// RotateLog rotates a single log file when it exceeds maxSize.
// The current file is renamed to .1, existing .1 -> .2, etc.
// maxBackups controls how many historical files to keep.
func RotateLog(filename string, maxSize int64, maxBackups int) error {
	logMutex.RLock()
	dir := globalLogDir
	logMutex.RUnlock()

	if dir == "" {
		return fmt.Errorf("log directory not initialized")
	}

	path := filepath.Join(dir, filename)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if info.Size() < maxSize {
		return nil
	}

	// Rotate: shift old backups
	for i := maxBackups - 1; i > 0; i-- {
		oldPath := filepath.Join(dir, fmt.Sprintf("%s.%d", filename, i))
		newPath := filepath.Join(dir, fmt.Sprintf("%s.%d", filename, i+1))
		os.Rename(oldPath, newPath)
	}

	backupPath := filepath.Join(dir, fmt.Sprintf("%s.1", filename))
	os.Rename(path, backupPath)

	return nil
}
