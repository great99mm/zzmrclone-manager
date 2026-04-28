package watcher

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"rclone-manager/internal/models"
	"rclone-manager/internal/rclone"
)

type Watcher struct {
	watchers    map[uint]*fsnotify.Watcher
	executors   map[uint]*rclone.Executor
	tasks       map[uint]*models.Task
	mu          sync.RWMutex
}

func NewWatcher(executor *rclone.Executor) *Watcher {
	return &Watcher{
		watchers:  make(map[uint]*fsnotify.Watcher),
		executors: make(map[uint]*rclone.Executor),
		tasks:     make(map[uint]*models.Task),
	}
}

func (w *Watcher) StartTaskWatch(task *models.Task, executor *rclone.Executor) error {
	if !task.WatchEnabled {
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
	return w.StartTaskWatch(task, executor)
}
