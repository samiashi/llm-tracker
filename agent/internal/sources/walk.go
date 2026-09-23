package sources

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/samiashi/llm-tracker/agent/internal/store"
)

// maxWholeFileBytes caps an adapter that must read a file entire rather than
// tailing it. Generous enough for any real session aggregate, small enough
// that a runaway file cannot be pulled into a background daemon's memory.
const maxWholeFileBytes = 64 << 20

// tailJSONL reads only the bytes appended to path since the last pass and
// hands each complete line to fn.
//
// Three details matter. A line that is still being written is left alone, so
// the cursor never lands mid-record and the partial line is picked up whole on
// the next pass. A file that has shrunk was rotated or replaced, so reading
// restarts from zero, and rewound says so: that position must be committed
// even when no complete line follows it, or once the replacement outgrows the
// old file the next pass resumes at the old offset, inside it. And lines are
// read with ReadBytes rather than a Scanner because Claude Code records
// routinely exceed bufio.Scanner's limit, which would otherwise abort the file
// with a confusing error.
func tailJSONL(ctx context.Context, st *store.Store, path string, fn func(at int64, line []byte)) (consumedBytes, newOffset, fileSize int64, rewound bool, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, 0, false, err
	}
	size := fi.Size()

	offset, lastSize, err := st.Cursor(ctx, path)
	if err != nil {
		return 0, 0, 0, false, err
	}
	if size < offset || size < lastSize {
		offset, rewound = 0, true
	}
	if offset >= size {
		return 0, offset, size, rewound, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, false, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, 0, 0, false, err
	}

	r := bufio.NewReaderSize(f, 256*1024)
	var consumed int64
	for ctx.Err() == nil {
		line, err := r.ReadBytes('\n')
		if err != nil {
			// An unterminated trailing line is an in-flight write.
			break
		}
		// The line's own start: the only identifier some formats have, and
		// the cursor guarantees each offset is read exactly once.
		at := offset + consumed
		consumed += int64(len(line))
		if t := bytes.TrimSpace(line); len(t) > 0 {
			fn(at, t)
		}
	}

	// Not persisted here: the caller commits the cursor with the rows it
	// covers, or an interrupted pass marks bytes read whose events were never
	// stored.
	return consumed, offset + consumed, size, rewound, nil
}

// walkRoot is filepath.WalkDir that also descends a root which is itself a
// symlink -- a projects directory moved to another disk -- where WalkDir alone
// visits nothing while the source still counts as present. Paths are reported
// under root as given, because cursors, meta keys and rewind scopes are keyed
// on it.
func walkRoot(root string, fn fs.WalkDirFunc) error {
	fi, err := os.Lstat(root)
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return filepath.WalkDir(root, fn)
	}
	target, err := filepath.EvalSymlinks(root)
	if err != nil {
		return filepath.WalkDir(root, fn)
	}
	return filepath.WalkDir(target, func(path string, d fs.DirEntry, err error) error {
		rel, rerr := filepath.Rel(target, path)
		if rerr != nil {
			return fn(path, d, err)
		}
		return fn(filepath.Join(root, rel), d, err)
	})
}

// walkJSONL is the shape almost every adapter needs: walk a set of roots, tail
// each matching file from wherever the last pass stopped, and hand complete
// lines to onLine. A new adapter is then usually a path, a filename predicate
// and a function that turns one line into an Event.
func walkJSONL(
	ctx context.Context,
	c *Ctx,
	roots []string,
	match func(path string) bool,
	onLine func(path string, at int64, line []byte),
) (Result, error) {
	var res Result
	for _, root := range roots {
		err := walkRoot(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !match(path) {
				return nil //nolint:nilerr // one unreadable file must not abort the walk
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}

			n, offset, size, rewound, terr := tailJSONL(ctx, c.Store, path, func(at int64, line []byte) { onLine(path, at, line) })
			if terr != nil {
				res.Errors = append(res.Errors, terr)
				// Nothing is committed, so the cursor stays where it was and
				// the file is retried whole on the next pass.
				c.discard()
				res.Files++
				return nil
			}

			// Rows and cursor move together: an interrupted pass either keeps
			// both or neither, and never advances past events it did not store.
			stored, cerr := c.commitFile(ctx, path, n > 0 || rewound, offset, size)
			if cerr != nil {
				res.Errors = append(res.Errors, cerr)
			}
			res.Stored += stored
			res.Files++
			res.BytesRead += n
			return nil
		})
		if err != nil {
			res.Unparsed = c.takeUnparsed()
			return res, err
		}
	}
	res.Unparsed = c.takeUnparsed()
	return res, nil
}

// hasExt reports whether path ends in suffix, for use as a walkJSONL match.
func hasExt(suffix string) func(string) bool {
	return func(p string) bool { return strings.HasSuffix(p, suffix) }
}

// baseName reports whether the file is named exactly name.
func baseName(name string) func(string) bool {
	return func(p string) bool { return filepath.Base(p) == name }
}

// anyOf combines matchers.
func anyOf(ms ...func(string) bool) func(string) bool {
	return func(p string) bool {
		for _, m := range ms {
			if m(p) {
				return true
			}
		}
		return false
	}
}
