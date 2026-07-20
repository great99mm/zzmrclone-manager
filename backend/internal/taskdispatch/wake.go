package taskdispatch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
)

type TriggerRunner interface {
	Trigger(context.Context, uint, string) error
}

type ownerRunner interface{ HasRunOwner(uint) bool }

type WakeConsumer struct {
	DB     *gorm.DB
	Runner TriggerRunner
	Now    func() time.Time
	Every  time.Duration

	RetryMax      int
	RetryDelay    time.Duration
	RetrySleep    func(time.Duration)
	LeaseDuration time.Duration
	stop          chan struct{}
	done          chan struct{}
	once          sync.Once
}

func (w *WakeConsumer) Start() {
	w.once.Do(func() { w.stop = make(chan struct{}); w.done = make(chan struct{}); go w.loop() })
}
func (w *WakeConsumer) Stop() {
	if w.stop != nil {
		close(w.stop)
		<-w.done
	}
}
func (w *WakeConsumer) loop() {
	defer close(w.done)
	interval := w.Every
	if interval <= 0 {
		interval = time.Second
	}
	clock := w.Now
	if clock == nil {
		clock = time.Now
	}
	w.poll(clock())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			w.poll(clock())
		case <-w.stop:
			return
		}
	}
}
func (w *WakeConsumer) Poll(now time.Time) { w.poll(now) }

func (w *WakeConsumer) poll(now time.Time) {
	if w.DB == nil || w.Runner == nil {
		return
	}
	var tasks []models.Task
	if err := w.DB.Where("enabled = ? AND task_type = ? AND rotation_strategy = ? AND rotation_rescan_pending = ? AND rotation_stop_requested = ? AND rotation_quota_wake_at IS NOT NULL AND rotation_quota_wake_at <= ?", true, "rotation", "proactive_quota", true, false, now).Order("id ASC").Find(&tasks).Error; err != nil {
		return
	}
	// Keep the ordering explicit even when a test database/mock ignores ORDER BY.
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	for _, task := range tasks {
		owner, ownerAware := w.Runner.(ownerRunner)
		if !ownerAware {
			continue
		}
		if owner.HasRunOwner(task.ID) {
			continue
		}
		token := fmt.Sprintf("wake-%d-%d", task.ID, atomic.AddUint64(&wakeClaimCounter, 1))
		claimed, err := w.claim(task.ID, task.RotationRescanGeneration, now, token)
		if err != nil || !claimed {
			continue
		}
		// Recheck the same adapter state after the CAS. An active launch can
		// begin between the initial predicate and claim; restore this generation
		// rather than manufacturing a second proactive request.
		if owner.HasRunOwner(task.ID) {
			_ = w.release(task.ID, token)
			continue
		}
		if err := w.Runner.Trigger(withWakeClaim(context.Background(), token), task.ID, "wake"); err != nil {
			_ = w.restore(task.ID, task.RotationRescanGeneration, now, token)
		}
	}
}

// claimWake leases only the exact due generation observed by this poll while
// leaving pending durable. Competing pollers therefore cannot launch the same
// wake, and a crashed owner can be retried after lease expiry.
var wakeClaimCounter uint64

func (w *WakeConsumer) claim(id uint, generation uint64, now time.Time, token string) (bool, error) {
	lease := now.Add(w.leaseDuration())
	var changed bool
	err := w.retry(func() error {
		result := w.DB.Model(&models.Task{}).Where("id = ? AND enabled = ? AND task_type = ? AND rotation_strategy = ? AND rotation_rescan_pending = ? AND rotation_stop_requested = ? AND rotation_rescan_generation = ? AND rotation_quota_wake_at IS NOT NULL AND rotation_quota_wake_at <= ? AND (rotation_wake_claim_token = '' OR rotation_wake_claim_until IS NULL OR rotation_wake_claim_until <= ?)", id, true, "rotation", "proactive_quota", true, false, generation, now, now).Updates(map[string]interface{}{
			"rotation_wake_claim_token": token,
			"rotation_wake_claim_until": lease,
		})
		if result.Error != nil {
			return result.Error
		}
		changed = result.RowsAffected == 1
		return nil
	})
	return changed, err
}

func (w *WakeConsumer) release(id uint, token string) error {
	return w.retry(func() error {
		return w.DB.Model(&models.Task{}).Where("id = ? AND rotation_wake_claim_token = ?", id, token).Updates(map[string]interface{}{"rotation_wake_claim_token": "", "rotation_wake_claim_until": nil}).Error
	})
}

func (w *WakeConsumer) restore(id uint, generation uint64, now time.Time, token string) error {
	return w.retry(func() error {
		wake := now.Add(time.Minute)
		result := w.DB.Model(&models.Task{}).Where("id = ? AND task_type = ? AND rotation_strategy = ? AND rotation_rescan_generation = ? AND rotation_rescan_pending = ? AND rotation_stop_requested = ? AND rotation_wake_claim_token = ?", id, "rotation", "proactive_quota", generation, true, false, token).Updates(map[string]interface{}{
			"rotation_quota_wake_at":    wake,
			"rotation_wake_claim_token": "",
			"rotation_wake_claim_until": nil,
		})
		if result.Error != nil {
			return result.Error
		}
		return nil
	})
}

func (w *WakeConsumer) leaseDuration() time.Duration {
	if w.LeaseDuration > 0 {
		return w.LeaseDuration
	}
	return time.Minute
}

func (w *WakeConsumer) retry(fn func() error) error {
	max := w.RetryMax
	if max <= 0 {
		max = 8
	}
	var err error
	for attempt := 0; attempt < max; attempt++ {
		err = fn()
		if err == nil {
			return nil
		}
		if !retryableWakeDBError(err) {
			return err
		}
		w.sleep(attempt)
	}
	return err
}

func (w *WakeConsumer) sleep(attempt int) {
	delay := w.RetryDelay
	if delay <= 0 {
		delay = 50 * time.Millisecond
	}
	for i := 0; i < attempt; i++ {
		delay *= 2
	}
	if w.RetrySleep != nil {
		w.RetrySleep(delay)
	} else {
		time.Sleep(delay)
	}
}

func retryableWakeDBError(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return errors.Is(err, gorm.ErrInvalidTransaction) || containsAny(text, "database is locked", "database table is locked", "busy")
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
