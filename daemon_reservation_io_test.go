package ghalet

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Songmu/ghalet/fshelper"
)

func TestOverlappingFirstReservationsWaitForTargetDirectoryDurability(t *testing.T) {
	d, api := testDaemon(t)
	api.jitErr = errTestAPI
	seedDistribution(t, d.config, "#!/bin/sh\nexit 0\n")
	entered, release := make(chan struct{}), make(chan struct{})
	releaseOnCleanup(t, release)
	var calls atomic.Int32
	var durable, unsafeOverlap atomic.Bool
	d.makeRunDirectory = func(dir string) error {
		if calls.Add(1) == 1 {
			// Model the interval between target-directory visibility and
			// the creator completing its parent sync.
			if err := os.Mkdir(filepath.Dir(dir), 0755); err != nil {
				return err
			}
			close(entered)
			<-release
			if err := fshelper.SyncDirectory(d.config.Runner.WorkRoot); err != nil {
				return err
			}
			durable.Store(true)
		} else if !durable.Load() {
			unsafeOverlap.Store(true)
		}
		return createRunDirectory(dir)
	}
	d.mu.Lock()
	for range 2 {
		if err := d.reserve(d.targets[buildTargetID]); err != nil {
			d.mu.Unlock()
			t.Fatal(err)
		}
	}
	d.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first reservation did not enter directory initialization")
	}
	if status := responsiveStatus(t, d); len(status.Runs) != 2 {
		t.Fatal("waiting directory initialization hid reservations", status)
	}
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("second reservation bypassed its target's pending parent sync")
	}
	close(release)
	settleRuns(d)
	if calls.Load() != 2 || unsafeOverlap.Load() {
		t.Fatal("overlapping first reservations did not preserve directory durability")
	}
}
