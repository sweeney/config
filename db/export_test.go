package db

import (
	"testing"
	"time"
)

// SetSnapshotClock pins the clock used to name pre-rebuild snapshots so a
// test can predict the path, and restores it when the test ends.
func SetSnapshotClock(t *testing.T, at time.Time) {
	previous := snapshotClock
	snapshotClock = func() time.Time { return at }
	t.Cleanup(func() { snapshotClock = previous })
}
