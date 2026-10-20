package credentialstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

type brokenPromptWriter struct{}

func (brokenPromptWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestPromptProcess(t *testing.T) {
	mode := os.Getenv("GHALET_PROMPT_TEST_PROCESS")
	if mode == "" {
		return
	}
	if mode == "output-failure" {
		_, err := readPassword(context.Background(), brokenPromptWriter{})
		if err == nil {
			os.Exit(2)
		}
		return
	}
	options := Options{Name: "prompt", Type: "pat", Prompt: true, NoDefault: true, PromptOutput: os.Stdout}
	if mode == "preflight" {
		options.NoDefault, options.Config = false, "/nonexistent/ghalet-config.yaml"
	}
	_, err := SetFile(context.Background(), options, os.Stdin)
	if mode == "cancel" || mode == "preflight" {
		if err == nil {
			os.Exit(3)
		}
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
}

func TestPromptPTY(t *testing.T) {
	for _, mode := range []string{"immediate-input", "different-stdin", "cancel", "preflight", "output-failure"} {
		t.Run(mode, func(t *testing.T) {
			root := testDirectory(t)
			command := exec.Command(os.Args[0], "-test.run=^TestPromptProcess$")
			command.Env = append(os.Environ(), "GHALET_PROMPT_TEST_PROCESS="+mode, "STATE_DIRECTORY=", "XDG_STATE_HOME="+root)
			attrs := &syscall.SysProcAttr{Setsid: true, Setctty: true}
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer slave.Close()
			original, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
			if mode == "different-stdin" {
				alternate, slave, err := pty.Open()
				if err != nil {
					t.Fatal(err)
				}
				defer alternate.Close()
				defer slave.Close()
				command.Stdin, attrs.Ctty = slave, 1
			}
			command.SysProcAttr = attrs
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			slave.Close()
			captured := make(chan []byte, 1)
			go func() {
				var output bytes.Buffer
				var buffer [256]byte
				sent := false
				for {
					n, err := master.Read(buffer[:])
					output.Write(buffer[:n])
					if !sent && strings.Contains(output.String(), "PAT: ") {
						sent = true
						input := "synthetic-pty-secret\n"
						if mode == "cancel" {
							input = "\x03"
						}
						master.Write([]byte(input))
					}
					if err != nil || n == 0 {
						break
					}
				}
				captured <- output.Bytes()
			}()
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				command.Process.Kill()
				<-done
				t.Fatal("terminal prompt did not exit")
			}
			state, err := term.GetState(int(master.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(state, original) {
				t.Fatal("controlling terminal was not restored")
			}
			output := <-captured
			if bytes.Contains(output, []byte("synthetic-pty-secret")) {
				t.Fatal("prompt echoed the credential")
			}
			if mode == "preflight" && bytes.Contains(output, []byte("PAT: ")) {
				t.Fatal("preflight failure exposed a premature prompt")
			}
		})
	}
}
