//go:build !linux

package quota

import "time"

func (s Scanner) Scan(root string, minAge time.Duration) ([]LocalSnapshot, error) {
	return nil, unsupportedTraversalError()
}
