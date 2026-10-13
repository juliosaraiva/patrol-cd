//go:build unix && !linux

package sandbox

import "syscall"

const fdDir = "/dev/fd"

// setAddressSpaceLimit is best effort outside Linux: macOS accepts but does not
// enforce RLIMIT_AS, and some BSDs reject values below the current usage. Failures
// are ignored so the command still runs with its other limits.
func setAddressSpaceLimit(bytes uint64) error {
	_ = lowerRlimit(syscall.RLIMIT_AS, bytes, bytes)
	return nil
}
