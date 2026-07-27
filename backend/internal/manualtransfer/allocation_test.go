package manualtransfer

import (
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
	"rclone-manager/internal/quota"
)

func TestAllocationVersionOneCapsAndOrder(t *testing.T) {
	tests := []struct {
		name           string
		accountCount   int
		files          []allocationTestFile
		wantAssigned   int64
		wantBytes      int64
		wantUnassigned int64
	}{
		{name: "100GB one account", accountCount: 1, files: []allocationTestFile{{"one", 100_000_000_000}}, wantAssigned: 1, wantBytes: 100_000_000_000},
		{name: "900GB two accounts", accountCount: 2, files: []allocationTestFile{{"one", 600_000_000_000}, {"two", 300_000_000_000}}, wantAssigned: 2, wantBytes: 900_000_000_000},
		{name: "three accounts reach 2.1TB", accountCount: 3, files: []allocationTestFile{{"a", 700_000_000_000}, {"b", 700_000_000_000}, {"c", 700_000_000_000}}, wantAssigned: 3, wantBytes: 2_100_000_000_000},
		{name: "3TB four accounts capped at 2.4TB", accountCount: 4, files: []allocationTestFile{{"a", 600_000_000_000}, {"b", 600_000_000_000}, {"c", 600_000_000_000}, {"d", 600_000_000_000}, {"e", 600_000_000_000}}, wantAssigned: 4, wantBytes: 2_400_000_000_000, wantUnassigned: 1},
		{name: "five plus accounts never exceed run cap", accountCount: 6, files: []allocationTestFile{{"a", 700_000_000_000}, {"b", 700_000_000_000}, {"c", 700_000_000_000}, {"d", 700_000_000_000}, {"e", 700_000_000_000}}, wantAssigned: 3, wantBytes: 2_100_000_000_000, wantUnassigned: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			database := manualTestDB(t)
			run := seedAllocationRun(t, database, tc.accountCount, tc.files)
			service := NewService(database)
			service.Start()
			defer service.Stop()
			result, err := service.CreateAllocate(AllocateRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: "allocate-" + tc.name, ActorIdentity: "test", ActorType: "admin_session"})
			if err != nil {
				t.Fatal(err)
			}
			allocated := waitManualRun(t, service, result.Run.ID, ManualRunStateAllocated)
			if allocated.AssignedCount != tc.wantAssigned || allocated.AssignedBytes != tc.wantBytes || allocated.UnassignedCount != tc.wantUnassigned {
				t.Fatalf("allocation totals = assigned %d/%d unassigned %d, want %d/%d/%d", allocated.AssignedCount, allocated.AssignedBytes, allocated.UnassignedCount, tc.wantAssigned, tc.wantBytes, tc.wantUnassigned)
			}
			rows, err := service.ListAllocationFiles(run.ID, "", 500, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if len(rows.Files) != len(tc.files) {
				t.Fatalf("allocation rows = %d, want %d", len(rows.Files), len(tc.files))
			}
		})
	}
}

func TestAllocationOversizeZeroBytesStableDigestAndNoDuplicates(t *testing.T) {
	database := manualTestDB(t)
	files := []allocationTestFile{{"z", 10}, {"zero", 0}, {"oversize", PerAccountBudgetBytes + 1}, {"a/nested", 20}}
	run := seedAllocationRun(t, database, 2, files)
	service := NewService(database)
	service.Start()
	defer service.Stop()
	first, err := service.CreateAllocate(AllocateRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: "stable", ActorIdentity: "test", ActorType: "admin_session"})
	if err != nil {
		t.Fatal(err)
	}
	allocated := waitManualRun(t, service, first.Run.ID, ManualRunStateAllocated)
	page, err := service.ListAllocationFiles(run.ID, "", 500, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if page.Files[0].RelativePath != "a/nested" || page.Files[0].AccountPosition == nil || *page.Files[0].AccountPosition != 0 {
		t.Fatalf("canonical allocation order/assignment = %#v", page.Files)
	}
	var oversize, zero ManualRunFile
	for _, file := range page.Files {
		if file.RelativePath == "oversize" {
			oversize = file
		}
		if file.RelativePath == "zero" {
			zero = file
		}
	}
	if oversize.UnassignedReason != ManualAllocationReasonOversize || zero.AccountPosition == nil || *zero.AccountPosition != 0 {
		t.Fatalf("oversize/zero assignment = %#v / %#v", oversize, zero)
	}
	if allocated.AllocationDigest == "" {
		t.Fatal("allocation digest is empty")
	}
	replay, err := service.CreateAllocate(AllocateRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: "stable", ActorIdentity: "test", ActorType: "admin_session"})
	if err != nil || !replay.Existing || replay.Run.ID != run.ID || replay.Run.AllocationDigest != allocated.AllocationDigest {
		t.Fatalf("allocation replay = %#v, err=%v", replay, err)
	}
	changed := AllocateRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: "stable", ActorIdentity: "test", ActorType: "admin_session"}
	changed.ExpectedRevision++
	if _, err := service.CreateAllocate(changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed allocation request error = %v", err)
	}
}

func TestAllocationInactiveRowsAndStaleRevisionAreHidden(t *testing.T) {
	database := manualTestDB(t)
	run := seedAllocationRun(t, database, 1, []allocationTestFile{{"visible", 10}})
	if err := database.Create(&ManualRunAllocation{RunID: run.ID, Generation: 9, RelativePath: "hidden", SnapshotKey: "hidden", SizeBytes: 1, AccountPosition: intPointer(0)}).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(database)
	if _, err := service.ListAllocationFiles(run.ID, "", 10, "", ""); !errors.Is(err, ErrAllocationUnavailable) {
		t.Fatalf("inactive allocation was exposed: %v", err)
	}
	if _, err := service.CreateAllocate(AllocateRequest{RunID: run.ID, ExpectedRunID: &run.ID, ExpectedRevision: run.Revision + 1, ExpectedConfigRevision: run.ManualConfigRevision, IdempotencyKey: "stale", ActorIdentity: "test", ActorType: "admin_session"}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale allocation revision error = %v", err)
	}
	var reservations int64
	if err := database.Model(&models.QuotaReservation{}).Count(&reservations).Error; err != nil {
		t.Fatal(err)
	}
	if reservations != 0 {
		t.Fatalf("allocation created quota reservations: %d", reservations)
	}
}

type allocationTestFile struct {
	path string
	size int64
}

func seedAllocationRun(t *testing.T, database *gorm.DB, accountCount int, files []allocationTestFile) ManualTransferRun {
	t.Helper()
	root := t.TempDir()
	identity, err := quota.OpenSourceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	run := ManualTransferRun{TaskID: uint(accountCount + 100), State: ManualRunStateAnalyzed, Revision: 2, ManualInputRevision: 1, ManualConfigRevision: 1, ActorIdentity: "test", ActorType: "admin_session", SourcePath: root, DestinationPath: "/dest", TransferMode: models.TransferModeCopy, ConfigIdentity: filepath.Join(root, "config"), FrozenInput: "{}", IdempotencyKey: "seed-" + time.Now().Format("150405.000000000"), RequestFingerprint: "seed", SourceRootDevice: identity.Device, SourceRootInode: identity.Inode, SnapshotGeneration: 1, SnapshotCount: int64(len(files))}
	_ = identity.Close()
	if err := database.Create(&models.Task{ID: run.TaskID, Name: "manual", TaskType: models.TaskTypeManual, ManualStrategy: models.ManualStrategyAllocation, SourceType: "local", SourceDir: root, DestType: "remote", RemoteDir: "/dest", TransferMode: models.TransferModeCopy, Enabled: true, WatchEnabled: false, ScheduleEnabled: false, QBEnabled: false}).Error; err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		run.SnapshotBytes += file.size
	}
	if err := database.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	for position := 0; position < accountCount; position++ {
		if err := database.Create(&ManualRunAccount{RunID: run.ID, Position: position, AccountID: uint(position + 1), AccountIdentity: "account-" + strconv.Itoa(position+1), RemoteName: "remote-" + strconv.Itoa(position+1), ConfigIdentity: filepath.Join(root, "config")}).Error; err != nil {
			t.Fatal(err)
		}
	}
	activated := time.Now().UTC()
	for index, file := range files {
		if err := database.Create(&ManualRunFile{RunID: run.ID, Generation: 1, RelativePath: file.path, SnapshotKey: "snapshot-" + strconv.Itoa(index), SizeBytes: file.size, Device: 1, Inode: int64(index + 1), ActivatedAt: &activated}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return run
}

func intPointer(value int) *int { return &value }
