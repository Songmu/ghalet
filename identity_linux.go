package ghalet

import (
	"errors"
	"fmt"
	"os"
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
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	// comm is parenthesized and can itself contain spaces or parentheses.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", errors.New("invalid /proc process identity")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return "", errors.New("incomplete /proc process identity")
	}
	return fmt.Sprintf("linux:%s:%s", boot, fields[19]), nil
}
