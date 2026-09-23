package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogRotatesAtTheCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")

	const maxBytes = 1024
	lf, err := OpenLog(path, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()

	line := strings.Repeat("x", 100) + "\n"
	for range 100 {
		if _, err := lf.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > maxBytes {
		t.Errorf("current log is %d bytes, above the %d cap", fi.Size(), maxBytes)
	}

	// Exactly one previous generation is kept.
	prev, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("no rotated generation: %v", err)
	}
	if prev.Size() > maxBytes {
		t.Errorf("rotated log is %d bytes, above the %d cap", prev.Size(), maxBytes)
	}
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Error("a second generation was kept; the budget is two files")
	}

	// 10,000 bytes written against a 1KB cap must not be sitting on disk.
	total := fi.Size() + prev.Size()
	if total > 2*maxBytes {
		t.Errorf("total on disk is %d bytes, want at most %d", total, 2*maxBytes)
	}
}

// Reopening must append rather than truncate, or a restart loses the tail
// that explains why it restarted.
func TestLogAppendsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")

	lf, err := OpenLog(path, DefaultLogMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lf.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	lf.Close()

	lf2, err := OpenLog(path, DefaultLogMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer lf2.Close()
	if _, err := lf2.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != "first\nsecond\n" {
		t.Fatalf("log = %q, want both lines", got)
	}
}

// A briefly unwritable directory stands in for a full disk or a permissions
// change.
func TestAFailedRotationKeepsLogging(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	lf, err := OpenLog(path, 256)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()

	filler := strings.Repeat("x", 100) + "\n"
	for range 2 {
		if _, err := lf.Write([]byte(filler)); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	// Past the cap, so this write rotates -- and the rename fails.
	_, during := lf.Write([]byte("written while rotation fails " + filler))
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if during != nil {
		t.Fatalf("a failed rotation lost the line that prompted it: %v", during)
	}

	if _, err := lf.Write([]byte("written after recovery\n")); err != nil {
		t.Fatalf("logging did not recover once rotation could succeed: %v", err)
	}

	prev, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("no rotated generation after recovery: %v", err)
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prev), "written while rotation fails") {
		t.Errorf("the line written during the failure is missing from %s.1", path)
	}
	if string(cur) != "written after recovery\n" {
		t.Errorf("current log = %q, want only the line written after rotating", cur)
	}
}
