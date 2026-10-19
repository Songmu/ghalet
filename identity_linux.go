package ghalet

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

var linuxBootID = sync.OnceValues(func() (string, error) {
	value, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(value)), err
})

func kernelProcessStart(pid int) (string, error) {
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

func kernelGroupStopped(pgid int, observed map[int]processEntry) (bool, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		stopped, err := linuxGroupStopped("/proc", pgid, observed)
		if !errors.Is(err, os.ErrNotExist) {
			return stopped, err
		}
		lastErr = err
	}
	return false, fmt.Errorf("kernel process snapshot kept changing: %w", lastErr)
}

func linuxGroupStopped(root string, pgid int, previous map[int]processEntry) (bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, err
	}
	observed := false
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		fields, err := linuxProcessFields(filepath.Join(root, entry.Name(), "stat"))
		if err != nil {
			// A vanished PID may have forked a child omitted from this enumeration.
			return false, err
		}
		group, err := strconv.Atoi(fields[2])
		if err != nil {
			return false, fmt.Errorf("invalid kernel process group: %w", err)
		}
		if group != pgid {
			continue
		}
		observed = true
		// A zombie thread-group leader can still have live worker threads.
		if fields[0] != "Z" || fields[17] != "1" {
			return false, nil
		}
		old := previous[pid]
		boot, err := linuxBootID()
		if err != nil {
			return false, err
		}
		// A newly observed zombie could have forked after this PID enumeration.
		if !old.Zombie || old.PGID != pgid || old.Started != fmt.Sprintf("linux:%s:%s", boot, fields[19]) {
			return false, nil
		}
	}
	return observed, nil
}
