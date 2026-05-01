package scheduler

import (
	"log"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"rclone-manager/internal/models"
	"rclone-manager/internal/rclone"
)

type Scheduler struct {
	cron      *cron.Cron
	entries   map[uint]cron.EntryID
	executor  *rclone.Executor
	mu        sync.RWMutex
}

func NewScheduler(executor *rclone.Executor) *Scheduler {
	return &Scheduler{
		cron:     cron.New(cron.WithSeconds()),
		entries:  make(map[uint]cron.EntryID),
		executor: executor,
	}
}

func (s *Scheduler) Start() {
	s.cron.Start()
	log.Println("Scheduler started")
}

func (s *Scheduler) Stop() {
	ctx := s.cron.Stop()
	<-ctx.Done()
	log.Println("Scheduler stopped")
}

func (s *Scheduler) AddTask(task *models.Task) error {
	if !task.ScheduleEnabled || task.ScheduleInterval <= 0 {
		return nil
	}

	// Remove existing schedule
	s.RemoveTask(task.ID)

	interval := time.Duration(task.ScheduleInterval) * time.Minute

	entryID, err := s.cron.AddFunc("@every " + interval.String(), func() {
		if !s.executor.IsRunning(task.ID) {
			log.Printf("Scheduled execution for task %d", task.ID)
			s.executor.ExecuteMove(task)
		} else {
			log.Printf("Task %d already running, skipping scheduled execution", task.ID)
		}
	})

	if err != nil {
		return err
	}

	s.mu.Lock()
	s.entries[task.ID] = entryID
	s.mu.Unlock()

	log.Printf("Added scheduled task %d with interval %v", task.ID, interval)
	return nil
}

func (s *Scheduler) RemoveTask(taskID uint) {
	s.mu.Lock()
	if entryID, exists := s.entries[taskID]; exists {
		s.cron.Remove(entryID)
		delete(s.entries, taskID)
		log.Printf("Removed scheduled task %d", taskID)
	}
	s.mu.Unlock()
}

func (s *Scheduler) GetNextRun(taskID uint) *time.Time {
	s.mu.RLock()
	entryID, exists := s.entries[taskID]
	s.mu.RUnlock()

	if !exists {
		return nil
	}

	entry := s.cron.Entry(entryID)
	next := entry.Next
	return &next
}
