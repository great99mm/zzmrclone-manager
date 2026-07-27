package manualtransfer

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestEnsureSchemaRepairsFirstPhaseIndexesAndReconcilesDuplicates(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migration.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.AutoMigrate(&ManualTransferRun{}, &ManualRunFile{}, &ManualRunEvent{}); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{ManualRunIdempotencyIndex, ManualRunFilesRunPathIndex, ManualRunFilesPathUniqueIndex, ManualRunFilesSnapshotUniqueIndex, ManualRunActiveIndex} {
		_ = database.Exec("DROP INDEX IF EXISTS " + index).Error
	}
	if err := database.Exec("CREATE UNIQUE INDEX " + ManualRunFilesPathUniqueIndex + " ON manual_run_files(run_id, relative_path)").Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Exec("CREATE UNIQUE INDEX " + ManualRunFilesSnapshotUniqueIndex + " ON manual_run_files(run_id, snapshot_key)").Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Exec("CREATE UNIQUE INDEX " + ManualRunIdempotencyIndex + " ON manual_transfer_runs(idempotency_key)").Error; err != nil {
		t.Fatal(err)
	}
	first := migrationRun(1, "old-1")
	second := migrationRun(1, "old-2")
	otherTask := migrationRun(3, "old-3")
	if err := database.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&otherTask).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&ManualRunFile{RunID: first.ID, RelativePath: "old", SnapshotKey: "old", SizeBytes: 1, Device: 1, Inode: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(database); err != nil {
		t.Fatal(err)
	}
	var reconciled, retained ManualTransferRun
	if err := database.First(&reconciled, first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.First(&retained, second.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reconciled.State != ManualRunStateAnalysisFailed || retained.State != ManualRunStateAnalyzing {
		t.Fatalf("migration states = %q/%q", reconciled.State, retained.State)
	}
	var event ManualRunEvent
	if err := database.Where("run_id = ? AND event_type = ?", first.ID, ManualRunEventMigrationReconciled).First(&event).Error; err != nil {
		t.Fatal(err)
	}
	if event.ActorIdentity != "system" || event.ActorType != "system" {
		t.Fatalf("migration event actor = %q/%q", event.ActorIdentity, event.ActorType)
	}
	var files int64
	if err := database.Model(&ManualRunFile{}).Where("run_id = ?", first.ID).Count(&files).Error; err != nil {
		t.Fatal(err)
	}
	if files != 0 {
		t.Fatalf("reconciled run retained %d file rows", files)
	}
	assertIndexColumns(t, database, ManualRunIdempotencyIndex, []string{"task_id", "idempotency_key"})
	assertIndexColumns(t, database, ManualRunFilesRunPathIndex, []string{"run_id", "generation", "relative_path"})
	assertIndexColumns(t, database, ManualRunFilesPathUniqueIndex, []string{"run_id", "generation", "relative_path"})
	assertIndexColumns(t, database, ManualRunFilesSnapshotUniqueIndex, []string{"run_id", "generation", "snapshot_key"})
	assertIndexColumns(t, database, ManualRunActiveIndex, []string{"task_id"})
	shared := migrationRun(4, "old-1")
	shared.State = ManualRunStateAnalysisFailed
	if err := database.Create(&shared).Error; err != nil {
		t.Fatal("task-scoped idempotency index rejected a shared key", err)
	}
	if err := database.Create(&ManualTransferRun{TaskID: 1, State: ManualRunStateAnalyzing, Revision: 1, ActorIdentity: "system", ActorType: "system", SourcePath: "/source", DestinationPath: "/dest", TransferMode: "copy", ConfigIdentity: "/config", FrozenInput: "{}", IdempotencyKey: "old-4", RequestFingerprint: "old-4", SourceRootDevice: 1, SourceRootInode: 1}).Error; err == nil {
		t.Fatal("active-run unique index accepted a duplicate analyzing task")
	}
}

func TestEnsureSchemaUpgradesMissingGenerationColumnsFromPhaseOne(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "phase-one.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.AutoMigrate(&phaseOneRun{}, &phaseOneFile{}, &phaseOneEvent{}); err != nil {
		t.Fatal(err)
	}
	first := phaseOneRun{TaskID: 1, State: ManualRunStateAnalyzing, Revision: 1, ActorIdentity: "operator", ActorType: "admin_session", SourcePath: "/source", DestinationPath: "/dest", TransferMode: "copy", ConfigIdentity: "/config", FrozenInput: "{}", IdempotencyKey: "phase-one-1", RequestFingerprint: "phase-one-1", SourceRootDevice: 1, SourceRootInode: 1}
	second := first
	second.IdempotencyKey = "phase-one-2"
	if err := database.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&phaseOneFile{RunID: first.ID, RelativePath: "old", SnapshotKey: "old", SizeBytes: 1, Device: 1, Inode: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&ManualTransferRun{}, &ManualRunFile{}, &ManualRunEvent{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(database); err != nil {
		t.Fatal(err)
	}
	if !database.Migrator().HasColumn(&ManualTransferRun{}, "snapshot_generation") || !database.Migrator().HasColumn(&ManualRunFile{}, "generation") || !database.Migrator().HasColumn(&ManualRunFile{}, "activated_at") {
		t.Fatal("generation columns were not added to the phase-one schema")
	}
	assertIndexColumns(t, database, ManualRunFilesPathUniqueIndex, []string{"run_id", "generation", "relative_path"})
	assertIndexColumns(t, database, ManualRunFilesSnapshotUniqueIndex, []string{"run_id", "generation", "snapshot_key"})
}

type phaseOneRun struct {
	ID                 uint `gorm:"primaryKey"`
	TaskID             uint
	State              string
	Revision           int64
	ActorIdentity      string
	ActorType          string
	SourcePath         string
	DestinationPath    string
	TransferMode       string
	ConfigIdentity     string
	FrozenInput        string `gorm:"type:text"`
	IdempotencyKey     string `gorm:"uniqueIndex:uq_manual_transfer_runs_task_idempotency"`
	RequestFingerprint string
	SourceRootDevice   int64
	SourceRootInode    int64
	SnapshotDigest     string
	SnapshotCount      int64
	SnapshotBytes      int64
	AnalysisStartedAt  *time.Time
	AnalyzedAt         *time.Time
	FailedAt           *time.Time
	LastError          string `gorm:"type:text"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (phaseOneRun) TableName() string { return "manual_transfer_runs" }

type phaseOneFile struct {
	ID           uint   `gorm:"primaryKey"`
	RunID        uint   `gorm:"uniqueIndex:uq_manual_run_files_run_path,priority:1;uniqueIndex:uq_manual_run_files_run_snapshot,priority:1"`
	RelativePath string `gorm:"uniqueIndex:uq_manual_run_files_run_path,priority:2"`
	SnapshotKey  string `gorm:"uniqueIndex:uq_manual_run_files_run_snapshot,priority:2"`
	SizeBytes    int64
	MtimeNS      int64
	Device       int64
	Inode        int64
	CreatedAt    time.Time
}

func (phaseOneFile) TableName() string { return "manual_run_files" }

type phaseOneEvent struct {
	ID            uint `gorm:"primaryKey"`
	RunID         uint
	EventType     string
	FromState     string
	ToState       string
	ActorIdentity string
	ActorType     string
	Details       string `gorm:"type:text"`
	CreatedAt     time.Time
}

func (phaseOneEvent) TableName() string { return "manual_run_events" }

func migrationRun(taskID uint, key string) ManualTransferRun {
	return ManualTransferRun{TaskID: taskID, State: ManualRunStateAnalyzing, Revision: 1, ActorIdentity: "operator", ActorType: "admin_session", SourcePath: "/source", DestinationPath: "/dest", TransferMode: "copy", ConfigIdentity: "/config", FrozenInput: "{}", IdempotencyKey: key, RequestFingerprint: key, SourceRootDevice: 1, SourceRootInode: 1}
}

func assertIndexColumns(t *testing.T, database *gorm.DB, name string, want []string) {
	t.Helper()
	type indexColumn struct {
		Seq  int    `gorm:"column:seqno"`
		Name string `gorm:"column:name"`
	}
	var columns []indexColumn
	if err := database.Raw("PRAGMA index_info(" + name + ")").Scan(&columns).Error; err != nil {
		t.Fatal(err)
	}
	if len(columns) != len(want) {
		t.Fatalf("index %s columns = %#v, want %#v", name, columns, want)
	}
	for i, column := range columns {
		if column.Seq != i || column.Name != want[i] {
			t.Fatalf("index %s columns = %#v, want %#v", name, columns, want)
		}
	}
}
