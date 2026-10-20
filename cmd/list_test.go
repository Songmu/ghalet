package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"testing"
)

func TestListCommand(t *testing.T) {
	missing := filepath.Join(testDirectory(t), "missing.yaml")
	for _, args := range [][]string{
		{"list", "--config", missing},
		{"--config", missing, "list"},
	} {
		var out, stderr bytes.Buffer
		if err := Run(t.Context(), args, &out, &stderr); err != nil {
			t.Fatal(err, stderr.String())
		}
		var result struct {
			Daemons []json.RawMessage `json:"daemons"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Daemons == nil || len(result.Daemons) != 0 ||
			stderr.Len() != 0 {
			t.Fatal("list did not return an empty JSON array", out.String(), stderr.String(), err)
		}
	}
	if err := list(t.Context(), missing, failingWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("list ignored output error", err)
	}
}
