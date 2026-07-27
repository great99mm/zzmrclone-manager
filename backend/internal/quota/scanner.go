package quota

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// LocalSnapshot is the stable metadata captured for one local regular file.
// Content is deliberately not read in Phase 2.
type LocalSnapshot struct {
	RelativePath string
	SizeBytes    int64
	MtimeNS      int64
	Device       int64
	Inode        int64
	RootDevice   int64
	RootInode    int64
	SnapshotKey  string
}

func ValidateRelativePath(value string) error {
	if value == "" || filepath.IsAbs(value) || strings.ContainsAny(value, "\\\x00\r\n") || filepath.ToSlash(value) != value {
		return fmt.Errorf("unsafe relative path %q", value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("noncanonical relative path %q", value)
		}
	}
	return nil
}

// Scanner controls the metadata stability check. LookupHook is a test
// affordance: it runs immediately before each independent no-follow lookup.
type Scanner struct {
	Now                      func() time.Time
	SettleInterval           time.Duration
	Sleep                    func(time.Duration)
	LookupHook               func(relativePath string, observation int)
	BeforeFinalValidation    func()
	BeforeSnapshotValidation func(relativePath string)
}

type ScanOutcome struct {
	Snapshots      []LocalSnapshot
	NextEligibleAt *time.Time
}

// SnapshotConsumer receives one stable regular-file observation at a time.
// Implementations can persist the observation immediately instead of retaining
// the complete source tree in memory.
type SnapshotConsumer func(LocalSnapshot) error

func unsupportedTraversalError() error {
	return fmt.Errorf("secure descriptor-relative scanner is unsupported on this platform")
}

type fileMetadata struct {
	size    int64
	mtimeNS int64
	device  int64
	inode   int64
}

func makeSnapshotKey(relative string, meta fileMetadata) string {
	hash := sha256.New()
	for _, value := range []string{
		relative,
		strconv.FormatInt(meta.size, 10),
		strconv.FormatInt(meta.mtimeNS, 10),
		strconv.FormatInt(meta.device, 10),
		strconv.FormatInt(meta.inode, 10),
	} {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
