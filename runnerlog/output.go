package runnerlog

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/Songmu/ghalet/logreduct"
)

const maxLogLineBytes = 64 * 1024
const omittedLogLine = "[ghalet: log line over 64 KiB omitted]\n"

// Output collects complete lines before masking secrets and detecting readiness.
// Close flushes a final partial line, syncs the file, and closes it.
type Output struct {
	mu       sync.Mutex
	file     *os.File
	buffer   []byte
	dropping bool
	secrets  *logreduct.Redactor
	ready    func()
}

// NewOutput takes ownership of file. It calls ready, if non-nil, at most once
// when a complete, non-omitted line contains the runner readiness message.
func NewOutput(file *os.File, secrets *logreduct.Redactor, ready func()) *Output {
	return &Output{file: file, secrets: secrets, ready: ready}
}

func (w *Output) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	size := len(data)
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		part := data
		if end >= 0 {
			part = data[:end]
		}
		if !w.dropping {
			if len(part) > maxLogLineBytes-len(w.buffer) {
				w.buffer = w.buffer[:0]
				w.dropping = true
				if _, err := io.WriteString(w.file, w.secrets.Redact(omittedLogLine)); err != nil {
					return 0, err
				}
			} else {
				w.buffer = append(w.buffer, part...)
			}
		}
		if end < 0 {
			break
		}
		if !w.dropping {
			line := string(w.buffer)
			if strings.Contains(line, "Listening for Jobs") && w.ready != nil {
				w.ready()
				w.ready = nil
			}
			if _, err := io.WriteString(w.file, w.secrets.Redact(line+"\n")); err != nil {
				return 0, err
			}
		}
		w.buffer, w.dropping = w.buffer[:0], false
		data = data[end+1:]
	}
	return size, nil
}

func (w *Output) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, writeErr := io.WriteString(w.file, w.secrets.Redact(string(w.buffer)))
	return errors.Join(writeErr, w.file.Sync(), w.file.Close())
}

func copyRedacted(input io.Reader, dest string, secret *logreduct.Redactor) error {
	output, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	writer := NewOutput(output, secret, nil)
	_, copyErr := io.Copy(writer, input)
	return errors.Join(copyErr, writer.Close())
}
