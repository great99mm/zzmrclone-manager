//go:build !linux

package quota

import (
	"context"
	"time"
)

func (s Scanner) Scan(root string, minAge time.Duration) ([]LocalSnapshot, error) {
	return nil, unsupportedTraversalError()
}

func (s Scanner) Stream(root string, minAge time.Duration, consumer SnapshotConsumer) error {
	return unsupportedTraversalError()
}

func (s Scanner) StreamContext(ctx context.Context, root string, minAge time.Duration, consumer SnapshotConsumer) error {
	return unsupportedTraversalError()
}
