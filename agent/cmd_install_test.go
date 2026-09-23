package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The copy is staged and renamed into place, and the staged file never
// outlives the call: whether the install lands or fails, only the binary
// launchd starts is left in bin/.
func TestInstallLeavesNoStagedCopyBehind(t *testing.T) {
	src := filepath.Join(t.TempDir(), "llm-tracker-agent")
	if err := os.WriteFile(src, []byte("pretend this is a mach-o binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("installed", func(t *testing.T) {
		dir := t.TempDir()
		dst, err := installBinary(src, dir)
		if err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(dst); err != nil || string(b) != "pretend this is a mach-o binary" {
			t.Fatalf("installed %q (%v), want the running binary's bytes", b, err)
		}
		if _, err := os.Stat(dst + ".new"); !os.IsNotExist(err) {
			t.Fatalf("the staged copy is still there: %v", err)
		}
	})

	t.Run("the rename fails", func(t *testing.T) {
		dir := t.TempDir()
		// A directory where the binary goes: the rename over it fails.
		if err := os.MkdirAll(filepath.Join(dir, "bin", "llm-tracker-agent", "x"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := installBinary(src, dir); err == nil {
			t.Fatal("installed over a directory")
		}
		if _, err := os.Stat(filepath.Join(dir, "bin", "llm-tracker-agent.new")); !os.IsNotExist(err) {
			t.Fatalf("a failed install left its staged copy: %v", err)
		}
	})
}
