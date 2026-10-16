//go:build windows

package config

import "os"

// checkPermissions is a no-op on Windows, where POSIX mode bits do not describe access
// (the file is created in the user's profile directory).
func checkPermissions(os.FileInfo) error { return nil }
