package taskdispatch

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/proactive"
	"rclone-manager/internal/quota"
)

type fakeLegacy struct {
	mu      sync.Mutex
	calls   []models.Task
	stopped []uint
	err     error
}

type blockingProactiveScanner struct {
	entered chan struct{}
	release chan struct{}
}

func (s *blockingProactiveScanner) Scan(string, time.Duration) ([]quota.LocalSnapshot, error) {
	s.entered <- struct{}{}
	<-s.release
	return nil, nil
}

type idleProactiveExecutor struct{}

func (idleProactiveExecutor) RunBatch(context.Context, uint) error     { return nil }
func (idleProactiveExecutor) RecoverBatch(context.Context, uint) error { return nil }

var testDBCounter uint64

func (f *fakeLegacy) ExecuteMove(t *models.Task) error {
	f.mu.Lock()
	f.calls = append(f.calls, *t)
	f.mu.Unlock()
	return f.err
}
func (f *fakeLegacy) IsRunning(uint) bool { return false }
func (f *fakeLegacy) StopTask(id uint) error {
	f.mu.Lock()
	f.stopped = append(f.stopped, id)
	f.mu.Unlock()
	return nil
}

func taskDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s-%d?mode=memory&cache=shared", t.Name(), atomic.AddUint64(&testDBCounter, 1))), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Task{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestTriggerReloadsTaskByID(t *testing.T) {
	db := taskDB(t)
	task := models.Task{TaskType: "normal", Name: "before"}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	legacy := &fakeLegacy{}
	d := New(db, legacy, nil)
	if err := db.Model(&task).Update("name", "after").Error; err != nil {
		t.Fatal(err)
	}
	if err := d.Trigger(context.Background(), task.ID, "start"); err != nil {
		t.Fatal(err)
	}
	legacy.mu.Lock()
	defer legacy.mu.Unlock()
	if len(legacy.calls) != 1 || legacy.calls[0].Name != "after" {
		t.Fatalf("calls=%#v", legacy.calls)
	}
}

func TestLegacyTriggerPropagatesExecuteError(t *testing.T) {
	db := taskDB(t)
	task := models.Task{TaskType: "normal"}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	want := context.Canceled
	if err := (func() error {
		return New(db, &fakeLegacy{err: want}, nil).Trigger(context.Background(), task.ID, "start")
	})(); err != want {
		t.Fatalf("err=%v want=%v", err, want)
	}
}

type fakeWakeRunner struct {
	mu          sync.Mutex
	calls       []uint
	active      map[uint]bool
	activeAfter int
	checks      int
	err         error
}

func (f *fakeWakeRunner) Trigger(_ context.Context, id uint, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id)
	return f.err
}
func (f *fakeWakeRunner) IsActive(id uint) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	if f.activeAfter > 0 && f.checks >= f.activeAfter {
		return true
	}
	return f.active[id]
}
func (f *fakeWakeRunner) HasRunOwner(id uint) bool { return f.IsActive(id) }

func proactiveWakeTask(wake time.Time) models.Task {
	return models.Task{TaskType: "rotation", RotationStrategy: "proactive_quota", SourceType: "local", SourceDir: "/src", DestType: "remote", RemoteName: "drive", TransferMode: models.TransferModeCopy, RotationRemotes: `["drive"]`, RotationRescanPending: true, RotationRescanGeneration: 1, RotationQuotaWakeAt: &wake}
}

func TestWakeConsumerUsesSortedDueIDsAndConsumesOnce(t *testing.T) {
	db := taskDB(t)
	now := time.Unix(100, 0)
	due := now.Add(-time.Second)
	tasks := []models.Task{proactiveWakeTask(due), proactiveWakeTask(due)}
	for i := range tasks {
		if err := db.Create(&tasks[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeWakeRunner{}
	w := &WakeConsumer{DB: db, Runner: runner}
	w.Poll(now)
	w.Poll(now)
	runner.mu.Lock()
	if len(runner.calls) != 2 || runner.calls[0] != tasks[0].ID || runner.calls[1] != tasks[1].ID {
		t.Fatalf("calls=%v", runner.calls)
	}
	runner.mu.Unlock()
	var stored []models.Task
	if err := db.Order("id").Find(&stored).Error; err != nil {
		t.Fatal(err)
	}
	for _, task := range stored {
		if !task.RotationRescanPending || task.RotationWakeClaimToken == "" || task.RotationWakeClaimUntil == nil {
			t.Fatalf("wake lease was not retained: %#v", task)
		}
	}
	w.Poll(now.Add(2 * time.Minute))
	runner.mu.Lock()
	if len(runner.calls) != 4 {
		t.Fatalf("expired lease was not reconsumed: %v", runner.calls)
	}
	runner.mu.Unlock()
}

func TestWakeConsumerActiveSkipRetainsWake(t *testing.T) {
	db := taskDB(t)
	now := time.Unix(100, 0)
	due := now.Add(-time.Second)
	task := proactiveWakeTask(due)
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	runner := &fakeWakeRunner{active: map[uint]bool{task.ID: true}}
	if !runner.IsActive(task.ID) {
		t.Fatalf("runner active state missing for task %d", task.ID)
	}
	(&WakeConsumer{DB: db, Runner: runner}).Poll(now)
	runner.mu.Lock()
	if len(runner.calls) != 0 {
		t.Fatalf("calls=%v", runner.calls)
	}
	runner.mu.Unlock()
	var stored models.Task
	if err := db.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !stored.RotationRescanPending || stored.RotationQuotaWakeAt == nil {
		t.Fatalf("active wake was consumed: %#v", stored)
	}
}

func TestWakeConsumerSkipsDisabledProactiveTask(t *testing.T) {
	db := taskDB(t)
	now := time.Unix(100, 0)
	due := now.Add(-time.Second)
	task := proactiveWakeTask(due)
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&task).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	runner := &fakeWakeRunner{}
	(&WakeConsumer{DB: db, Runner: runner}).Poll(now)
	runner.mu.Lock()
	if len(runner.calls) != 0 {
		t.Fatalf("disabled task was triggered: %v", runner.calls)
	}
	runner.mu.Unlock()
}

func TestWakeConsumerRepeatedConcurrentActiveSkipDoesNotTrigger(t *testing.T) {
	db := taskDB(t)
	now := time.Unix(100, 0)
	due := now.Add(-time.Second)
	task := proactiveWakeTask(due)
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	runner := &fakeWakeRunner{active: map[uint]bool{task.ID: true}}
	if !runner.IsActive(task.ID) {
		t.Fatalf("runner active state missing for task %d", task.ID)
	}
	w := &WakeConsumer{DB: db, Runner: runner, RetrySleep: func(time.Duration) {}}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				w.Poll(now)
			}
		}()
	}
	wg.Wait()
	runner.mu.Lock()
	if len(runner.calls) != 0 {
		t.Fatalf("active task was triggered: %v", runner.calls)
	}
	runner.mu.Unlock()
	var stored models.Task
	if err := db.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !stored.RotationRescanPending || stored.RotationQuotaWakeAt == nil {
		t.Fatalf("active wake changed: %#v", stored)
	}
}

func TestWakeConsumerTriggerFailureRestoresRetryWake(t *testing.T) {
	db := taskDB(t)
	now := time.Unix(100, 0)
	due := now.Add(-time.Second)
	task := proactiveWakeTask(due)
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	runner := &fakeWakeRunner{err: context.Canceled}
	(&WakeConsumer{DB: db, Runner: runner}).Poll(now)
	var stored models.Task
	if err := db.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !stored.RotationRescanPending || stored.RotationQuotaWakeAt == nil || !stored.RotationQuotaWakeAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("retry wake missing: %#v", stored)
	}
}

func TestWakeConsumerRechecksActiveAfterClaim(t *testing.T) {
	db := taskDB(t)
	now := time.Unix(100, 0)
	due := now.Add(-time.Second)
	task := proactiveWakeTask(due)
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	runner := &fakeWakeRunner{activeAfter: 2}
	(&WakeConsumer{DB: db, Runner: runner}).Poll(now)
	runner.mu.Lock()
	if len(runner.calls) != 0 {
		t.Fatalf("race started a duplicate trigger: %v", runner.calls)
	}
	runner.mu.Unlock()
	var stored models.Task
	if err := db.First(&stored, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !stored.RotationRescanPending || stored.RotationQuotaWakeAt == nil {
		t.Fatalf("claimed wake was lost: %#v", stored)
	}
}

func TestWakeConsumerConcurrentPollClaimsOnce(t *testing.T) {
	db := taskDB(t)
	now := time.Unix(100, 0)
	due := now.Add(-time.Second)
	task := proactiveWakeTask(due)
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	runner := &fakeWakeRunner{}
	w := &WakeConsumer{DB: db, Runner: runner, RetrySleep: func(time.Duration) {}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); w.Poll(now) }()
	}
	wg.Wait()
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.calls) != 1 {
		t.Fatalf("duplicate calls=%v", runner.calls)
	}
}

func TestMutationFenceRejectsActiveRun(t *testing.T) {
	db := taskDB(t)
	task := models.Task{TaskType: "normal"}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	d := New(db, &fakeLegacy{}, nil)
	d.mu.Lock()
	d.active[task.ID] = &runState{done: make(chan struct{}), cancel: func() {}}
	d.mu.Unlock()
	if err := d.WithTaskExclusive(context.Background(), task.ID, func(*models.Task) error { return nil }); err != ErrTaskActive {
		t.Fatalf("err=%v", err)
	}
}

func TestProactiveTriggerFencedByActiveManualMaintenance(t *testing.T) {
	db := taskDB(t)
	if err := db.AutoMigrate(&models.DestinationScopeMaintenance{}, &models.DestinationScopeCoordinator{}); err != nil {
		t.Fatal(err)
	}
	task := models.Task{Name: "proactive", TaskType: "rotation", RotationStrategy: "proactive_quota", SourceType: "local", SourceDir: "/tmp/source", DestType: "remote", RemoteName: "drive", RemoteDir: "/dst", TransferMode: models.TransferModeCopy, RotationRemotes: `["drive"]`, MinAge: "0s", Enabled: true, RotationQuotaLimitBytes: models.DefaultRotationQuotaLimitBytes, Status: "idle"}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	configPath := "/tmp/rclone.conf"
	if err := db.Create(&models.DestinationScopeMaintenance{DestinationScope: models.DestinationScope(configPath, "/dst"), Epoch: 1, OwnerTaskID: task.ID, State: models.MaintenanceStateExhausted, DedupeState: models.DedupeStatePending, Reason: models.MaintenanceReasonManualMerge, Revision: 1}).Error; err != nil {
		t.Fatal(err)
	}
	p := &proactive.Dispatcher{DB: db, Executor: idleProactiveExecutor{}, ConfigResolver: func(string) (string, error) { return configPath, nil }, Now: func() time.Time { return time.Unix(100, 0) }}
	d := New(db, nil, p)
	if err := d.Trigger(context.Background(), task.ID, "start"); err != nil {
		t.Fatalf("Trigger during active maintenance returned err=%v (expected no-op)", err)
	}
	if d.HasRunOwner(task.ID) {
		t.Fatal("Trigger created a run owner during active maintenance")
	}
	if d.IsActive(task.ID) {
		t.Fatal("Trigger marked task active during maintenance fence")
	}
}

func TestProactiveTriggerIgnoresLedgerActiveWithoutRunOwner(t *testing.T) {
	db := taskDB(t)
	if err := db.AutoMigrate(&models.QuotaAccount{}, &models.RotationQuotaOversize{}, &models.RotationQuotaDirectoryAssignment{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{}, &models.QuotaReservation{}); err != nil {
		t.Fatal(err)
	}
	task := models.Task{Name: "proactive", TaskType: "rotation", RotationStrategy: "proactive_quota", SourceType: "local", SourceDir: "/tmp/source", DestType: "remote", RemoteName: "drive", RemoteDir: "/dst", TransferMode: models.TransferModeCopy, RotationRemotes: `["drive"]`, MinAge: "0s", Enabled: true, RotationQuotaLimitBytes: models.DefaultRotationQuotaLimitBytes}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.RotationQuotaBatch{TaskID: task.ID, RequestKey: "planned", State: models.BatchStatePlanned}).Error; err != nil {
		t.Fatal(err)
	}
	scanner := &blockingProactiveScanner{entered: make(chan struct{}, 2), release: make(chan struct{}, 2)}
	p := &proactive.Dispatcher{DB: db, Quota: &quota.Service{DB: db, ConfigResolver: func(raw string) (string, error) { return raw, nil }}, Executor: idleProactiveExecutor{}, Scanner: func(models.Task) proactive.LocalScanner { return scanner }, Now: func() time.Time { return time.Unix(100, 0) }}
	d := New(db, nil, p)
	if err := d.Trigger(context.Background(), task.ID, "start"); err != nil {
		t.Fatal(err)
	}
	<-scanner.entered
	if !d.HasRunOwner(task.ID) {
		t.Fatal("owner was not registered")
	}
	if err := d.Trigger(context.Background(), task.ID, "watch"); err != nil {
		t.Fatal(err)
	}
	scanner.release <- struct{}{}
	for d.HasRunOwner(task.ID) {
		runtime.Gosched()
	}
	var pending models.Task
	if err := db.First(&pending, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !pending.RotationRescanPending || pending.RotationQuotaWakeAt == nil {
		t.Fatalf("owner cleanup stranded pending work: %#v", pending)
	}
	if err := d.Trigger(context.Background(), task.ID, "start"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-scanner.entered:
	case <-time.After(time.Second):
		t.Fatal("manual resume did not call RequestScan")
	}
	scanner.release <- struct{}{}
}
