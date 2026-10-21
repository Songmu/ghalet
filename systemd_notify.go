package ghalet

import (
	"net"
	"os"
	"time"
)

func notifyReady() error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	connection, err := net.DialTimeout("unixgram", socket, time.Second)
	if err != nil {
		return err
	}
	defer connection.Close()
	if err := connection.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	_, err = connection.Write([]byte("READY=1"))
	return err
}
