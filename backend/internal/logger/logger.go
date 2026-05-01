package logger

import (
	"fmt"
	"os"
	"path/filepath"
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

func ReadLog(filename string, lines int) ([]string, error) {
	logMutex.RLock()
	dir := globalLogDir
	logMutex.RUnlock()

	path := filepath.Join(dir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return []string{string(data)}, nil
}

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
