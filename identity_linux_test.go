package ghalet

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxGroupStoppedRequiresCompleteSnapshot(t *testing.T) {
	boot, err := linuxBootID()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		state   string
		threads string
		missing bool
		want    bool
		wantErr bool
		newPID  bool
	}{
		{name: "zombie only", state: "Z", threads: "1", want: true},
		{name: "live omitted child", state: "S", threads: "1"},
		{name: "zombie leader with live threads", state: "Z", threads: "2"},
		{name: "vanished PID", missing: true, wantErr: true},
		{name: "new zombie could have forked", state: "Z", threads: "1", newPID: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			previous := map[int]processEntry{
				1234: {ProcessIdentity: ProcessIdentity{PID: 1234, PGID: 1234, Started: fmt.Sprintf("linux:%s:42", boot)}, Zombie: true},
				5678: {ProcessIdentity: ProcessIdentity{PID: 5678, PGID: 1234, Started: fmt.Sprintf("linux:%s:42", boot)}, Zombie: true},
			}
			if tc.newPID {
				delete(previous, 5678)
			}
			for _, process := range []struct{ pid, state, threads string }{
				{"1234", "Z", "1"},
				{"5678", tc.state, tc.threads},
			} {
				dir := filepath.Join(root, process.pid)
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if tc.missing && process.pid == "5678" {
					continue
				}
				fields := strings.Fields("Z 1 1234 0 0 0 0 0 0 0 0 0 0 0 0 0 0 1 0 42")
				fields[0], fields[17] = process.state, process.threads
				data := fmt.Sprintf("%s (test (process)) %s\n", process.pid, strings.Join(fields, " "))
				if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			stopped, err := linuxGroupStopped(root, 1234, previous)
			if stopped != tc.want || (err != nil) != tc.wantErr {
				t.Fatal("incomplete kernel snapshot authorized cleanup", stopped, err)
			}
		})
	}
}
