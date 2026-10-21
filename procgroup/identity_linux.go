package procgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

var linuxBootID = sync.OnceValues(func() (string, error) {
	value, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(value)), err
})

// ProcessStart returns the process birth time scoped to the current kernel boot.
func ProcessStart(pid int) (string, error) {
	boot, err := linuxBootID()
	if err != nil {
		return "", err
	}
	fields, err := linuxProcessFields("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("linux:%s:%s", boot, fields[19]), nil
}

func linuxProcessFields(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// comm is parenthesized and can itself contain spaces or parentheses.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return nil, errors.New("invalid /proc process identity")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return nil, errors.New("incomplete /proc process identity")
	}
	return fields, nil
}

func kernelGroupStopped(pgid int, observed map[int]Entry) (bool, error) {
	return linuxGroupStopped("/proc", pgid, observed, unix.Getpgid)
}

func linuxGroupStopped(root string, pgid int, previous map[int]Entry, groupForPID func(int) (int, error)) (bool, error) {
	var lastErr error
	confirming := false
	for attempt := 0; attempt < 3; attempt++ {
		stopped, ambiguous, err := linuxGroupSnapshot(root, pgid, previous, groupForPID)
		if err == nil {
			if !stopped || !ambiguous || confirming {
				return stopped, nil
			}
			// Re-enumerate after an unclassified PID vanished so its possible
			// late-forked descendant is included in the next group check.
			confirming = true
			continue
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ESRCH) {
			return stopped, err
		}
		confirming = false
		lastErr = err
	}
	return false, fmt.Errorf("kernel process snapshot kept changing: %w", lastErr)
}

func linuxGroupSnapshot(root string, pgid int, previous map[int]Entry, groupForPID func(int) (int, error)) (bool, bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, false, err
	}
	observed := false
	ambiguous := false
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		group, err := groupForPID(pid)
		if err != nil {
			if errors.Is(err, unix.ESRCH) && previous[pid].PGID != pgid {
				ambiguous = true
				continue
			}
			return false, false, err
		}
		if group != pgid {
			continue
		}
		fields, err := linuxProcessFields(filepath.Join(root, entry.Name(), "stat"))
		if err != nil {
			// A vanished PID may have forked a child omitted from this enumeration.
			return false, false, err
		}
		group, err = strconv.Atoi(fields[2])
		if err != nil {
			return false, false, fmt.Errorf("invalid kernel process group: %w", err)
		}
		if group != pgid {
			// Membership changed after the query; do not lose a possible forker.
			return false, false, nil
		}
		observed = true
		// A zombie thread-group leader can still have live worker threads.
		if fields[0] != "Z" || fields[17] != "1" {
			return false, false, nil
		}
		old := previous[pid]
		boot, err := linuxBootID()
		if err != nil {
			return false, false, err
		}
		// A newly observed zombie could have forked after this PID enumeration.
		if !old.Zombie || old.PGID != pgid || old.Started != fmt.Sprintf("linux:%s:%s", boot, fields[19]) {
			return false, false, nil
		}
	}
	return observed, ambiguous, nil
}
