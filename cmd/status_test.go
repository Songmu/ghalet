package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Songmu/ghalet"
)

func serveStatus(t *testing.T, response commandResponse) {
	t.Helper()
	sum := sha256.Sum256([]byte(response.Status.Config))
	dir := filepath.Join("/tmp", "ghalet-"+strconv.Itoa(os.Geteuid()))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, hex.EncodeToString(sum[:12])+".sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(listener.Addr().String(), 0600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		listener.Close()
		<-done
	})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			var request struct {
				Command string `json:"command"`
			}
			if err := json.NewDecoder(conn).Decode(&request); err == nil {
				if request.Command != "status" {
					t.Error("unexpected control command", request.Command)
				}
				json.NewEncoder(conn).Encode(response)
			}
			conn.Close()
		}
	}()
}

func TestStatusFailOnError(t *testing.T) {
	for _, unhealthy := range []bool{false, true} {
		name := "healthy"
		if unhealthy {
			name = "unhealthy"
		}
		t.Run(name, func(t *testing.T) {
			response := commandResponse{Status: &ghalet.Status{Config: writeConfig(t), Capacity: 2}}
			if unhealthy {
				response.Status.Problems = []ghalet.Problem{{Reason: "credentials expired"}}
				response.Error = "1 diagnostic problem(s)"
			}
			serveStatus(t, response)
			var baseline string
			for _, tc := range []struct {
				args        []string
				failOnError bool
			}{
				{args: []string{"status"}},
				{args: []string{"status", "--fail-on-error"}, failOnError: true},
				{args: []string{"--fail-on-error", "status"}, failOnError: true},
				{args: []string{"status", "--fail-on-error=false"}},
			} {
				args := append(append([]string{}, tc.args...), "--config", response.Status.Config)
				var output, stderr bytes.Buffer
				err := Run(t.Context(), args, &output, &stderr)
				if (err != nil) != (unhealthy && tc.failOnError) {
					t.Fatalf("%v: unexpected exit behavior: %v", args, err)
				}
				if err != nil {
					var diagnostic ghalet.OperationError
					if !errors.As(err, &diagnostic) || err.Error() != response.Error {
						t.Fatal("diagnostic error was lost", err)
					}
				}
				var result commandResponse
				if err := json.Unmarshal(output.Bytes(), &result); err != nil {
					t.Fatal(err, output.String())
				}
				if result.Error != response.Error || result.Status == nil ||
					result.Status.Config != response.Status.Config ||
					len(result.Status.Problems) != len(response.Status.Problems) || stderr.Len() != 0 {
					t.Fatal("status output was lost", output.String(), stderr.String())
				}
				if baseline == "" {
					baseline = output.String()
				} else if output.String() != baseline {
					t.Fatal("--fail-on-error changed the JSON output", output.String())
				}
			}
			for _, failOnError := range []bool{false, true} {
				if err := status(t.Context(), response.Status.Config, failOnError, failingWriter{}); !errors.Is(err, io.ErrClosedPipe) {
					t.Fatal("status ignored output failure", err)
				}
			}
		})
	}
}

func TestStatusConnectionFailure(t *testing.T) {
	path := writeConfig(t)
	for _, failOnError := range []bool{false, true} {
		var output bytes.Buffer
		if err := status(t.Context(), path, failOnError, &output); err == nil || output.Len() != 0 {
			t.Fatal("unavailable daemon did not fail", err, output.String())
		}
	}
}
