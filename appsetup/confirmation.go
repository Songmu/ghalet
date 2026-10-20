package appsetup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func readConfirmation(ctx context.Context, input *os.File) (answer string, resultErr error) {
	if input == nil {
		return "", errors.New("confirmation input is required (or use --yes)")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	raw, err := input.SyscallConn()
	if err != nil {
		return "", err
	}
	var fd int
	if err := raw.Control(func(value uintptr) { fd = int(value) }); err != nil {
		return "", err
	}
	defer runtime.KeepAlive(input)
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return "", err
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags|unix.O_NONBLOCK); err != nil {
		return "", err
	}
	defer func() {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags); err != nil {
			answer = ""
			resultErr = errors.Join(resultErr, fmt.Errorf("restore confirmation input flags: %w", err))
		}
	}()
	var line strings.Builder
	var buffer [1]byte
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		ready, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 100)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return "", err
		}
		if ready == 0 {
			continue
		}
		n, err := unix.Read(fd, buffer[:])
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return "", err
		}
		if n == 0 || buffer[0] == '\n' {
			return line.String(), nil
		}
		line.WriteByte(buffer[0])
	}
}
