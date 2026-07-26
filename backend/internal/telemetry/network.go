package telemetry

import (
	"bufio"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"rclone-manager/internal/models"
)

const (
	procNetDevPath       = "/proc/net/dev"
	sampleInterval       = 5 * time.Minute
	sampleRetention      = 72 * time.Hour
	maxStoredSamples int = 1000
)

type Report struct {
	Available            bool       `json:"available"`
	TxBytes              *int64     `json:"tx_bytes"`
	Rolling24hTxBytes    *int64     `json:"rolling_24h_tx_bytes"`
	BaselineAvailable    bool       `json:"baseline_available"`
	BaselineAt           *time.Time `json:"baseline_at"`
	SampledAt            *time.Time `json:"sampled_at"`
	LedgerCommittedBytes int64      `json:"ledger_committed_bytes"`
	DifferenceBytes      *int64     `json:"difference_bytes"`
}

// ReadAggregateTxBytes reads aggregate non-loopback interface counters. It is
// intentionally advisory: callers must ignore an unavailable result.
func ReadAggregateTxBytes() (int64, error) {
	file, err := os.Open(procNetDevPath)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	var data strings.Builder
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		data.WriteString(scanner.Text())
		data.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return ParseProcNetDev(data.String())
}

// ParseProcNetDev parses Linux /proc/net/dev contents without touching the
// filesystem, which also makes counter handling deterministic in tests.
func ParseProcNetDev(contents string) (int64, error) {
	var total int64
	found := false
	scanner := bufio.NewScanner(strings.NewReader(contents))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		if name == "" || name == "lo" || strings.HasPrefix(name, "lo") {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 9 {
			return 0, errors.New("invalid /proc/net/dev interface row")
		}
		value, parseErr := strconv.ParseInt(fields[8], 10, 64)
		if parseErr != nil || value < 0 {
			return 0, errors.New("invalid /proc/net/dev transmit counter")
		}
		if total > int64(^uint64(0)>>1)-value {
			return 0, errors.New("/proc/net/dev transmit counters overflow")
		}
		total += value
		found = true
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if !found {
		return 0, errors.New("no non-loopback interface counters available")
	}
	return total, nil
}

// Capture samples once per interval and prunes old rows. Errors are returned
// for diagnostics, but telemetry must never be a scheduling gate.
func Capture(database *gorm.DB, now time.Time) error {
	if database == nil {
		return errors.New("telemetry database is required")
	}
	if !database.Migrator().HasTable(&models.NetworkTelemetrySample{}) {
		return nil
	}
	available := true
	txBytes, err := ReadAggregateTxBytes()
	if err != nil {
		available = false
		txBytes = 0
	}
	var latest models.NetworkTelemetrySample
	if result := database.Order("sampled_at DESC").First(&latest); result.Error == nil && now.Sub(latest.SampledAt) < sampleInterval && now.After(latest.SampledAt) {
		return nil
	}
	if result := database.Create(&models.NetworkTelemetrySample{SampledAt: now, TxBytes: txBytes, Available: available}); result.Error != nil {
		return result.Error
	}
	cutoff := now.Add(-sampleRetention)
	if err := database.Where("sampled_at < ?", cutoff).Delete(&models.NetworkTelemetrySample{}).Error; err != nil {
		return err
	}
	var count int64
	if err := database.Model(&models.NetworkTelemetrySample{}).Count(&count).Error; err != nil {
		return err
	}
	if count > int64(maxStoredSamples) {
		var ids []uint
		if err := database.Model(&models.NetworkTelemetrySample{}).Order("sampled_at ASC").Limit(int(count-int64(maxStoredSamples))).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) > 0 {
			return database.Where("id IN ?", ids).Delete(&models.NetworkTelemetrySample{}).Error
		}
	}
	return nil
}

func RollingReport(database *gorm.DB, now time.Time) (Report, error) {
	var report Report
	if !database.Migrator().HasTable(&models.NetworkTelemetrySample{}) {
		return report, nil
	}
	var latest models.NetworkTelemetrySample
	if result := database.Order("sampled_at DESC").First(&latest); result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return report, nil
		}
		if strings.Contains(strings.ToLower(result.Error.Error()), "no such table") {
			return report, nil
		}
		return report, result.Error
	}
	// The newest sample is authoritative. An unavailable sample must not cause
	// an older available counter to be presented as current.
	if !latest.Available {
		report.SampledAt = &latest.SampledAt
		return report, nil
	}
	value := latest.TxBytes
	report.Available = true
	report.TxBytes = &value
	report.SampledAt = &latest.SampledAt
	cutoff := now.Add(-24 * time.Hour)
	var baseline models.NetworkTelemetrySample
	if result := database.Where("available = ? AND sampled_at <= ?", true, cutoff).Order("sampled_at DESC").First(&baseline); result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return report, nil
		}
		if strings.Contains(strings.ToLower(result.Error.Error()), "no such table") {
			return report, nil
		}
		return report, result.Error
	}
	if latest.TxBytes < baseline.TxBytes {
		return report, nil
	}
	delta := latest.TxBytes - baseline.TxBytes
	report.Rolling24hTxBytes = &delta
	report.BaselineAvailable = true
	report.BaselineAt = &baseline.SampledAt
	return report, nil
}

// SortSamples is kept small and deterministic for tests and diagnostics.
func SortSamples(samples []models.NetworkTelemetrySample) {
	sort.Slice(samples, func(i, j int) bool { return samples[i].SampledAt.Before(samples[j].SampledAt) })
}
