// Package db owns the server's storage and the queries the dashboard runs.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/samiashi/llm-tracker/schema"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB holds separate read and write pools over the same SQLite file.
//
// SQLite in WAL mode serves many readers beside one writer, but a single
// pooled connection serialises them, queueing every dashboard read behind the
// write in flight. Writes go through a pool capped at one: SQLite permits
// exactly one writer, and a pool that discovers that gets SQLITE_BUSY instead
// of a queue.
type DB struct {
	read   *sql.DB
	write  *sql.DB
	prices *schema.PriceTable

	// Pruning reports whether this server still deletes raw events on a
	// schedule. While it does, ingest refuses events below the retention
	// floor: re-admitting a day about to be rolled up again is churn.
	Pruning bool
}

// Open connects, migrates, and loads the price table.
func Open(dsn string) (*DB, error) {
	// Pages are read through one memory map of the whole file, which every
	// connection shares with the OS cache, rather than copied into a cache
	// per connection: nine private 64MB caches, which this driver allocates
	// at twice their size, took one poll past 1.5GB on a 2GB server.
	// 2147418112 is the largest map the driver takes; negative cache_size is
	// in KiB.
	const opts = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)&_pragma=cache_size(-16384)" +
		"&_pragma=mmap_size(2147418112)"

	// The writer keeps temp storage, its statement journals included, in
	// memory: on a file, a batch of 2,000 new events took 2.6 times as long.
	write, err := sql.Open("sqlite", dsn+opts+"&_pragma=temp_store(2)")
	if err != nil {
		return nil, err
	}
	write.SetMaxOpenConns(1)

	goose.SetBaseFS(migrationFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return nil, err
	}
	if err := goose.Up(write, "migrations"); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	// Opened after migrations so readers never see a half-migrated schema.
	// Readers sort to a file, the default: ranking sessions over "All" sorts
	// every stored event, and in memory that grows with the history.
	read, err := sql.Open("sqlite", dsn+opts)
	if err != nil {
		return nil, err
	}
	read.SetMaxOpenConns(8)
	// Idle must match open, or Go's default of 2 closes and reopens six of
	// them under load -- and SQLite's page cache is per-connection, so each
	// reopened one starts cold and re-runs the pragmas above.
	read.SetMaxIdleConns(8)

	return &DB{read: read, write: write, prices: schema.DefaultPriceTable()}, nil
}

func (d *DB) Close() error {
	rerr := d.read.Close()
	if werr := d.write.Close(); werr != nil {
		return werr
	}
	return rerr
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// begin starts an immediate transaction, taking the write lock at once. A
// deferred one that reads first must upgrade later, and against a concurrent
// writer that upgrade fails with SQLITE_BUSY_SNAPSHOT, which busy_timeout does
// not retry: a prune beside a live server would never commit.
func (d *DB) begin(ctx context.Context) (*sql.Tx, error) {
	tx, err := d.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `ROLLBACK; BEGIN IMMEDIATE`); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// PriceTableVersion names the price table events are priced with.
func (d *DB) PriceTableVersion() string { return d.prices.Version }

// queryRower reads one row: the read pool, or a transaction that must see its
// own writes.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// setting reads one setting, "" when unset.
func setting(ctx context.Context, q queryRower, key string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM setting WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// dayStartUnix converts an ISO day to a unix timestamp at UTC midnight,
// returning 0 for anything unparseable so a bad value deletes nothing.
func dayStartUnix(day string) int64 {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return 0
	}
	return t.Unix()
}
