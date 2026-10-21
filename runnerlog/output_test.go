package runnerlog

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Songmu/ghalet/logreduct"
)

func TestLogLineLimitsApplyToLiveOutputAndArchives(t *testing.T) {
	longChunks := make([]string, 4096)
	for i := range longChunks {
		longChunks[i] = strings.Repeat("x", 1024)
	}
	for _, tc := range []struct {
		name   string
		chunks []string
		want   string
		ready  int
	}{
		{
			name:   "exact limit",
			chunks: []string{strings.Repeat("x", maxLogLineBytes-len("secret-value")), "sec", "ret-value\n"},
			want:   strings.Repeat("x", maxLogLineBytes-len("secret-value")) + "***\n",
		},
		{
			name:   "single large write",
			chunks: []string{strings.Repeat("x", 4*1024*1024)},
			want:   omittedLogLine,
		},
		{
			name:   "incremental unterminated line",
			chunks: longChunks,
			want:   omittedLogLine,
		},
		{
			name:   "secret across omission boundary and resume",
			chunks: []string{strings.Repeat("x", maxLogLineBytes-3) + "sec", "ret-value", "\nsec", "ret-value\nListening for ", "Jobs\nsec", "ret-value"},
			want:   omittedLogLine + "***\nListening for Jobs\n***",
			ready:  1,
		},
		{
			name:   "oversized readiness line",
			chunks: []string{"Listening for Jobs", strings.Repeat("x", maxLogLineBytes), "\n"},
			want:   omittedLogLine,
		},
		{
			name:   "newline-inclusive secret",
			chunks: []string{"whole-line-", "secret\n", "next\n"},
			want:   "***next\n",
		},
		{
			name:   "multiline secret",
			chunks: []string{"-----BEGIN PRIVATE ", "KEY-----\nprivate-", "key-line-one\nprivate-key-line-", "two\n-----END PRIVATE KEY-----\nnext\n"},
			want:   "***\n***\n***\n***\nnext\n",
		},
		{
			name:   "overlapping secrets",
			chunks: []string{"shared-", "secret-private-", "suffix\nabc", "d\nabc", "d"},
			want:   "***\n***\n***",
		},
		{
			name:   "CRLF multiline secret with final partial line",
			chunks: []string{"-----BEGIN PRIVATE KEY-----\r", "\nprivate-key-line-", "one\r\nprivate-key-line-two\r\n-----END PRIVATE KEY-----"},
			want:   "***\r\n***\r\n***\r\n***",
		},
	} {
		for _, live := range []bool{true, false} {
			mode := "archive"
			if live {
				mode = "live"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "output.log")
				secret := &logreduct.Redactor{}
				secret.Add("secret-value")
				secret.Add("whole-line-secret\n")
				secret.Add("shared-secret")
				secret.Add("shared-secret-private-suffix")
				secret.Add("abc")
				secret.Add("bcd")
				secret.Add("-----BEGIN PRIVATE KEY-----\r\nprivate-key-line-one\r\nprivate-key-line-two\r\n-----END PRIVATE KEY-----")
				if live {
					file, err := os.Create(path)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { file.Close() })
					ready := 0
					writer := NewOutput(file, secret, func() { ready++ })
					for _, chunk := range tc.chunks {
						if n, err := writer.Write([]byte(chunk)); err != nil || n != len(chunk) {
							t.Fatal("bounded output write failed", n, err)
						}
						if len(writer.buffer) > maxLogLineBytes {
							t.Fatal("unterminated output exceeded its buffer limit", len(writer.buffer))
						}
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
					if ready != tc.ready {
						t.Fatal("readiness detection changed across chunks or accepted an omitted line", ready)
					}
				} else {
					readers := make([]io.Reader, len(tc.chunks))
					for i, chunk := range tc.chunks {
						readers[i] = strings.NewReader(chunk)
					}
					if err := copyRedacted(io.MultiReader(readers...), path, secret); err != nil {
						t.Fatal(err)
					}
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != tc.want {
					t.Fatalf("unexpected bounded/redacted output: got %d bytes, want %d (%v)", len(data), len(tc.want), err)
				}
			})
		}
	}
}

func TestRunnerOutputRedactsSplitSecretsAndDetectsReadiness(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "output.log"))
	if err != nil {
		t.Fatal(err)
	}
	secret := &logreduct.Redactor{}
	secret.Add("secret-value")
	ready := 0
	writer := NewOutput(file, secret, func() { ready++ })
	for _, value := range []string{"sec", "ret-value\nListening for ", "Jobs\nListening for Jobs\nsec", "ret-value"} {
		if _, err := writer.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file.Name())
	if ready != 1 || strings.Contains(string(data), "secret-value") || strings.Count(string(data), "***") != 2 {
		t.Fatalf("ready=%d output=%s", ready, data)
	}
}
