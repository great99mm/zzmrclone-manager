package taskdispatch

import (
	"context"
	"errors"
	"sync"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/proactive"
)

type wakeClaimContextKey struct{}

func withWakeClaim(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, wakeClaimContextKey{}, token)
}
func wakeClaim(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(wakeClaimContextKey{}).(string)
	return value
}

var (
	ErrTaskActive = errors.New("task is active")
	ErrTaskClosed = errors.New("task launch is fenced")
)

type LegacyRunner interface {
	ExecuteMove(*models.Task) error
	IsRunning(uint) bool
	StopTask(uint) error
}

type taskGate struct {
	mu      sync.Mutex
	cond    *sync.Cond
	closed  bool
	entered int
}

func newTaskGate() *taskGate { g := &taskGate{}; g.cond = sync.NewCond(&g.mu); return g }

type runState struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// TaskDispatcher is the only trigger boundary used by scheduled, watched and
// manual task launches. It owns only short launch-state locks; actual transfer
// and quota work runs outside those locks.
type TaskDispatcher struct {
	DB        *gorm.DB
	Legacy    LegacyRunner
	Proactive *proactive.Dispatcher

	mu     sync.Mutex
	gates  map[uint]*taskGate
	active map[uint]*runState
}

func New(db *gorm.DB, legacy LegacyRunner, p *proactive.Dispatcher) *TaskDispatcher {
	return &TaskDispatcher{DB: db, Legacy: legacy, Proactive: p, gates: map[uint]*taskGate{}, active: map[uint]*runState{}}
}

func (d *TaskDispatcher) gate(id uint) *taskGate {
	d.mu.Lock()
	defer d.mu.Unlock()
	if g := d.gates[id]; g != nil {
		return g
	}
	g := newTaskGate()
	d.gates[id] = g
	return g
}

func (d *TaskDispatcher) Trigger(ctx context.Context, taskID uint, source string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	g := d.gate(taskID)
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return ErrTaskClosed
	}
	g.entered++
	var task models.Task
	err := d.DB.First(&task, taskID).Error
	if err != nil {
		g.entered--
		g.cond.Broadcast()
		g.mu.Unlock()
		return err
	}
	proactiveTask := task.TaskType == "rotation" && task.RotationStrategy == "proactive_quota" && d.Proactive != nil
	if proactiveTask && source == "start" {
		// Manual start owns the lifecycle. Clear the stop request while the
		// launch gate is held, and fail closed if StopAndWait won the CAS race.
		if err := d.Proactive.ClearStop(taskID, task.RotationStopGeneration); err != nil {
			g.entered--
			g.cond.Broadcast()
			g.mu.Unlock()
			return err
		}
		task.RotationStopRequested = false
	}
	if proactiveTask {
		if d.HasRunOwner(taskID) {
			g.entered--
			g.cond.Broadcast()
			g.mu.Unlock()
			if source == "wake" {
				return ErrTaskActive
			}
			if _, err := d.Proactive.MarkPending(taskID); err != nil {
				return err
			}
			return d.Proactive.PersistImmediateWake(taskID)
		}
		// Fence: do not start a scan while the destination scope has active
		// maintenance (manual merge or quota exhaustion). This mirrors the
		// maintenancePaused check inside RequestScan but avoids briefly
		// marking the task active in memory and then immediately returning.
		if blocked, err := d.Proactive.MutationBlocked(task); err != nil {
			g.entered--
			g.cond.Broadcast()
			g.mu.Unlock()
			return err
		} else if blocked {
			g.entered--
			g.cond.Broadcast()
			g.mu.Unlock()
			return nil
		}
	}
	runCtx := ctx
	if proactiveTask {
		runCtx = context.Background()
	}
	runCtx, cancel := context.WithCancel(runCtx)
	run := &runState{cancel: cancel, done: make(chan struct{})}
	d.mu.Lock()
	d.active[taskID] = run
	d.mu.Unlock()
	g.entered--
	g.cond.Broadcast()
	g.mu.Unlock()

	finish := func() {
		close(run.done)
		cancel()
		d.mu.Lock()
		if d.active[taskID] == run {
			delete(d.active, taskID)
		}
		d.mu.Unlock()
	}
	if proactiveTask {
		go func() { defer finish(); _ = d.Proactive.RequestScan(runCtx, taskID) }()
		if token := wakeClaim(ctx); token != "" {
			if err := d.ackWakeClaim(taskID, token); err != nil {
				return err
			}
		}
		return nil
	}
	if d.Legacy == nil {
		finish()
		return nil
	}
	if d.Legacy.IsRunning(taskID) {
		finish()
		return ErrTaskActive
	}
	err = d.Legacy.ExecuteMove(&task)
	finish()
	return err
}

func (d *TaskDispatcher) ackWakeClaim(taskID uint, token string) error {
	result := d.DB.Model(&models.Task{}).Where("id = ? AND rotation_wake_claim_token = ?", taskID, token).Updates(map[string]interface{}{"rotation_wake_claim_token": "", "rotation_wake_claim_until": nil})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("wake claim was not acknowledged")
	}
	return nil
}

func (d *TaskDispatcher) IsActive(taskID uint) bool {
	d.mu.Lock()
	_, ok := d.active[taskID]
	d.mu.Unlock()
	if ok {
		return true
	}
	if d.Proactive != nil && d.Proactive.IsActive(taskID) {
		return true
	}
	return d.Legacy != nil && d.Legacy.IsRunning(taskID)
}

// HasRunOwner reports only the adapter-owned live invocation. It deliberately
// excludes durable quota batches, which may be resumed by a fresh RequestScan.
func (d *TaskDispatcher) HasRunOwner(taskID uint) bool {
	d.mu.Lock()
	_, ok := d.active[taskID]
	d.mu.Unlock()
	return ok
}

func (d *TaskDispatcher) StopAndWait(ctx context.Context, taskID uint) error {
	if ctx == nil {
		ctx = context.Background()
	}
	g := d.gate(taskID)
	g.mu.Lock()
	g.closed = true
	for g.entered != 0 {
		g.cond.Wait()
	}
	g.mu.Unlock()
	var captured uint64
	var err error
	var task models.Task
	if loadErr := d.DB.First(&task, taskID).Error; loadErr != nil {
		d.reopen(g)
		return loadErr
	}
	proactiveTask := task.TaskType == "rotation" && task.RotationStrategy == "proactive_quota"
	if d.Proactive != nil && proactiveTask {
		captured, err = d.Proactive.RequestStop(taskID)
		if err != nil {
			d.reopen(g)
			return err
		}
	}
	d.mu.Lock()
	run := d.active[taskID]
	d.mu.Unlock()
	if run != nil {
		run.cancel()
	}
	if d.Legacy != nil && d.Legacy.IsRunning(taskID) {
		_ = d.Legacy.StopTask(taskID)
	}
	if run != nil {
		select {
		case <-run.done:
		case <-ctx.Done():
			d.reopen(g)
			return ctx.Err()
		}
	}
	if d.Proactive != nil && proactiveTask {
		if err := d.Proactive.ClearPendingGeneration(taskID, captured); err != nil && !errors.Is(err, proactive.ErrPendingSuperseded) {
			d.reopen(g)
			return err
		}
		if err := d.Proactive.StopLiveMoveProcesses(ctx, taskID); err != nil {
			d.reopen(g)
			return err
		}
	}
	d.reopen(g)
	return nil
}

func (d *TaskDispatcher) reopen(g *taskGate) {
	g.mu.Lock()
	g.closed = false
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (d *TaskDispatcher) WithTaskExclusive(ctx context.Context, taskID uint, fn func(*models.Task) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	g := d.gate(taskID)
	g.mu.Lock()
	g.closed = true
	for g.entered != 0 {
		g.cond.Broadcast()
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			d.reopen(g)
			return ctx.Err()
		default:
		}
		g.mu.Lock()
	}
	if d.IsActive(taskID) {
		g.closed = false
		g.mu.Unlock()
		return ErrTaskActive
	}
	var task models.Task
	err := d.DB.First(&task, taskID).Error
	if err == nil {
		err = fn(&task)
	}
	g.closed = false
	g.cond.Broadcast()
	g.mu.Unlock()
	return err
}
