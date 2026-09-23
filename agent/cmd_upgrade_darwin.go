package main

import "syscall"

// appleSilicon reports whether this Mac is Apple Silicon. hw.optional.arm64
// describes the hardware, so it holds for a binary running under Rosetta,
// whose runtime.GOARCH is amd64; an Intel Mac has no such key.
func appleSilicon() bool {
	v, err := syscall.SysctlUint32("hw.optional.arm64")
	return err == nil && v == 1
}
