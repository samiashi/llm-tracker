//go:build !darwin

package main

// appleSilicon exists off macOS only so the agent compiles for Linux CI; the
// collector runs on Macs alone.
func appleSilicon() bool { return false }
