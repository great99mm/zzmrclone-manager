package manualtransfer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/proactive"
	"rclone-manager/internal/quota"
)

type manualWorkerFixture struct {
	db       *gorm.DB
	service  *Service
	run      ManualTransferRun
	root     string
	files    []quota.LocalSnapshot
	runner   *manualWorkerFakeRunner
	accounts []ManualRunAccount
}

func manualWorkerDB(t *testing.T) *gorm.DB {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "manual-workers.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.AutoMigrate(
		&models.Task{}, &models.QuotaAccount{}, &models.QuotaReservation{}, &models.RotationQuotaBatch{}, &models.RotationQuotaBatchFile{},
		&ManualTransferRun{}, &ManualRunAccount{}, &ManualRunFile{}, &ManualRunAllocation{}, &ManualRunEvent{},
		&ManualRunWorker{}, &ManualWorkerAttempt{}, &ManualWorkerFile{}, &ManualWorkerEvent{}, &ManualWorkerProgress{}, &ManualWorkerLog{},
	); err != nil {
		t.Fatal(err)
	}
	return database
}

func newManualWorkerFixture(t *testing.T, mode string, files map[string]string) manualWorkerFixture {
	t.Helper()
	database := manualWorkerDB(t)
	root := t.TempDir()
	for name, value := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(config, []byte("[remote-a]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	scanned, err := (quota.Scanner{}).Scan(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	rootHandle, err := quota.OpenSourceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	task := models.Task{ID: 901, Name: "manual-worker", TaskType: models.TaskTypeManual, ManualStrategy: models.ManualStrategyAllocation, SourceType: "local", SourceDir: root, DestType: "remote", RemoteName: "remote", RemoteDir: "/dest", TransferMode: mode, RcloneConfig: config, Enabled: true}
	if err := database.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	accounts := []ManualRunAccount{{RunID: 1, Position: 0, AccountID: 1, AccountIdentity: "account-a", RemoteName: "remote-a", ConfigIdentity: config}, {RunID: 1, Position: 1, AccountID: 2, AccountIdentity: "account-b", RemoteName: "remote-b", ConfigIdentity: config}}
	for _, account := range accounts {
		if err := database.Create(&models.QuotaAccount{ID: account.AccountID, QuotaKey: account.AccountIdentity, RemoteName: account.RemoteName, ConfigIdentity: config, Enabled: true}).Error; err != nil {
			t.Fatal(err)
		}
	}
	run := ManualTransferRun{TaskID: task.ID, State: ManualRunStateAllocated, Revision: 2, ManualInputRevision: 1, ManualConfigRevision: 1, ActorIdentity: "operator", ActorType: "admin_session", SourcePath: root, DestinationPath: "/dest", TransferMode: mode, ConfigIdentity: config, FrozenInput: "{}", IdempotencyKey: "seed-worker", RequestFingerprint: "seed-worker", SourceRootDevice: rootHandle.Device, SourceRootInode: rootHandle.Inode, SnapshotGeneration: 1, AllocationGeneration: 1, AllocationVersion: ManualAllocationVersion}
	_ = rootHandle.Close()
	for _, file := range scanned {
		run.SnapshotCount++
		run.SnapshotBytes += file.SizeBytes
	}
	if err := database.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		account.RunID = run.ID
		if err := database.Create(&account).Error; err != nil {
			t.Fatal(err)
		}
	}
	for index, file := range scanned {
		if err := database.Create(&ManualRunFile{RunID: run.ID, Generation: 1, RelativePath: file.RelativePath, SnapshotKey: file.SnapshotKey, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode, ActivatedAt: ptrTime(time.Now().UTC())}).Error; err != nil {
			t.Fatal(err)
		}
		position := index % 2
		account := accounts[position]
		if err := database.Create(&ManualRunAllocation{RunID: run.ID, Generation: 1, RelativePath: file.RelativePath, SnapshotKey: file.SnapshotKey, SizeBytes: file.SizeBytes, AccountPosition: &position, AccountID: account.AccountID, AccountIdentity: account.AccountIdentity, ActivatedAt: ptrTime(time.Now().UTC())}).Error; err != nil {
			t.Fatal(err)
		}
	}
	runner := &manualWorkerFakeRunner{sizes: make(map[string]int64)}
	for _, file := range scanned {
		runner.sizes[file.RelativePath] = file.SizeBytes
	}
	service := NewService(database)
	service.Runner = runner
	service.LogDir = t.TempDir()
	service.ManifestDir = t.TempDir()
	service.StageDir = t.TempDir()
	service.Start()
	t.Cleanup(func() { service.Stop() })
	return manualWorkerFixture{db: database, service: service, run: run, root: root, files: scanned, runner: runner, accounts: accounts}
}

type manualWorkerFakeRunner struct {
	mu                   sync.Mutex
	sizes                map[string]int64
	specs                []proactive.CopySpec
	moveSpecs            []proactive.MoveSpec
	manifests            [][]string
	remote               map[string]bool
	startErr             error
	movePartialThenError bool
	failFirst            bool
	started              chan struct{}
	processes            []*manualWorkerFakeProcess
	streamed             bool
	streamedOutput       string
	resultStdout         string
	resultStderr         string
	statCalls            map[string]int
	statCallsAtStart     map[string][]int
	statErrors           map[string]error
	statStarted          chan struct{}
}

func (r *manualWorkerFakeRunner) StartCopy(_ context.Context, spec proactive.CopySpec) (proactive.ProcessHandle, error) {
	data, err := os.ReadFile(spec.ManifestPath)
	if err != nil {
		return nil, err
	}
	paths := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(paths) == 1 && paths[0] == "" {
		paths = nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.statCalls == nil {
		r.statCalls = make(map[string]int)
	}
	if r.statCallsAtStart == nil {
		r.statCallsAtStart = make(map[string][]int)
	}
	r.statCallsAtStart[spec.DestinationRemote] = append(r.statCallsAtStart[spec.DestinationRemote], r.statCalls[spec.DestinationRemote])
	r.specs = append(r.specs, spec)
	r.manifests = append(r.manifests, append([]string(nil), paths...))
	if r.startErr != nil {
		return nil, r.startErr
	}
	if r.remote == nil {
		r.remote = make(map[string]bool)
	}
	if r.failFirst && len(r.processes) == 0 {
		if len(paths) > 0 {
			r.remote[paths[0]] = true
		}
	} else {
		for _, path := range paths {
			r.remote[path] = true
		}
	}
	process := &manualWorkerFakeProcess{result: proactive.ProcessResult{ExitCode: 0, PID: 100 + len(r.processes), ProcessStartToken: "fake-token"}, done: make(chan struct{})}
	process.result.Stdout = r.resultStdout
	process.result.Stderr = r.resultStderr
	if r.failFirst && len(r.processes) == 0 {
		process.result.ExitCode = 1
		process.result.Stderr = "token=super-secret"
	}
	r.processes = append(r.processes, process)
	if r.streamed {
		output := make(chan proactive.ProcessOutputChunk, 4)
		progress := make(chan proactive.ProcessProgress, 4)
		streamed := &manualWorkerStreamingProcess{manualWorkerFakeProcess: process, output: output, progress: progress}
		go func() {
			streamedOutput := r.streamedOutput
			if streamedOutput == "" {
				streamedOutput = "token=stream-secret token=stream-secret\n"
			}
			output <- proactive.ProcessOutputChunk{Stream: "stderr", Data: streamedOutput}
			progress <- proactive.ProcessProgress{RelativePath: paths[0], CompletedCount: 1, CompletedBytes: r.sizes[paths[0]], SpeedBytesPerSecond: 42, ProgressPercent: 50}
			<-process.done
			close(output)
			close(progress)
		}()
		if r.started == nil {
			process.release()
		}
		if r.started != nil {
			select {
			case <-r.started:
			default:
				close(r.started)
			}
		}
		return streamed, nil
	}
	if r.started == nil {
		process.release()
	}
	if r.started != nil {
		select {
		case <-r.started:
		default:
			close(r.started)
		}
	}
	return process, nil
}

func (r *manualWorkerFakeRunner) StartMove(_ context.Context, spec proactive.MoveSpec) (proactive.ProcessHandle, error) {
	data, err := os.ReadFile(spec.ManifestPath)
	if err != nil {
		return nil, err
	}
	paths := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(paths) == 1 && paths[0] == "" {
		paths = nil
	}
	r.mu.Lock()
	r.moveSpecs = append(r.moveSpecs, spec)
	startErr := r.startErr
	partial := r.movePartialThenError
	r.mu.Unlock()
	if partial && len(paths) > 0 {
		if err := removeManualWorkerMoveFiles(paths[:1], spec.SourceRoot); err != nil {
			return nil, err
		}
		return nil, errors.New("move process failed after partial handoff")
	}
	if startErr != nil {
		return nil, startErr
	}
	if err := removeManualWorkerMoveFiles(paths, spec.SourceRoot); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	process := &manualWorkerFakeProcess{result: proactive.ProcessResult{ExitCode: 0, PID: 200 + len(r.processes), ProcessStartToken: "move-token"}, done: make(chan struct{})}
	r.processes = append(r.processes, process)
	if r.streamed && spec.OutputSink != nil {
		streamedOutput := r.streamedOutput
		if streamedOutput == "" {
			streamedOutput = "token=move-stream-secret token=move-stream-secret\n"
		}
		go spec.OutputSink(proactive.ProcessOutputChunk{Stream: "stderr", Data: streamedOutput})
	}
	if r.started == nil {
		process.release()
	}
	if r.started != nil {
		select {
		case <-r.started:
		default:
			close(r.started)
		}
	}
	return process, nil
}

func removeManualWorkerMoveFiles(paths []string, sourceRoot *os.File) error {
	if sourceRoot == nil {
		return errors.New("move source root is unavailable")
	}
	for _, relative := range paths {
		if relative == "" {
			continue
		}
		if err := os.Remove(filepath.Join(fmt.Sprintf("/proc/self/fd/%d", sourceRoot.Fd()), filepath.FromSlash(relative))); err != nil {
			return err
		}
	}
	return nil
}

func (r *manualWorkerFakeRunner) StatRemote(ctx context.Context, _ string, remote string, _ string, relative string) (proactive.RemoteObject, error) {
	r.mu.Lock()
	if r.statCalls == nil {
		r.statCalls = make(map[string]int)
	}
	r.statCalls[remote]++
	statStarted := r.statStarted
	if statStarted != nil {
		select {
		case <-statStarted:
		default:
			close(statStarted)
		}
	}
	statErr := r.statErrors[relative]
	remoteExists := r.remote[relative]
	size := r.sizes[relative]
	r.mu.Unlock()
	if statStarted != nil {
		<-ctx.Done()
		return proactive.RemoteObject{}, ctx.Err()
	}
	if statErr != nil {
		return proactive.RemoteObject{}, statErr
	}
	if !remoteExists {
		return proactive.RemoteObject{}, proactive.ErrRemoteObjectNotFound
	}
	return proactive.RemoteObject{Path: relative, Size: size}, nil
}

type manualWorkerFakeProcess struct {
	mu      sync.Mutex
	result  proactive.ProcessResult
	done    chan struct{}
	stopped bool
	once    sync.Once
}

func (p *manualWorkerFakeProcess) Wait() (proactive.ProcessResult, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped && p.result.ExitCode == 0 {
		p.result.ExitCode = 1
	}
	return p.result, nil
}
func (p *manualWorkerFakeProcess) Stop() error {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.once.Do(func() { close(p.done) })
	return nil
}
func (p *manualWorkerFakeProcess) PID() int           { return p.result.PID }
func (p *manualWorkerFakeProcess) StartToken() string { return p.result.ProcessStartToken }

func (p *manualWorkerFakeProcess) release() { p.once.Do(func() { close(p.done) }) }

type manualWorkerStreamingProcess struct {
	*manualWorkerFakeProcess
	output   <-chan proactive.ProcessOutputChunk
	progress <-chan proactive.ProcessProgress
}

func (p *manualWorkerStreamingProcess) Output() <-chan proactive.ProcessOutputChunk { return p.output }
func (p *manualWorkerStreamingProcess) Progress() <-chan proactive.ProcessProgress  { return p.progress }

type manualWorkerInspector struct {
	mu       sync.Mutex
	status   proactive.ProcessStatus
	inspects int
	stops    int
	stopErr  error
}

func (i *manualWorkerInspector) Inspect(int, string) (proactive.ProcessStatus, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.inspects++
	return i.status, nil
}

func (i *manualWorkerInspector) StopVerified(int, string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.stops++
	if i.stopErr != nil {
		return i.stopErr
	}
	i.status = proactive.ProcessStatus{Confirmed: true, Alive: false}
	return nil
}

type manualWorkerTaskFence struct {
	database *gorm.DB
	mu       sync.Mutex
	ids      []uint
}

func (f *manualWorkerTaskFence) WithTaskExclusive(_ context.Context, taskID uint, fn func(*models.Task) error) error {
	f.mu.Lock()
	f.ids = append(f.ids, taskID)
	f.mu.Unlock()
	var task models.Task
	if err := f.database.First(&task, taskID).Error; err != nil {
		return err
	}
	return fn(&task)
}

func waitWorkerState(t *testing.T, service *Service, workerID uint, want string) ManualRunWorker {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		detail, err := service.GetWorker(workerID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Worker.State == want {
			return detail.Worker
		}
		time.Sleep(5 * time.Millisecond)
	}
	detail, _ := service.GetWorker(workerID)
	t.Fatalf("worker %d did not reach %q: %#v", workerID, want, detail.Worker)
	return detail.Worker
}

func startFixture(t *testing.T, fixture manualWorkerFixture, key string) StartResult {
	t.Helper()
	run := fixture.run
	result, err := fixture.service.StartRun(context.Background(), StartRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: key, ActorIdentity: "operator", ActorType: "admin_session"})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestManualCopyWorkersHappyPathIsolationAndIdempotentStart(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a", "nested/b.txt": "bb", "c.txt": "ccc"})
	first := startFixture(t, fixture, "start-once")
	replay, err := fixture.service.StartRun(context.Background(), StartRequest{RunID: fixture.run.ID, ExpectedRunID: &fixture.run.ID, ExpectedRevision: fixture.run.Revision, ExpectedConfigRevision: fixture.run.ManualConfigRevision, IdempotencyKey: "start-once", ActorIdentity: "operator", ActorType: "admin_session"})
	if err != nil || !replay.Existing || len(replay.WorkerIDs) != len(first.WorkerIDs) {
		t.Fatalf("idempotent start = %#v, err=%v", replay, err)
	}
	for _, workerID := range first.WorkerIDs {
		waitWorkerState(t, fixture.service, workerID, ManualWorkerStateSucceeded)
	}
	workers, err := fixture.service.GetRunWorkers(fixture.run.ID)
	if err != nil || len(workers) != 2 {
		t.Fatalf("workers = %#v, err=%v", workers, err)
	}
	fixture.runner.mu.Lock()
	if len(fixture.runner.specs) != 2 {
		t.Fatalf("runner process count = %d, want 2", len(fixture.runner.specs))
	}
	for remote, counts := range fixture.runner.statCallsAtStart {
		if len(counts) != 1 || counts[0] != 0 {
			t.Fatalf("fresh worker %s remote stats before copy = %#v, want [0]", remote, counts)
		}
	}
	got := make([]string, 0)
	for _, manifest := range fixture.runner.manifests {
		got = append(got, manifest...)
	}
	fixture.runner.mu.Unlock()
	sort.Strings(got)
	want := []string{"a.txt", "c.txt", "nested/b.txt"}
	sort.Strings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("assigned paths = %#v, want %#v", got, want)
	}
	var reservations, batches int64
	_ = fixture.db.Model(&models.QuotaReservation{}).Count(&reservations)
	_ = fixture.db.Model(&models.RotationQuotaBatch{}).Count(&batches)
	if reservations != 0 || batches != 0 {
		t.Fatalf("manual copy touched legacy state: reservations=%d batches=%d", reservations, batches)
	}
	var attempts int64
	if err := fixture.db.Model(&ManualWorkerAttempt{}).Where("run_id = ?", fixture.run.ID).Count(&attempts).Error; err != nil || attempts != 2 {
		t.Fatalf("attempt count = %d, err=%v", attempts, err)
	}
}

func TestManualCopyConcurrentStartCreatesOneWorkerSet(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a", "b.txt": "bb"})
	request := StartRequest{RunID: fixture.run.ID, ExpectedRunID: &fixture.run.ID, ExpectedRevision: fixture.run.Revision, ExpectedConfigRevision: fixture.run.ManualConfigRevision, IdempotencyKey: "concurrent-start", ActorIdentity: "operator", ActorType: "admin_session"}
	results := make(chan StartResult, 2)
	errorsCh := make(chan error, 2)
	var group sync.WaitGroup
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := fixture.service.StartRun(context.Background(), request)
			results <- result
			errorsCh <- err
		}()
	}
	group.Wait()
	close(results)
	close(errorsCh)
	var first StartResult
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	for result := range results {
		if len(result.WorkerIDs) != 2 {
			t.Fatalf("concurrent start worker ids = %#v", result.WorkerIDs)
		}
		if first.Run.ID == 0 {
			first = result
		}
	}
	workers, err := fixture.service.GetRunWorkers(fixture.run.ID)
	if err != nil || len(workers) != 2 {
		t.Fatalf("concurrent worker rows = %d, err=%v", len(workers), err)
	}
	for _, worker := range workers {
		waitWorkerState(t, fixture.service, worker.ID, ManualWorkerStateSucceeded)
	}
	fixture.runner.mu.Lock()
	started := len(fixture.runner.specs)
	fixture.runner.mu.Unlock()
	if started != 2 {
		t.Fatalf("concurrent start launched %d processes, want 2", started)
	}
}

func TestManualCopyWorkerRetryUsesOnlyIncompleteFilesAndRedactsLogs(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a", "b.txt": "bb", "c.txt": "ccc", "d.txt": "dddd"})
	fixture.runner.failFirst = true
	result := startFixture(t, fixture, "retry-start")
	workerID := waitAnyWorkerState(t, fixture.service, result.WorkerIDs, ManualWorkerStateFailed)
	failedDetail, err := fixture.service.GetWorker(workerID)
	if err != nil {
		t.Fatal(err)
	}
	incomplete := ""
	for _, file := range failedDetail.Files {
		if file.State != ManualWorkerFileStateVerified {
			incomplete = file.RelativePath
			break
		}
	}
	if incomplete == "" {
		t.Fatal("failed worker had no incomplete file")
	}
	detail, err := fixture.service.RetryWorker(context.Background(), workerID, "operator", "admin_session")
	if err != nil {
		t.Fatal(err)
	}
	waitWorkerState(t, fixture.service, workerID, ManualWorkerStateSucceeded)
	fixture.runner.mu.Lock()
	manifests := append([][]string(nil), fixture.runner.manifests...)
	fixture.runner.mu.Unlock()
	if len(manifests) < 2 {
		t.Fatalf("retry manifest = %#v, want a second attempt", manifests)
	}
	fixture.runner.mu.Lock()
	preflightCounts := append([]int(nil), fixture.runner.statCallsAtStart[failedDetail.Worker.RemoteName]...)
	fixture.runner.mu.Unlock()
	if len(preflightCounts) < 2 || preflightCounts[0] != 0 || preflightCounts[1] != len(failedDetail.Files) {
		t.Fatalf("retry remote stats before copy = %#v, want only %d post-run checks", preflightCounts, len(failedDetail.Files))
	}
	last := manifests[len(manifests)-1]
	if len(last) != 1 || last[0] != incomplete {
		t.Fatalf("retry manifest = %#v, want only incomplete %q", manifests, incomplete)
	}
	page, err := fixture.service.GetWorkerLogs(workerID, 0, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page.Data, "super-secret") || !strings.Contains(page.Data, "[redacted]") {
		t.Fatalf("worker log was not redacted: %q", page.Data)
	}
	if detail.Worker.AttemptNumber != 2 {
		t.Fatalf("attempt number = %d, want 2", detail.Worker.AttemptNumber)
	}
}

func waitAnyWorkerState(t *testing.T, service *Service, workerIDs []uint, want string) uint {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, workerID := range workerIDs {
			detail, err := service.GetWorker(workerID)
			if err != nil {
				t.Fatal(err)
			}
			if detail.Worker.State == want {
				return workerID
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("none of workers %#v reached %q", workerIDs, want)
	return 0
}

func TestManualCopyWorkerCancelRaceAndRestartNoAutoStart(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a"})
	fixture.runner.started = make(chan struct{})
	result := startFixture(t, fixture, "cancel-start")
	select {
	case <-fixture.runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("copy process did not start")
	}
	workerID := result.WorkerIDs[0]
	if _, err := fixture.service.CancelWorker(context.Background(), workerID, "operator", "admin_session"); err != nil {
		t.Fatal(err)
	}
	waitWorkerState(t, fixture.service, workerID, ManualWorkerStateCancelled)

	var activeWorker ManualRunWorker
	if err := fixture.db.Where("run_id = ?", fixture.run.ID).First(&activeWorker).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&activeWorker).Updates(map[string]interface{}{"state": ManualWorkerStateRunning, "lease_token": "restart-lease", "process_id": 12345, "process_start_token": "12345:1"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&ManualWorkerAttempt{}).Where("id = ?", activeWorker.CurrentAttemptID).Updates(map[string]interface{}{"state": ManualWorkerStateRunning, "lease_token": "restart-lease", "process_id": 12345, "process_start_token": "12345:1"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&ManualWorkerFile{}).Where("worker_id = ?", activeWorker.ID).Update("state", ManualWorkerFileStatePending).Error; err != nil {
		t.Fatal(err)
	}
	fixture.runner.mu.Lock()
	statsBeforeRecovery := fixture.runner.statCalls[activeWorker.RemoteName]
	fixture.runner.mu.Unlock()
	restarted := NewService(fixture.db)
	restarted.Runner = fixture.runner
	if err := restarted.RecoverWorkers(); err != nil {
		t.Fatal(err)
	}
	state, err := restarted.GetWorker(activeWorker.ID)
	if err != nil || state.Worker.State != ManualWorkerStateCancelled {
		t.Fatalf("restart recovery = %#v, err=%v", state.Worker, err)
	}
	fixture.runner.mu.Lock()
	started := len(fixture.runner.specs)
	statsAfterRecovery := fixture.runner.statCalls[activeWorker.RemoteName]
	fixture.runner.mu.Unlock()
	if started != 1 {
		t.Fatalf("restart recovery auto-started a process: %d", started)
	}
	if statsAfterRecovery != statsBeforeRecovery {
		t.Fatalf("cancelled restart recovery performed remote stats: before=%d after=%d", statsBeforeRecovery, statsAfterRecovery)
	}
}

func TestManualRunStopInterruptsPostRunRemoteVerification(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a"})
	fixture.runner.statStarted = make(chan struct{})
	result := startFixture(t, fixture, "cancel-post-run-verification")
	if len(result.WorkerIDs) != 1 {
		t.Fatalf("worker IDs = %#v, want one worker", result.WorkerIDs)
	}
	select {
	case <-fixture.runner.statStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("post-run remote verification did not start")
	}
	run, err := fixture.service.GetRun(fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := fixture.service.StopRun(context.Background(), SettlementRequest{RunID: run.ID, ExpectedRevision: run.Revision, IdempotencyKey: "stop-post-run-verification", ActorIdentity: "operator", ActorType: "admin_session"})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Run.SettlementState != ManualSettlementStateStopped {
		t.Fatalf("settlement state = %q, want stopped", stopped.Run.SettlementState)
	}
	waitWorkerState(t, fixture.service, result.WorkerIDs[0], ManualWorkerStateCancelled)
}

func TestManualWorkerRecoverySkipsRemoteScanBeforeManifest(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a"})
	worker := ManualRunWorker{RunID: fixture.run.ID, AccountID: fixture.accounts[0].AccountID, AccountPosition: fixture.accounts[0].Position, AccountIdentity: fixture.accounts[0].AccountIdentity, RemoteName: fixture.accounts[0].RemoteName, ConfigIdentity: fixture.accounts[0].ConfigIdentity, State: ManualWorkerStateStarting, AttemptNumber: 1, Revision: 1, AssignedCount: 1, AssignedBytes: 1, LeaseToken: strings.Repeat("a", 48)}
	if err := fixture.db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	attempt := ManualWorkerAttempt{RunID: fixture.run.ID, WorkerID: worker.ID, AttemptNumber: 1, State: ManualWorkerStateStarting, LeaseToken: worker.LeaseToken, AssignedCount: 1, AssignedBytes: 1}
	if err := fixture.db.Create(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&worker).Update("current_attempt_id", attempt.ID).Error; err != nil {
		t.Fatal(err)
	}
	file := fixture.files[0]
	if err := fixture.db.Create(&ManualWorkerFile{RunID: fixture.run.ID, WorkerID: worker.ID, AttemptID: attempt.ID, RelativePath: file.RelativePath, SnapshotKey: file.SnapshotKey, SizeBytes: file.SizeBytes, MtimeNS: file.MtimeNS, Device: file.Device, Inode: file.Inode, State: ManualWorkerFileStatePending}).Error; err != nil {
		t.Fatal(err)
	}

	restarted := NewService(fixture.db)
	restarted.Runner = fixture.runner
	if err := restarted.RecoverWorkers(); err != nil {
		t.Fatal(err)
	}
	detail, err := restarted.GetWorker(worker.ID)
	if err != nil || detail.Worker.State != ManualWorkerStateNeedsAttention {
		t.Fatalf("restart recovery = %#v, err=%v", detail.Worker, err)
	}
	fixture.runner.mu.Lock()
	stats := fixture.runner.statCalls[worker.RemoteName]
	fixture.runner.mu.Unlock()
	if stats != 0 {
		t.Fatalf("pre-manifest restart performed %d remote stats", stats)
	}
}

func TestManualMoveWorkersHappyPathIsolatedPerAccount(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeMove, map[string]string{"a.txt": "a", "nested/b.txt": "bb", "c.txt": "ccc"})
	result := startFixture(t, fixture, "move-start")
	for _, workerID := range result.WorkerIDs {
		waitWorkerState(t, fixture.service, workerID, ManualWorkerStateSucceeded)
	}
	fixture.runner.mu.Lock()
	moveSpecs := append([]proactive.MoveSpec(nil), fixture.runner.moveSpecs...)
	fixture.runner.mu.Unlock()
	if len(moveSpecs) != 2 {
		t.Fatalf("move process count = %d, want 2", len(moveSpecs))
	}
	remotes := map[string]bool{}
	for _, spec := range moveSpecs {
		remotes[spec.DestinationRemote] = true
	}
	if len(remotes) != 2 || !remotes["remote-a"] || !remotes["remote-b"] {
		t.Fatalf("move workers were not isolated per account: %#v", moveSpecs)
	}
	for _, relative := range []string{"a.txt", "nested/b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(fixture.root, filepath.FromSlash(relative))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("moved source remains: %s err=%v", relative, err)
		}
	}
	var run ManualTransferRun
	if err := fixture.db.First(&run, fixture.run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if run.State != ManualRunStateSucceeded {
		t.Fatalf("move run state = %q, want succeeded", run.State)
	}
}

func TestManualMoveStartFailureRestoresSourceBeforeTerminalizing(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeMove, map[string]string{"a.txt": "a", "b.txt": "bb"})
	fixture.runner.startErr = errors.New("move process start failed")
	result := startFixture(t, fixture, "move-start-failure")
	for _, workerID := range result.WorkerIDs {
		waitWorkerState(t, fixture.service, workerID, ManualWorkerStateFailed)
	}
	for _, relative := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(fixture.root, filepath.FromSlash(relative))); err != nil {
			t.Fatalf("pre-start failure did not restore %s: %v", relative, err)
		}
	}
}

func TestManualMoveAmbiguousHandoffTerminalizesNeedsAttention(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeMove, map[string]string{"a.txt": "a"})
	fixture.runner.movePartialThenError = true
	result := startFixture(t, fixture, "move-ambiguous")
	worker := waitWorkerState(t, fixture.service, result.WorkerIDs[0], ManualWorkerStateNeedsAttention)
	if !strings.Contains(worker.LastError, "ambiguous") {
		t.Fatalf("ambiguous move error = %q", worker.LastError)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ambiguous move unexpectedly restored or retained source: %v", err)
	}
	if _, err := fixture.service.RetryWorker(context.Background(), result.WorkerIDs[0], "operator", "admin_session"); !errors.Is(err, ErrManualMoveUnsupported) {
		t.Fatalf("ambiguous move retry = %v, want explicit manual-resolution guard", err)
	}
}

func TestManualCopyStartRejectsStaleTaskAndAccountFence(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*manualWorkerFixture)
	}{
		{name: "source", mutate: func(f *manualWorkerFixture) {
			_ = f.db.Model(&models.Task{}).Where("id = ?", 901).Update("source_dir", filepath.Join(f.root, "changed")).Error
		}},
		{name: "mode", mutate: func(f *manualWorkerFixture) {
			_ = f.db.Model(&models.Task{}).Where("id = ?", 901).Update("transfer_mode", models.TransferModeMove).Error
		}},
		{name: "config revision", mutate: func(f *manualWorkerFixture) {
			_ = f.db.Model(&models.Task{}).Where("id = ?", 901).Update("manual_account_revision", 2).Error
		}},
		{name: "disabled account", mutate: func(f *manualWorkerFixture) {
			_ = f.db.Model(&models.QuotaAccount{}).Where("id = ?", 1).Update("enabled", false).Error
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a"})
			testCase.mutate(&fixture)
			run := fixture.run
			_, err := fixture.service.StartRun(context.Background(), StartRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: "stale-" + testCase.name})
			if !errors.Is(err, ErrRevisionConflict) {
				t.Fatalf("stale start error = %v", err)
			}
		})
	}
}

func TestManualWorkerCancelRetryFenceUsesTaskID(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c", "d.txt": "d"})
	fixture.runner.failFirst = true
	started := startFixture(t, fixture, "task-id-fence")
	workerID := waitAnyWorkerState(t, fixture.service, started.WorkerIDs, ManualWorkerStateFailed)
	fence := &manualWorkerTaskFence{database: fixture.db}
	fixture.service.TaskFence = fence
	if _, err := fixture.service.CancelWorker(context.Background(), workerID, "operator", "admin_session"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.RetryWorker(context.Background(), workerID, "operator", "admin_session"); err != nil {
		t.Fatal(err)
	}
	fence.mu.Lock()
	ids := append([]uint(nil), fence.ids...)
	fence.mu.Unlock()
	if len(ids) != 2 || ids[0] != 901 || ids[1] != 901 {
		t.Fatalf("worker fence ids = %#v, want task id 901 for cancel/retry", ids)
	}
}

func TestManualWorkerStageBaseIsBootstrapped(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a"})
	fixture.service.StageDir = filepath.Join(t.TempDir(), "missing-stage-base")
	result := startFixture(t, fixture, "stage-bootstrap")
	for _, workerID := range result.WorkerIDs {
		waitWorkerState(t, fixture.service, workerID, ManualWorkerStateSucceeded)
	}
	if info, err := os.Stat(fixture.service.StageDir); err != nil || !info.IsDir() {
		t.Fatalf("stage base was not created: info=%v err=%v", info, err)
	}
}

func TestManualWorkerStartupAndHeartbeatStopVerifiedProcessBeforeClear(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a"})
	fixture.runner.started = make(chan struct{})
	fixture.service.LeaseRenewInterval = time.Millisecond
	inspector := &manualWorkerInspector{status: proactive.ProcessStatus{Confirmed: true, Alive: true}}
	fixture.service.Inspector = inspector
	started := startFixture(t, fixture, "heartbeat-recovery")
	workerID := started.WorkerIDs[0]
	select {
	case <-fixture.runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("copy process did not start")
	}
	var worker ManualRunWorker
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := fixture.db.First(&worker, workerID).Error; err != nil {
			t.Fatal(err)
		}
		if worker.State == ManualWorkerStateRunning && worker.ProcessID > 0 && worker.ProcessStartToken != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if worker.State != ManualWorkerStateRunning || worker.ProcessID <= 0 || worker.ProcessStartToken == "" {
		t.Fatalf("copy process identity was not persisted: %#v", worker)
	}
	if err := fixture.db.Model(&ManualRunWorker{}).Where("id = ?", workerID).Update("lease_token", "lost-lease").Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&ManualWorkerAttempt{}).Where("id = ?", worker.CurrentAttemptID).Update("lease_token", "lost-lease").Error; err != nil {
		t.Fatal(err)
	}
	waitWorkerState(t, fixture.service, workerID, ManualWorkerStateNeedsAttention)
	inspector.mu.Lock()
	stops, inspects := inspector.stops, inspector.inspects
	inspector.mu.Unlock()
	if stops == 0 || inspects == 0 {
		t.Fatalf("heartbeat recovery did not inspect/stop process: stops=%d inspects=%d", stops, inspects)
	}
	if err := fixture.db.First(&worker, workerID).Error; err != nil {
		t.Fatal(err)
	}
	if worker.ProcessID != 0 || worker.ProcessStartToken != "" || worker.LeaseToken != "" {
		t.Fatalf("durable process ownership was not cleared after stop: %#v", worker)
	}
}

func TestManualWorkerLiveStopFailureKeepsDurableOwnershipAndExposesCancel(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a"})
	fixture.runner.started = make(chan struct{})
	inspector := &manualWorkerInspector{status: proactive.ProcessStatus{Confirmed: true, Alive: true}, stopErr: errors.New("injected live stop failure")}
	fixture.service.Inspector = inspector
	started := startFixture(t, fixture, "live-stop-failure")
	workerID := started.WorkerIDs[0]
	select {
	case <-fixture.runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("copy process did not start")
	}
	var before ManualRunWorker
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := fixture.db.First(&before, workerID).Error; err != nil {
			t.Fatal(err)
		}
		if before.State == ManualWorkerStateRunning && before.ProcessID > 0 && before.ProcessStartToken != "" && before.LeaseToken != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if before.State != ManualWorkerStateRunning || before.ProcessID <= 0 || before.ProcessStartToken == "" || before.LeaseToken == "" {
		t.Fatalf("worker ownership was not persisted: %#v", before)
	}
	if _, err := fixture.service.CancelWorker(context.Background(), workerID, "operator", "admin_session"); err == nil {
		t.Fatal("live stop failure was reported as a successful cancellation")
	}
	worker := waitWorkerState(t, fixture.service, workerID, ManualWorkerStateNeedsAttention)
	if worker.ProcessID != before.ProcessID || worker.ProcessStartToken != before.ProcessStartToken || worker.LeaseToken != before.LeaseToken || !strings.Contains(worker.LastError, "injected live stop failure") {
		t.Fatalf("live stop failure cleared ownership: before=%#v after=%#v", before, worker)
	}
	if worker.Actionability != "cancel" {
		t.Fatalf("live stop failure actionability = %q, want cancel", worker.Actionability)
	}

	inspector.mu.Lock()
	inspector.stopErr = nil
	inspector.status = proactive.ProcessStatus{Confirmed: true, Alive: false}
	inspector.mu.Unlock()
	fixture.runner.mu.Lock()
	for _, process := range fixture.runner.processes {
		process.release()
	}
	fixture.runner.mu.Unlock()
}

func TestManualWorkerPendingStartReplayAndNeedsAttentionRetryAreExplicit(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a"})
	run := fixture.run
	request := StartRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: "pending-replay"}
	created, err := fixture.service.startRunUnderFence(request)
	if err != nil || created.Existing {
		t.Fatalf("durable pending start = %#v err=%v", created, err)
	}
	replay, err := fixture.service.StartRun(context.Background(), request)
	if err != nil || !replay.Existing {
		t.Fatalf("pending start replay = %#v err=%v", replay, err)
	}
	waitWorkerState(t, fixture.service, replay.WorkerIDs[0], ManualWorkerStateSucceeded)

	var worker ManualRunWorker
	if err := fixture.db.First(&worker, replay.WorkerIDs[0]).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&ManualRunWorker{}).Where("id = ?", worker.ID).Updates(map[string]interface{}{"state": ManualWorkerStateRunning, "lease_token": "restart-lease"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&ManualTransferRun{}).Where("id = ?", fixture.run.ID).Updates(map[string]interface{}{"state": ManualRunStateRunning, "settlement_state": ManualSettlementStateActive, "settlement_finished_at": nil}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&ManualWorkerAttempt{}).Where("id = ?", worker.CurrentAttemptID).Updates(map[string]interface{}{"state": ManualWorkerStateRunning, "lease_token": "restart-lease"}).Error; err != nil {
		t.Fatal(err)
	}
	fixture.runner.mu.Lock()
	fixture.runner.remote = map[string]bool{}
	fixture.runner.mu.Unlock()
	if err := fixture.db.Model(&ManualWorkerFile{}).Where("worker_id = ?", worker.ID).Update("state", ManualWorkerFileStatePending).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.RecoverWorkers(); err != nil {
		t.Fatal(err)
	}
	detail, err := fixture.service.GetWorker(worker.ID)
	if err != nil || detail.Worker.State != ManualWorkerStateNeedsAttention {
		t.Fatalf("startup recovery state = %#v err=%v", detail.Worker, err)
	}
	if _, err := fixture.service.RetryWorker(context.Background(), worker.ID, "operator", "admin_session"); err != nil {
		t.Fatalf("explicit needs-attention retry failed: %v", err)
	}
	waitWorkerState(t, fixture.service, worker.ID, ManualWorkerStateSucceeded)
}

func TestManualWorkerStreamsLogsAndProgressBeforeCompletion(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a", "b.txt": "bb"})
	fixture.runner.streamed = true
	fixture.runner.started = make(chan struct{})
	result := startFixture(t, fixture, "streamed-output")
	select {
	case <-fixture.runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("copy process did not start")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		page, err := fixture.service.GetWorkerLogs(result.WorkerIDs[0], 0, 64<<10)
		if err == nil && strings.Contains(page.Data, "[redacted]") {
			detail, detailErr := fixture.service.GetWorker(result.WorkerIDs[0])
			if detailErr != nil {
				t.Fatal(detailErr)
			}
			if detail.Worker.ProgressPercent == 50 && detail.Worker.SpeedBytesPerSecond == 42 && detail.Worker.CurrentRelativePath == "a.txt" {
				fixture.runner.mu.Lock()
				for _, process := range fixture.runner.processes {
					process.release()
				}
				fixture.runner.mu.Unlock()
				waitWorkerState(t, fixture.service, result.WorkerIDs[0], ManualWorkerStateSucceeded)
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("streamed log was not visible before process completion")
}

func TestManualMoveWorkerStreamsLogsBeforeCompletion(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeMove, map[string]string{"a.txt": "a"})
	fixture.runner.streamed = true
	fixture.runner.started = make(chan struct{})
	result := startFixture(t, fixture, "move-streamed-output")
	select {
	case <-fixture.runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("move process did not start")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		page, err := fixture.service.GetWorkerLogs(result.WorkerIDs[0], 0, 64<<10)
		if err == nil && strings.Contains(page.Data, "[redacted]") {
			fixture.runner.mu.Lock()
			for _, process := range fixture.runner.processes {
				process.release()
			}
			fixture.runner.mu.Unlock()
			waitWorkerState(t, fixture.service, result.WorkerIDs[0], ManualWorkerStateSucceeded)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("move worker log was not visible before process completion")
}

func TestManualWorkerRunStateTerminalMatrix(t *testing.T) {
	cases := []struct {
		left, right, want string
	}{
		{ManualWorkerStateSucceeded, ManualWorkerStateSucceeded, ManualRunStateSucceeded},
		{ManualWorkerStateSucceeded, ManualWorkerStateCancelled, ManualRunStateCancelled},
		{ManualWorkerStateCancelled, ManualWorkerStateCancelled, ManualRunStateCancelled},
		{ManualWorkerStateSucceeded, ManualWorkerStateFailed, ManualRunStateFailed},
		{ManualWorkerStateFailed, ManualWorkerStateCancelled, ManualRunStateFailed},
		{ManualWorkerStateNeedsAttention, ManualWorkerStateSucceeded, ManualRunStateNeedsAttention},
		{ManualWorkerStateRunning, ManualWorkerStateSucceeded, ManualRunStateRunning},
	}
	for _, testCase := range cases {
		t.Run(fmt.Sprintf("%s-%s", testCase.left, testCase.right), func(t *testing.T) {
			fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a"})
			for position, state := range []string{testCase.left, testCase.right} {
				if err := fixture.db.Create(&ManualRunWorker{RunID: fixture.run.ID, AccountID: uint(position + 1), AccountPosition: position, AccountIdentity: fmt.Sprintf("matrix-%d", position), ConfigIdentity: fixture.run.ConfigIdentity, RemoteName: fmt.Sprintf("remote-%d", position), State: state, AttemptNumber: 1, Revision: 1}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := fixture.db.Model(&ManualTransferRun{}).Where("id = ?", fixture.run.ID).Updates(map[string]interface{}{"state": ManualRunStateRunning, "revision": 3}).Error; err != nil {
				t.Fatal(err)
			}
			if err := fixture.service.deriveRunStateTx(fixture.db, fixture.run.ID); err != nil {
				t.Fatal(err)
			}
			var run ManualTransferRun
			if err := fixture.db.First(&run, fixture.run.ID).Error; err != nil {
				t.Fatal(err)
			}
			if run.State != testCase.want {
				t.Fatalf("derived state = %q, want %q", run.State, testCase.want)
			}
		})
	}
}

func TestManualWorkerLogOffsetsAndIsolation(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a", "b.txt": "b"})
	result := startFixture(t, fixture, "log-offsets")
	for _, workerID := range result.WorkerIDs {
		waitWorkerState(t, fixture.service, workerID, ManualWorkerStateSucceeded)
	}
	if err := fixture.service.appendWorkerLog(result.WorkerIDs[0], "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.appendWorkerLog(result.WorkerIDs[1], "beta"); err != nil {
		t.Fatal(err)
	}
	first, err := fixture.service.GetWorkerLogs(result.WorkerIDs[0], 0, 3)
	if err != nil || first.Data != "alp" || first.NextOffset != 3 || first.EOF {
		t.Fatalf("first log page = %#v err=%v", first, err)
	}
	second, err := fixture.service.GetWorkerLogs(result.WorkerIDs[0], first.NextOffset, 64<<10)
	if err != nil || !strings.Contains(second.Data, "ha") || strings.Contains(second.Data, "beta") {
		t.Fatalf("second log page = %#v err=%v", second, err)
	}
	other, err := fixture.service.GetWorkerLogs(result.WorkerIDs[1], 0, 64<<10)
	if err != nil || !strings.Contains(other.Data, "beta") || strings.Contains(other.Data, "alpha") {
		t.Fatalf("isolated worker log = %#v err=%v", other, err)
	}
	redacted := SanitizeMessage(`token=one token=two password: 'three' --secret four {"token":"five"}`)
	for _, secret := range []string{"one", "two", "three", "four", "five"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("repeated credential was not redacted: %q", redacted)
		}
	}
}

func TestWorkerLogRedactorHandlesEveryCredentialSplitBoundary(t *testing.T) {
	inputs := []string{
		"prefix token=split-secret suffix\n",
		"prefix password=split-secret suffix",
		`{"password":"split-secret"}` + "\n",
		"--credential split-secret\n",
	}
	for _, input := range inputs {
		for split := 0; split <= len(input); split++ {
			redactor := newWorkerLogRedactor()
			output := redactor.Feed(input[:split]) + redactor.Feed(input[split:]) + redactor.Flush()
			if strings.Contains(output, "split-secret") {
				t.Fatalf("split=%d leaked credential: %q", split, output)
			}
			if !strings.Contains(output, "[redacted]") {
				t.Fatalf("split=%d did not redact credential: %q", split, output)
			}
		}
	}
}

func TestWorkerProgressMergePreservesRichMetrics(t *testing.T) {
	state := newWorkerProgressState(ManualRunWorker{CompletedCount: 4, CompletedBytes: 400, SpeedBytesPerSecond: 40, ProgressPercent: 40, CurrentRelativePath: "rich.bin"})
	merged := state.merge(proactive.ProcessProgress{ProgressPercent: 50})
	if merged.CompletedCount != 4 || merged.CompletedBytes != 400 || merged.SpeedBytesPerSecond != 40 || merged.RelativePath != "rich.bin" || merged.ProgressPercent != 50 {
		t.Fatalf("percentage event overwrote rich progress: %#v", merged)
	}
}

func TestPendingUnknownNeedsAttentionRetryAndNoProcessCancel(t *testing.T) {
	for _, state := range []string{ManualWorkerStatePending, ManualWorkerStateUnknown, ManualWorkerStateNeedsAttention} {
		t.Run(state, func(t *testing.T) {
			fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a"})
			run := fixture.run
			request := StartRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: "recovery-" + state}
			created, err := fixture.service.startRunUnderFence(request)
			if err != nil {
				t.Fatal(err)
			}
			workerID := created.WorkerIDs[0]
			if err := fixture.db.Model(&ManualRunWorker{}).Where("id = ?", workerID).Update("state", state).Error; err != nil {
				t.Fatal(err)
			}
			if state != ManualWorkerStatePending {
				if err := fixture.db.Model(&ManualWorkerAttempt{}).Where("worker_id = ?", workerID).Update("state", state).Error; err != nil {
					t.Fatal(err)
				}
			}
			if state == ManualWorkerStatePending {
				if _, err := fixture.service.CancelWorker(context.Background(), workerID, "operator", "admin_session"); err != nil {
					t.Fatal(err)
				}
				detail, err := fixture.service.GetWorker(workerID)
				if err != nil || detail.Worker.State != ManualWorkerStateCancelled {
					t.Fatalf("no-process cancel = %#v err=%v", detail.Worker, err)
				}
				return
			}
			if _, err := fixture.service.RetryWorker(context.Background(), workerID, "operator", "admin_session"); err != nil {
				t.Fatal(err)
			}
			waitWorkerState(t, fixture.service, workerID, ManualWorkerStateSucceeded)
		})
	}
}

func TestManualWorkerRedactorTailAppendFailureDoesNotSucceed(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a"})
	fixture.runner.streamed = true
	fixture.runner.streamedOutput = "prefix-tail"
	var appends []string
	fixture.service.appendWorkerLogHook = func(_ uint, value string) error {
		appends = append(appends, value)
		if len(appends) == 2 {
			return errors.New("injected tail append failure")
		}
		return nil
	}
	result := startFixture(t, fixture, "tail-append-failure")
	worker := waitWorkerState(t, fixture.service, result.WorkerIDs[0], ManualWorkerStateNeedsAttention)
	if worker.State == ManualWorkerStateSucceeded || !strings.Contains(worker.LastError, "injected tail append failure") {
		t.Fatalf("tail append failure was not durable: %#v", worker)
	}
	var event ManualWorkerEvent
	if err := fixture.db.Where("worker_id = ? AND event_type = ?", worker.ID, ManualWorkerEventFinished).Order("id DESC").First(&event).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(event.Details, "injected tail append failure") {
		t.Fatalf("tail append failure was not audited: %#v", event)
	}
}

func TestManualWorkerStartPersistenceFailureDrainsStreamsAndJoinsErrors(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a"})
	fixture.runner.streamed = true
	fixture.service.PersistWorkerProcessFunc = func(uint, uint, string, proactive.ProcessHandle) error {
		return errors.New("injected process identity persistence failure")
	}
	fixture.service.PersistWorkerProgressFunc = func(uint, uint, proactive.ProcessProgress) error {
		return errors.New("injected progress persistence failure")
	}
	fixture.service.appendWorkerLogHook = func(_ uint, _ string) error {
		return errors.New("injected stream log persistence failure")
	}
	result := startFixture(t, fixture, "start-persistence-failure")
	worker := waitWorkerState(t, fixture.service, result.WorkerIDs[0], ManualWorkerStateUnknown)
	for _, message := range []string{"injected process identity persistence failure", "injected progress persistence failure", "injected stream log persistence failure"} {
		if !strings.Contains(worker.LastError, message) {
			t.Fatalf("worker error omitted %q: %s", message, worker.LastError)
		}
	}
	var event ManualWorkerEvent
	if err := fixture.db.Where("worker_id = ? AND event_type = ?", worker.ID, ManualWorkerEventFinished).Order("id DESC").First(&event).Error; err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{"injected process identity persistence failure", "injected progress persistence failure", "injected stream log persistence failure"} {
		if !strings.Contains(event.Details, message) {
			t.Fatalf("worker audit omitted %q: %s", message, event.Details)
		}
	}
}

func TestManualWorkerFinalResultAppendFailureDoesNotSucceed(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a", "b.txt": "b"})
	fixture.runner.resultStderr = "final result output"
	fixture.service.appendWorkerLogHook = func(_ uint, _ string) error {
		return errors.New("injected final result append failure")
	}
	result := startFixture(t, fixture, "final-result-append-failure")
	worker := waitWorkerState(t, fixture.service, result.WorkerIDs[0], ManualWorkerStateNeedsAttention)
	if worker.State == ManualWorkerStateSucceeded || !strings.Contains(worker.LastError, "injected final result append failure") {
		t.Fatalf("final result append failure was not durable: %#v", worker)
	}
	var event ManualWorkerEvent
	if err := fixture.db.Where("worker_id = ? AND event_type = ?", worker.ID, ManualWorkerEventFinished).Order("id DESC").First(&event).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(event.Details, "injected final result append failure") {
		t.Fatalf("final result append failure was not audited: %#v", event)
	}
}

func TestManualWorkerUnknownLogReturnsEOFAfterCurrentBytes(t *testing.T) {
	fixture := newManualWorkerFixture(t, models.TransferModeCopy, map[string]string{"a.txt": "a"})
	created, err := fixture.service.startRunUnderFence(StartRequest{RunID: fixture.run.ID, ExpectedRunID: &fixture.run.ID, ExpectedRevision: fixture.run.Revision, ExpectedConfigRevision: fixture.run.ManualConfigRevision, IdempotencyKey: "unknown-log-eof"})
	if err != nil {
		t.Fatal(err)
	}
	workerID := created.WorkerIDs[0]
	if err := fixture.db.Model(&ManualRunWorker{}).Where("id = ?", workerID).Update("state", ManualWorkerStateUnknown).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.appendWorkerLog(workerID, "current bytes"); err != nil {
		t.Fatal(err)
	}
	page, err := fixture.service.GetWorkerLogs(workerID, 0, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Data != "current bytes\n" || !page.EOF || page.NextOffset != int64(len(page.Data)) {
		t.Fatalf("unknown worker log page = %#v", page)
	}
	detail, err := fixture.service.GetWorker(workerID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Worker.Actionability != "retry" {
		t.Fatalf("unknown worker actionability = %q, want retry", detail.Worker.Actionability)
	}
}
