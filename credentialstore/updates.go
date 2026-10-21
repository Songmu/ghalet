package credentialstore

import (
	"errors"
	"fmt"
	"os"
)

type credentialUpdates struct {
	writeAtomic func(string, []byte, os.FileMode) error
	changes     []credentialFile
}

func (u *credentialUpdates) write(original credentialFile, data []byte, mode os.FileMode) error {
	if err := u.writeAtomic(original.path, data, mode); err != nil {
		return err
	}
	u.changes = append(u.changes, original)
	return nil
}

func (u *credentialUpdates) rollback(cause error, previousPath, description string) error {
	restoreFailed := false
	for i := len(u.changes) - 1; i >= 0; i-- {
		original := u.changes[i]
		// Keep the recovery copy if any later write could not be restored.
		if original.path == previousPath && restoreFailed {
			cause = errors.Join(cause, fmt.Errorf("%s retained at %s", description, previousPath))
			continue
		}
		var err error
		if original.exists {
			err = u.writeAtomic(original.path, original.data, original.mode)
		} else {
			err = os.Remove(original.path)
		}
		if err != nil {
			restoreFailed = true
			cause = errors.Join(cause, fmt.Errorf("restore %s: %w", original.path, err))
		}
	}
	return cause
}
