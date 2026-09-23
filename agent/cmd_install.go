package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/samiashi/llm-tracker/agent/internal/launchd"
)

func cmdInstall(dataDir string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	bin, err := installBinary(src, dataDir)
	if err != nil {
		return err
	}

	p, err := launchd.Install(bin, dataDir)
	if err != nil {
		return err
	}
	fmt.Println("installed and started:", p)
	fmt.Println("binary:", bin)
	fmt.Println("logs:", filepath.Join(dataDir, "agent.log"))
	return nil
}

// installBinary copies the running executable into the data directory and
// returns the stable path launchd should start. Not the path `install` ran
// from: a repo's bin/ is deleted by `make clean` or invalidated by a moved
// checkout, and launchd then retries the missing binary forever.
func installBinary(src, dataDir string) (string, error) {
	binDir := filepath.Join(dataDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(binDir, "llm-tracker-agent")

	// Already running from the install location: nothing to copy, and
	// copying a file onto itself would truncate it.
	if sameFile(src, dst) {
		return dst, nil
	}

	in, err := os.Open(src) //nolint:gosec // our own executable
	if err != nil {
		return "", err
	}
	defer in.Close() //nolint:errcheck

	// Staged then renamed, so a failure part-way through cannot leave a
	// half-written binary where launchd will try to start one.
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o700) //nolint:gosec
	if err != nil {
		return "", err
	}
	// discard removes the staged file after a failed copy. Its own errors are
	// dropped: the copy has already failed, and launchd never starts the temp
	// name.
	discard := func(e error) (string, error) {
		_ = out.Close()
		_ = os.Remove(tmp)
		return "", e
	}
	if _, err := io.Copy(out, in); err != nil {
		return discard(err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return dst, nil
}

// sameFile reports whether two paths are the same file on disk.
func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

func cmdUninstall() error {
	p, err := launchd.Uninstall()
	if err != nil {
		return err
	}
	fmt.Println("removed:", p)
	return nil
}
