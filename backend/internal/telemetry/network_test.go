package telemetry

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"rclone-manager/internal/models"
)

func TestParseProcNetDevAggregatesNonLoopbackTransmitBytes(t *testing.T) {
	contents := "Inter-| Receive | Transmit\n" +
		" lo: 1 2 3 4 5 6 7 8 999\n" +
		"eth0: 10 11 12 13 14 15 16 17 100\n" +
		"veth123: 20 21 22 23 24 25 26 27 250\n"
	value, err := ParseProcNetDev(contents)
	if err != nil || value != 350 {
		t.Fatalf("value=%d err=%v", value, err)
	}
}

func TestRollingReportRequiresBaseline(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.NetworkTelemetrySample{}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	if err := db.Create(&models.NetworkTelemetrySample{SampledAt: now, TxBytes: 100, Available: true}).Error; err != nil {
		t.Fatal(err)
	}
	report, err := RollingReport(db, now)
	if err != nil || !report.Available || report.Rolling24hTxBytes != nil || report.BaselineAvailable {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	if err := db.Create(&models.NetworkTelemetrySample{SampledAt: now.Add(-24 * time.Hour), TxBytes: 25, Available: true}).Error; err != nil {
		t.Fatal(err)
	}
	report, err = RollingReport(db, now)
	if err != nil || report.Rolling24hTxBytes == nil || *report.Rolling24hTxBytes != 75 {
		t.Fatalf("baseline report=%#v err=%v", report, err)
	}
}

func TestRollingReportTreatsLatestUnavailableSampleAsUnavailable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.NetworkTelemetrySample{}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	if err := db.Create(&models.NetworkTelemetrySample{SampledAt: now.Add(-24 * time.Hour), TxBytes: 100, Available: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.NetworkTelemetrySample{SampledAt: now, TxBytes: 250, Available: true}).Error; err != nil {
		t.Fatal(err)
	}
	unavailableAt := now.Add(time.Minute)
	if err := db.Create(&models.NetworkTelemetrySample{SampledAt: unavailableAt, Available: false}).Error; err != nil {
		t.Fatal(err)
	}
	report, err := RollingReport(db, unavailableAt)
	if err != nil || report.Available || report.TxBytes != nil || report.Rolling24hTxBytes != nil || report.SampledAt == nil || !report.SampledAt.Equal(unavailableAt) {
		t.Fatalf("unavailable latest report=%#v err=%v", report, err)
	}
}
