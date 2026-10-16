//go:build !windows

package config

import (
	"fmt"
	"os"
)

// checkPermissions refuses a configuration file that group or others may access (it
// holds session tokens).
func checkPermissions(fi os.FileInfo) error {
	if p := fi.Mode().Perm(); p == 0o600 || p == 0o400 {
		return nil
	}
	return fmt.Errorf("%w %s, change the file permission either to 0400 or 0600", ErrInsecurePermissions, fi.Mode().Perm().String())
}
