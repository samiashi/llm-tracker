package db

import (
	"context"
	"database/sql"
	"testing"
)

// Memory is the server's scarcest resource: nine connections each holding a
// private 64MB cache, and sorts kept in memory, took one poll past 1.5GB on a
// 2GB VM. Every connection must read through the shared map of the whole file
// instead, with a small cache of its own; readers sort to a file, and only the
// writer keeps its statement journals in memory, which ingest needs.
func TestEveryConnectionReadsThroughTheMapWithASmallCache(t *testing.T) {
	d := newDB(t)
	for name, pool := range map[string]*sql.DB{"read": d.read, "write": d.write} {
		conn, err := pool.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		pragma := func(p string) int64 {
			t.Helper()
			var v int64
			if err := conn.QueryRowContext(context.Background(), "PRAGMA "+p).Scan(&v); err != nil {
				t.Fatal(err)
			}
			return v
		}
		if mmap := pragma("mmap_size"); mmap < 2147418112 {
			t.Errorf("%s pool maps %d bytes; files past that are read into private caches", name, mmap)
		}
		// Negative is KiB, positive is pages; either way, no more than 16MB.
		if cache := pragma("cache_size"); cache < -16384 || cache > 16384*1024/4096 {
			t.Errorf("%s pool keeps a private cache of %d", name, cache)
		}
		if inMemory := pragma("temp_store") == 2; inMemory != (name == "write") {
			t.Errorf("%s pool keeps temp storage in memory: %v", name, inMemory)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
