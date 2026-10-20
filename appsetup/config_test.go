package appsetup

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Songmu/ghalet/fshelper"
)

func TestWriteConfigCoordinatesWriters(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "config.yaml")
	before := []byte("targets: []\n")
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	after := append(bytes.Clone(before), []byte("# updated\n")...)
	lock, err := fshelper.LockFile(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := writeConfig(path, before, after); err == nil {
		t.Fatal("setup ignored another configuration writer's lock")
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, before) {
		t.Fatal("locked configuration was modified", err)
	}
	lock.Close()
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- writeConfig(path, before, after)
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent writers reported %d successes; want one", successes)
	}
}
