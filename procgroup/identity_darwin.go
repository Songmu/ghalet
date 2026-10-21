package procgroup

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// ProcessStart returns the kernel's process birth identity.
func ProcessStart(pid int) (string, error) {
	entries, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}

	if len(entries) == 0 || entries[0].Proc.P_pid != int32(pid) {
		return "", fmt.Errorf("process %d disappeared: %w", pid, syscall.ESRCH)
	}
	start := entries[0].Proc.P_starttime
	return fmt.Sprintf("darwin:%d:%d", start.Sec, start.Usec), nil
}

func kernelGroupStopped(pgid int, observed map[int]Entry) (bool, error) {
	entries, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return false, err
	}
	const zombie = 5 // SZOMB in Darwin's sys/proc.h.
	for _, entry := range entries {
		old := observed[int(entry.Proc.P_pid)]
		start := entry.Proc.P_starttime
		identity := fmt.Sprintf("darwin:%d:%d", start.Sec, start.Usec)
		if entry.Proc.P_stat != zombie || !old.Zombie || old.PGID != pgid || old.Started != identity {
			return false, nil
		}
	}
	return len(entries) > 0, nil
}
