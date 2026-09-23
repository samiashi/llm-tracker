package store

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// Health is what the agent records about its last pass and sync, so `status`
// can tell a rejected or silent agent from a healthy one: otherwise the only
// moving number is "awaiting upload", which rises either way.
type Health struct {
	LastPassAt   time.Time
	LastSyncAt   time.Time
	LastSyncErr  string
	LastFound    int
	LastStored   int
	LastUploaded int
}

const (
	keyLastPassAt   = "health:last_pass_at"
	keyLastSyncAt   = "health:last_sync_at"
	keyLastSyncErr  = "health:last_sync_err"
	keyLastFound    = "health:last_found"
	keyLastStored   = "health:last_stored"
	keyLastUploaded = "health:last_uploaded"
)

// RecordPass notes that a collection pass finished.
func (s *Store) RecordPass(ctx context.Context, found, stored int) error {
	if err := s.SetMeta(ctx, keyLastPassAt, strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		return err
	}
	if err := s.SetMeta(ctx, keyLastFound, strconv.Itoa(found)); err != nil {
		return err
	}
	return s.SetMeta(ctx, keyLastStored, strconv.Itoa(stored))
}

// RecordSync notes the outcome of an upload. A nil error clears the last
// failure, so a recovered agent stops reporting one.
func (s *Store) RecordSync(ctx context.Context, uploaded int, err error) error {
	msg := ""
	if err != nil {
		// Bounded: the server's error text is remote-controlled and ends up
		// in a status the operator reads.
		msg = err.Error()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		msg = strings.ReplaceAll(msg, "\n", " ")
	} else if serr := s.SetMeta(ctx, keyLastSyncAt,
		strconv.FormatInt(time.Now().Unix(), 10)); serr != nil {
		return serr
	}
	if uerr := s.SetMeta(ctx, keyLastUploaded, strconv.Itoa(uploaded)); uerr != nil {
		return uerr
	}
	return s.SetMeta(ctx, keyLastSyncErr, msg)
}

// Health reads back what the last pass and the last sync did.
func (s *Store) Health(ctx context.Context) (Health, error) {
	var h Health
	unix := func(k string) time.Time {
		v, err := s.Meta(ctx, k)
		if err != nil || v == "" {
			return time.Time{}
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return time.Time{}
		}
		return time.Unix(n, 0)
	}
	num := func(k string) int {
		v, err := s.Meta(ctx, k)
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(v)
		return n
	}

	h.LastPassAt = unix(keyLastPassAt)
	h.LastSyncAt = unix(keyLastSyncAt)
	h.LastFound = num(keyLastFound)
	h.LastStored = num(keyLastStored)
	h.LastUploaded = num(keyLastUploaded)
	msg, err := s.Meta(ctx, keyLastSyncErr)
	if err != nil {
		return h, err
	}
	h.LastSyncErr = msg
	return h, nil
}
