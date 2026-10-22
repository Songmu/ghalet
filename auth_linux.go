package ghalet

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Tags from Linux's include/uapi/linux/posix_acl.h.
const (
	aclUserObj  = 0x01
	aclUser     = 0x02
	aclGroupObj = 0x04
	aclGroup    = 0x08
	aclMask     = 0x10
	aclOther    = 0x20
)

func protectedSystemdCredential(file *os.File, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm() != 0440 {
		return false
	}
	path := file.Name()
	parts := strings.Split(strings.TrimPrefix(path, "/run/credentials/"), "/")
	if !strings.HasPrefix(path, "/run/credentials/") || len(parts) != 2 ||
		parts[0] == "" || parts[1] == "" || filepath.Clean(path) != path {
		return false
	}
	// Root-owned, non-writable ancestors make the path check stable against
	// replacement by the service user, including through directory ACLs.
	for _, dir := range []string{"/", "/run", "/run/credentials", filepath.Dir(path)} {
		parent, err := os.Lstat(dir)
		if err != nil || !parent.IsDir() || parent.Mode().Perm()&0022 != 0 {
			return false
		}
		owner, ok := parent.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != 0 {
			return false
		}
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return false
	}
	size, err := unix.Fgetxattr(int(file.Fd()), "system.posix_acl_access", nil)
	if err != nil || size < 4 || size > 65536 {
		return false
	}
	acl := make([]byte, size)
	n, err := unix.Fgetxattr(int(file.Fd()), "system.posix_acl_access", acl)
	return err == nil && protectedCredentialACL(acl[:n], uint32(os.Geteuid()))
}

func protectedCredentialACL(acl []byte, uid uint32) bool {
	// Linux's POSIX ACL xattr uses a little-endian version header followed by
	// (tag, permissions, ID) entries. Group mode bits represent the mask.
	if len(acl) < 4 || (len(acl)-4)%8 != 0 || binary.LittleEndian.Uint32(acl) != 2 {
		return false
	}
	var owner, group, mask, other, user bool
	for offset := 4; offset < len(acl); offset += 8 {
		tag := binary.LittleEndian.Uint16(acl[offset:])
		perm := binary.LittleEndian.Uint16(acl[offset+2:])
		id := binary.LittleEndian.Uint32(acl[offset+4:])
		if perm&^uint16(7) != 0 {
			return false
		}
		switch tag {
		case aclUserObj:
			if owner || perm != 4 {
				return false
			}
			owner = true
		case aclUser:
			if id == uid && uid != 0 {
				if user || perm&4 != 4 {
					return false
				}
				user = true
			} else if perm&4 != 0 {
				return false
			}
		case aclGroupObj:
			if group || perm&4 != 0 {
				return false
			}
			group = true
		case aclGroup:
			if perm&4 != 0 {
				return false
			}
		case aclMask:
			if mask || perm != 4 {
				return false
			}
			mask = true
		case aclOther:
			if other || perm != 0 {
				return false
			}
			other = true
		default:
			return false
		}
	}
	return owner && group && mask && other && user
}
