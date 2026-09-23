package config

import (
	"io"
	"os"
	"path/filepath"
	"sync"
)

// RotatingFile is a size-capped log file. One previous generation is kept:
// enough for a diagnostic tail to survive a restart mid-incident.
//
// launchd does not rotate StandardOutPath or StandardErrorPath, and the
// per-file error path logs one line per skipped file per pass: a permissions
// change, a full disk or an unmounted volume makes every file fail at once, and
// the log grows by hundreds of megabytes a day, which worsens a full disk.
type RotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	n        int64
	f        *os.File
}

// LogPath is the daemon's log in a data directory: the file it writes and the
// one launchd's plist names.
func LogPath(dataDir string) string { return filepath.Join(dataDir, "agent.log") }

// DefaultLogMaxBytes bounds one generation. Two of these is the worst case on
// disk.
const DefaultLogMaxBytes = 8 << 20

// OpenLog opens path for appending, rotating it when it passes maxBytes.
func OpenLog(path string, maxBytes int64) (*RotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	var n int64
	if fi, serr := f.Stat(); serr == nil {
		n = fi.Size()
	}
	return &RotatingFile{path: path, maxBytes: maxBytes, n: n, f: f}, nil
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.n+int64(len(p)) > r.maxBytes {
		// A failed rotation leaves the current file open and is retried on
		// the next write, so the line that prompted it is not lost.
		_ = r.rotate()
	}
	n, err := r.f.Write(p)
	r.n += int64(n)
	return n, err
}

// rotate moves the current file aside and starts a new one. The caller holds
// the lock.
//
// The old handle is closed only once the new file is open: closed first, a
// failed rename or open leaves nothing to write to until the daemon restarts.
func (r *RotatingFile) rotate() error {
	// Rename over any previous generation: two files is the whole budget. A
	// missing file was already moved aside by a rotation whose open then
	// failed.
	if err := os.Rename(r.path, r.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_ = r.f.Close()
	r.f, r.n = f, 0
	return nil
}

func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

var _ io.WriteCloser = (*RotatingFile)(nil)
