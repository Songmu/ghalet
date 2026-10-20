package appsetup

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func TestReadConfirmation(t *testing.T) {
	for _, answer := range []string{"yes\n", "y\n", "YES\r\n", "n\n", "yes", ""} {
		t.Run(answer, func(t *testing.T) {
			actual, err := readConfirmation(t.Context(), confirmationInput(t, answer))
			if err != nil || actual != strings.TrimSuffix(answer, "\n") {
				t.Fatal("confirmation input was not read correctly", actual, err)
			}
		})
	}
}

func TestConfirmationTerminalDeadline(t *testing.T) {
	master, input, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer input.Close()
	fd := input.Fd()
	flags, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	state, err := term.GetState(int(fd))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := master.Write([]byte("yes")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := readConfirmation(ctx, input); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("terminal input without a newline ignored the deadline", err)
	}
	currentFlags, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
	if err != nil || currentFlags != flags {
		t.Fatal("terminal descriptor flags were not restored", err)
	}
	currentState, err := term.GetState(int(fd))
	if err != nil || !reflect.DeepEqual(currentState, state) {
		t.Fatal("terminal settings were changed", err)
	}
}
