package ghalet

import (
	"encoding/binary"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

type credentialACLEntry struct {
	tag, perm uint16
	id        uint32
}

func credentialACL(entries ...credentialACLEntry) []byte {
	data := binary.LittleEndian.AppendUint32(nil, 2)
	for _, entry := range entries {
		data = binary.LittleEndian.AppendUint16(data, entry.tag)
		data = binary.LittleEndian.AppendUint16(data, entry.perm)
		data = binary.LittleEndian.AppendUint32(data, entry.id)
	}
	return data
}

func systemdCredentialACL(uid uint32) []credentialACLEntry {
	return []credentialACLEntry{
		{aclUserObj, 4, ^uint32(0)}, {aclUser, 4, uid},
		{aclGroupObj, 0, ^uint32(0)}, {aclMask, 4, ^uint32(0)},
		{aclOther, 0, ^uint32(0)},
	}
}

func TestProtectedCredentialACL(t *testing.T) {
	const uid = 12345
	for _, tc := range []struct {
		name string
		edit func([]credentialACLEntry) []credentialACLEntry
		want bool
	}{
		{name: "systemd", want: true},
		{name: "masked permissions", want: true, edit: func(a []credentialACLEntry) []credentialACLEntry {
			a[1].perm = 7
			a[2].perm = 3
			return append(a[:3], append([]credentialACLEntry{{aclGroup, 3, 54321}}, a[3:]...)...)
		}},
		{name: "unrelated user", edit: func(a []credentialACLEntry) []credentialACLEntry { a[1].id++; return a }},
		{name: "owning group", edit: func(a []credentialACLEntry) []credentialACLEntry { a[2].perm = 4; return a }},
		{name: "named group", edit: func(a []credentialACLEntry) []credentialACLEntry {
			return append(a[:3], append([]credentialACLEntry{{aclGroup, 4, 54321}}, a[3:]...)...)
		}},
		{name: "other", edit: func(a []credentialACLEntry) []credentialACLEntry { a[4].perm = 4; return a }},
		{name: "writable mask", edit: func(a []credentialACLEntry) []credentialACLEntry { a[3].perm = 6; return a }},
		{name: "missing mask", edit: func(a []credentialACLEntry) []credentialACLEntry { return append(a[:3], a[4:]...) }},
		{name: "duplicate owner", edit: func(a []credentialACLEntry) []credentialACLEntry { return append(a, a[0]) }},
		{name: "unknown tag", edit: func(a []credentialACLEntry) []credentialACLEntry { a[2].tag = 99; return a }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := systemdCredentialACL(uid)
			if tc.edit != nil {
				entries = tc.edit(entries)
			}
			if got := protectedCredentialACL(credentialACL(entries...), uid); got != tc.want {
				t.Fatalf("accepted=%v, want %v", got, tc.want)
			}
		})
	}
	for _, acl := range [][]byte{nil, {2, 0, 0, 0, 1}, {1, 0, 0, 0}} {
		if protectedCredentialACL(acl, uid) {
			t.Fatal("malformed ACL accepted")
		}
	}
}

func TestProtectedSystemdCredentialHelper(t *testing.T) {
	path := os.Getenv("GHALET_TEST_CREDENTIAL_PATH")
	if path == "" {
		t.Skip("credential subprocess only")
	}
	value, err := readProtected(path)
	if os.Getenv("GHALET_TEST_CREDENTIAL_REJECT") == "true" {
		if err == nil {
			t.Fatal("unsafe credential accepted")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if value != "dummy-credential" {
		t.Fatal("unexpected dummy credential contents")
	}
	if os.Geteuid() == 0 {
		t.Fatal("credential reader must be non-root")
	}
}

func TestProtectedSystemdCredentialAccess(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("GHALET_TEST_SYSTEMD_CREDENTIALS") != "1" {
		t.Skip("requires root on a disposable Linux host/container and GHALET_TEST_SYSTEMD_CREDENTIALS=1")
	}
	account, err := user.Lookup("nobody")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	if uid == 0 {
		t.Fatal("expected a non-root fixture user")
	}
	if err := os.MkdirAll("/run/credentials", 0755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/run/credentials", "ghalet-acl-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		want    bool
		outside bool
		edit    func(string, []credentialACLEntry) []credentialACLEntry
	}{
		{name: "root ACL", want: true},
		{name: "masked group write", want: true, edit: func(_ string, a []credentialACLEntry) []credentialACLEntry {
			a[2].perm = 2
			return a
		}},
		{name: "group readable without ACL", edit: func(path string, _ []credentialACLEntry) []credentialACLEntry {
			if err := os.Chmod(path, 0440); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{name: "unrelated UID", edit: func(_ string, a []credentialACLEntry) []credentialACLEntry {
			return append(a[:2], append([]credentialACLEntry{{aclUser, 4, uint32(uid + 1)}}, a[2:]...)...)
		}},
		{name: "owning group", edit: func(_ string, a []credentialACLEntry) []credentialACLEntry { a[2].perm = 4; return a }},
		{name: "named group", edit: func(_ string, a []credentialACLEntry) []credentialACLEntry {
			return append(a[:3], append([]credentialACLEntry{{aclGroup, 4, uint32(gid)}}, a[3:]...)...)
		}},
		{name: "other", edit: func(_ string, a []credentialACLEntry) []credentialACLEntry { a[4].perm = 4; return a }},
		{name: "non-root owner", edit: func(path string, a []credentialACLEntry) []credentialACLEntry {
			if err := os.Chown(path, int(uid), int(gid)); err != nil {
				t.Fatal(err)
			}
			return a
		}},
		{name: "writable parent", edit: func(_ string, a []credentialACLEntry) []credentialACLEntry {
			if err := os.Chmod(dir, 0777); err != nil {
				t.Fatal(err)
			}
			return a
		}},
		{name: "symlink", edit: func(path string, a []credentialACLEntry) []credentialACLEntry {
			if err := os.Rename(path, path+"-real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+"-real", path); err != nil {
				t.Fatal(err)
			}
			return a
		}},
		{name: "outside credentials", outside: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer os.Chmod(dir, 0755)
			path := filepath.Join(dir, "dummy")
			if tc.outside {
				outside, err := os.MkdirTemp("/run", "ghalet-acl-test-")
				if err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(outside)
				if err := os.Chmod(outside, 0755); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(outside, "dummy")
			}
			if err := os.WriteFile(path, []byte("dummy-credential\n"), 0600); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(path)
			defer os.Remove(path + "-real")
			if err := os.Chown(path, 0, int(gid)); err != nil {
				t.Fatal(err)
			}
			entries := systemdCredentialACL(uint32(uid))
			if tc.edit != nil {
				entries = tc.edit(path, entries)
			}
			if entries != nil {
				if err := unix.Setxattr(path, "system.posix_acl_access", credentialACL(entries...), 0); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command(executable, "-test.run=^TestProtectedSystemdCredentialHelper$", "-test.v")
			command.Env = append(os.Environ(), "GHALET_TEST_CREDENTIAL_PATH="+path,
				"GHALET_TEST_CREDENTIAL_REJECT="+strconv.FormatBool(!tc.want))
			command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("non-root reader: %v\n%s", err, output)
			}
		})
	}
}

func TestProtectedSystemdEncryptedCredential(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("GHALET_TEST_SYSTEMD_CREDENTIALS") != "1" {
		t.Skip("requires root, systemd and GHALET_TEST_SYSTEMD_CREDENTIALS=1 on a disposable Linux host")
	}
	dir, err := os.MkdirTemp("", "ghalet-encrypted-credential-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cipher := filepath.Join(dir, "dummy.cred")
	command := exec.Command("systemd-creds", "encrypt", "--name=dummy", "--with-key=host", "-", cipher)
	command.Stdin = strings.NewReader("dummy-credential\n")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("encrypt dummy credential: %v\n%s", err, output)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The unit's non-root user must be able to execute the test binary even
	// when go test placed the original under a private build directory.
	reader, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "credential-test")
	if err := os.WriteFile(binary, reader, 0755); err != nil {
		t.Fatal(err)
	}
	unit := "ghalet-credential-test-" + strconv.Itoa(os.Getpid()) + ".service"
	command = exec.Command("systemd-run", "--quiet", "--wait", "--pipe", "--collect", "--unit="+unit,
		"--property=User=nobody", "--property=LoadCredentialEncrypted=dummy:"+cipher,
		"--setenv=GHALET_TEST_CREDENTIAL_PATH=/run/credentials/"+unit+"/dummy",
		binary, "-test.run=^TestProtectedSystemdCredentialHelper$", "-test.v")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("non-root encrypted credential service: %v\n%s", err, output)
	}
}
