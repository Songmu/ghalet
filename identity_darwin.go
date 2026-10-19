package ghalet

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

func kernelProcessStart(pid int) (string, error) {
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
