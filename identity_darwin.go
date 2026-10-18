package ghalet

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func kernelProcessStart(pid int) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if info == nil || info.Proc.P_pid != int32(pid) {
		return "", fmt.Errorf("process %d disappeared", pid)
	}
	start := info.Proc.P_starttime
	return fmt.Sprintf("darwin:%d:%d", start.Sec, start.Usec), nil
}
