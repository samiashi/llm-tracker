package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// IssueToken creates an ingest token for one of login's machines and returns
// it. Only its hash is stored, so this is the one time it can be read.
func (d *DB) IssueToken(ctx context.Context, login, hostname string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := schema.EnrolledTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	_, err := d.write.ExecContext(ctx,
		`INSERT INTO agent_token (hash, login, hostname, created_at) VALUES (?, ?, ?, ?)`,
		tokenHash(token), login, hostname, time.Now().Unix())
	if err != nil {
		return "", err
	}
	return token, nil
}

// TokenLogin returns the login an issued token belongs to, and false for one
// that was never issued or has been revoked.
func (d *DB) TokenLogin(ctx context.Context, token string) (string, bool, error) {
	var login string
	err := d.read.QueryRowContext(ctx,
		`SELECT login FROM agent_token WHERE hash = ? AND revoked_at IS NULL`,
		tokenHash(token)).Scan(&login)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return login, true, nil
}

// Revoked is what revoking a login withdrew: its live ingest tokens, and the
// machines and accounts it had claimed.
type Revoked struct{ Tokens, Machines, Accounts int64 }

// RevokeTokens revokes every live token issued to login and releases the
// machines and accounts it claimed. The release is how a machine enrolled
// under one GitHub login can upload under another.
func (d *DB) RevokeTokens(ctx context.Context, login string) (Revoked, error) {
	var r Revoked
	tx, err := d.begin(ctx)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	for _, q := range []struct {
		n    *int64
		sql  string
		args []any
	}{
		{&r.Tokens, `UPDATE agent_token SET revoked_at = ? WHERE login = ? AND revoked_at IS NULL`,
			[]any{time.Now().Unix(), login}},
		{&r.Machines, `UPDATE machine SET login = NULL WHERE login = ?`, []any{login}},
		{&r.Accounts, `UPDATE account SET login = NULL WHERE login = ?`, []any{login}},
	} {
		res, err := tx.ExecContext(ctx, q.sql, q.args...)
		if err != nil {
			return r, err
		}
		if *q.n, err = res.RowsAffected(); err != nil {
			return r, err
		}
	}
	return r, tx.Commit()
}

// tokenHash is a plain SHA-256: a token is 256 random bits, so there is
// nothing for a slow hash to protect against guessing.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
