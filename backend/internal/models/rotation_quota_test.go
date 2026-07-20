package models

import (
	"reflect"
	"testing"
)

func TestRotationQuotaKeysParseAndEncode(t *testing.T) {
	parsed, err := ParseRotationQuotaKeys(` { " remote-a ": " key-a " } `)
	if err != nil {
		t.Fatalf("ParseRotationQuotaKeys returned error: %v", err)
	}
	if want := map[string]string{"remote-a": "key-a"}; !reflect.DeepEqual(parsed, want) {
		t.Fatalf("parsed quota keys = %#v, want %#v", parsed, want)
	}

	encoded := EncodeRotationQuotaKeys(parsed)
	if encoded != `{"remote-a":"key-a"}` {
		t.Fatalf("encoded quota keys = %q, want %q", encoded, `{"remote-a":"key-a"}`)
	}

	empty, err := ParseRotationQuotaKeys("   ")
	if err != nil {
		t.Fatalf("blank input returned error: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("blank input = %#v, want empty map", empty)
	}
}

func TestDefaultRotationQuotaKeyIsCanonical(t *testing.T) {
	first := DefaultRotationQuotaKey("/config", "drive")
	if first == "" || first != DefaultRotationQuotaKey("/config", "drive") {
		t.Fatalf("default quota key is not stable: %q", first)
	}
	if first == DefaultRotationQuotaKey("/other", "drive") || first == DefaultRotationQuotaKey("/config", "other") {
		t.Fatal("default quota key ignored config identity or remote")
	}
}

func TestRotationQuotaKeysRejectInvalidInput(t *testing.T) {
	for _, raw := range []string{
		`["remote-a", "key-a"]`,
		`null`,
		`{"remote-a": 123}`,
		`{"   ": "key-a"}`,
		`{"remote-a": "   "}`,
		`{"remote-a": "key-a"`,
		`{" remote-a ": "key-a", "remote-a": "key-b"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := ParseRotationQuotaKeys(raw); err == nil {
				t.Fatalf("ParseRotationQuotaKeys(%q) returned nil error", raw)
			}
		})
	}
}

func TestDestinationScopeVersionOneEncoding(t *testing.T) {
	const want = "c2c8a9c9ae1b6a3394d6beb58d1e3eed3ff8278b810308a4a03bb84b108b300e"
	if got := DestinationScope("/srv/rclone.conf", "/Shared/Uploads"); got != want {
		t.Fatalf("destination scope = %q, want version-one hash %q", got, want)
	}
}
