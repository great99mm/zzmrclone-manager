//go:build !linux

package quota

import "testing"

func TestScannerUnsupportedPlatform(t *testing.T) {
	if _, err := (Scanner{}).Scan("/tmp", 0); err == nil {
		t.Fatal("unsupported platform scan returned nil error")
	}
}
