//go:build linux

package sandbox

import "syscall"

const fdDir = "/proc/self/fd"

// setAddressSpaceLimit enforces RLIMIT_AS on Linux.
func setAddressSpaceLimit(bytes uint64) error {
	return lowerRlimit(syscall.RLIMIT_AS, bytes, bytes)
}
