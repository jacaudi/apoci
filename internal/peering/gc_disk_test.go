package peering

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/notify"
)

const testDiskPath = "/some/path"

func newTestGC(t *testing.T, cfg GCConfig) *GarbageCollector {
	t.Helper()
	db, blobs := testGCDeps(t)
	notifier := notify.New("test", nil, nil, nopLog())
	return NewGarbageCollector(cfg, db, blobs, notifier, nopLog())
}

func setLastRun(gc *GarbageCollector, t time.Time) {
	gc.lastRunNS.Store(t.UnixNano())
}

func TestMaybeCollectDiskTriggers(t *testing.T) {
	gc := newTestGC(t, GCConfig{
		Interval:               time.Hour,
		DiskUsageThreshold:     80,
		DiskUsageCheckInterval: time.Minute,
		DiskUsagePath:          testDiskPath,
	})
	setLastRun(gc, time.Now())
	before := gc.lastRunNS.Load()
	calls := 0
	gc.diskUsage = func(string) (int, error) {
		calls++
		return 90, nil
	}

	gc.maybeCollect(context.Background())

	require.Equal(t, 1, calls)
	require.Greater(t, gc.lastRunNS.Load(), before, "disk-triggered cycle should advance lastRun")
}

func TestMaybeCollectDiskBelowThreshold(t *testing.T) {
	gc := newTestGC(t, GCConfig{
		Interval:               time.Hour,
		DiskUsageThreshold:     80,
		DiskUsageCheckInterval: time.Minute,
		DiskUsagePath:          testDiskPath,
	})
	setLastRun(gc, time.Now())
	before := gc.lastRunNS.Load()
	gc.diskUsage = func(string) (int, error) { return 50, nil }

	gc.maybeCollect(context.Background())

	require.Equal(t, before, gc.lastRunNS.Load())
}

func TestMaybeCollectDiskTriggerDisabled(t *testing.T) {
	gc := newTestGC(t, GCConfig{
		Interval:               time.Hour,
		DiskUsageThreshold:     0,
		DiskUsageCheckInterval: time.Minute,
		DiskUsagePath:          testDiskPath,
	})
	setLastRun(gc, time.Now())
	calls := 0
	gc.diskUsage = func(string) (int, error) {
		calls++
		return 99, nil
	}

	gc.maybeCollect(context.Background())

	require.Zero(t, calls)
}

func TestMaybeCollectDiskTriggerNoPath(t *testing.T) {
	gc := newTestGC(t, GCConfig{
		Interval:               time.Hour,
		DiskUsageThreshold:     80,
		DiskUsageCheckInterval: time.Minute,
		DiskUsagePath:          "",
	})
	setLastRun(gc, time.Now())
	calls := 0
	gc.diskUsage = func(string) (int, error) {
		calls++
		return 99, nil
	}

	gc.maybeCollect(context.Background())

	require.Zero(t, calls)
}

func TestMaybeCollectTimeTriggers(t *testing.T) {
	gc := newTestGC(t, GCConfig{
		Interval:           time.Millisecond,
		DiskUsageThreshold: 0,
	})
	setLastRun(gc, time.Now().Add(-time.Hour))
	before := gc.lastRunNS.Load()
	gc.diskUsage = func(string) (int, error) {
		t.Fatal("diskUsage must not be called when interval is exceeded")
		return 0, nil
	}

	gc.maybeCollect(context.Background())

	require.Greater(t, gc.lastRunNS.Load(), before)
}

func TestMaybeCollectDiskUsageError(t *testing.T) {
	gc := newTestGC(t, GCConfig{
		Interval:           time.Hour,
		DiskUsageThreshold: 80,
		DiskUsagePath:      testDiskPath,
	})
	setLastRun(gc, time.Now())
	before := gc.lastRunNS.Load()
	gc.diskUsage = func(string) (int, error) { return 0, errors.New("statfs boom") }

	gc.maybeCollect(context.Background())

	require.Equal(t, before, gc.lastRunNS.Load())
}

func TestRunOnceUpdatesLastRun(t *testing.T) {
	gc := newTestGC(t, GCConfig{Interval: time.Hour})

	gc.RunOnce(context.Background())

	require.NotZero(t, gc.lastRunNS.Load())
}
