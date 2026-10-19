package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/Songmu/ghalet"
)

func TestPrintResponse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response commandResponse
		err      error
		wantJSON bool
	}{
		{name: "empty success", wantJSON: true},
		{name: "status", response: commandResponse{Status: &ghalet.Status{Config: "selected.yaml"}}, wantJSON: true},
		{name: "diagnostics", response: commandResponse{Status: &ghalet.Status{}}, err: ghalet.OperationError("diagnostic problem"), wantJSON: true},
		{name: "partial cleanup", response: commandResponse{Results: []ghalet.OperationResult{{Error: "cleanup failed"}}}, err: ghalet.OperationError("incomplete"), wantJSON: true},
		{name: "management failure", err: ghalet.OperationError("unknown target"), wantJSON: true},
		{name: "connection failure", err: errors.New("connection failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := printResponse(&output, tc.response, tc.err); !errors.Is(err, tc.err) {
				t.Fatal("operation error was not preserved", err)
			}
			if !tc.wantJSON {
				if output.Len() != 0 {
					t.Fatal("failure unexpectedly produced output", output.String())
				}
				return
			}
			var result commandResponse
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if (tc.err != nil && result.Error != tc.err.Error()) || (tc.response.Status != nil && result.Status == nil) ||
				len(result.Results) != len(tc.response.Results) {
				t.Fatal("management output was lost", output.String())
			}
		})
	}
}

func TestPrintResponseReportsWriteFailure(t *testing.T) {
	err := printResponse(failingWriter{}, commandResponse{}, nil)
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("output failure was ignored", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
