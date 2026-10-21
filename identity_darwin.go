package ghalet

import "golang.org/x/sys/unix"

func socketPeerUID(fd int) (int, error) {
	credentials, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, err
	}
	return int(credentials.Uid), nil
}
