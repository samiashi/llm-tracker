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

// RevokeTokens revokes every live token issued to login and says how many
// there were. Enrolment checks membership once, so this is how someone who
// leaves the org stops uploading.
func (d *DB) RevokeTokens(ctx context.Context, login string) (int64, error) {
	res, err := d.write.ExecContext(ctx,
		`UPDATE agent_token SET revoked_at = ? WHERE login = ? AND revoked_at IS NULL`,
		time.Now().Unix(), login)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// tokenHash is a plain SHA-256: a token is 256 random bits, so there is
// nothing for a slow hash to protect against guessing.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
