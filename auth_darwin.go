package ghalet

import "os"

func protectedSystemdCredential(*os.File, os.FileInfo) bool {
	return false
}
