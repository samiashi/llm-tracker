package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrLocked means another process holds the data directory. The caller says
// what to do about it, since only the caller knows which command was refused.
var ErrLocked = errors.New("another llm-tracker-agent is using this data directory")

// Lock is an exclusive hold on a data directory.
type Lock struct{ f *os.File }

// Acquire takes an exclusive lock on dataDir, or fails with ErrLocked.
//
// Two writers contend for SQLite's one write lock, and a resync that runs under
// a daemon's pass loses what it deleted: the pass then commits its cursors past
// rows that no longer exist. flock rather than a pidfile, because the kernel
// drops it when the process dies and a crash leaves nothing stale.
func Acquire(dataDir string) (*Lock, error) {
	path := filepath.Join(dataDir, "agent.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := io.ReadAll(io.LimitReader(f, 32))
		_ = f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if pid := strings.TrimSpace(string(holder)); pid != "" {
			return nil, fmt.Errorf("%w (%s, pid %s)", ErrLocked, dataDir, pid)
		}
		return nil, fmt.Errorf("%w (%s)", ErrLocked, dataDir)
	}
	// Recorded for a human, and for the error above, not for the locking.
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return &Lock{f: f}, nil
}

// Release drops the lock, which the kernel would otherwise do at exit.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	return l.f.Close()
}
