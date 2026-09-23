package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// Verifier says who a GitHub token belongs to and whether they are in the
// org, with an empty login when GitHub rejects the token. auth.Authenticator
// is the one main passes.
type Verifier interface {
	Member(ctx context.Context, token string) (login string, member bool, err error)
}

const (
	// maxEnrollBytes bounds a request that carries one hostname.
	maxEnrollBytes = 4 << 10
	// maxEnrolments bounds enrolment attempts across all callers, per minute.
	// Each one spends the caller's token on GitHub, and GitHub meets a run of
	// bad credentials by refusing this address for a while, which would take
	// dashboard logins down with it. A whole team installing at once is well
	// inside it.
	maxEnrolments = 30
)

// handleEnroll trades a GitHub token for an ingest token of the machine's own,
// if the token's owner is in the org -- the test the dashboard applies. The
// GitHub token is spent on that check and neither kept nor logged; the one
// issued is kept only as a hash, and `-revoke` withdraws it.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	// As for ingest: a request a page could send without a preflight is
	// refused before it costs anything.
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType,
			errors.New("enroll requires Content-Type: application/json"))
		return
	}
	ghToken, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || ghToken == "" {
		writeErr(w, http.StatusUnauthorized, errors.New("enroll takes a GitHub token as the bearer"))
		return
	}
	var in schema.EnrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEnrollBytes)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("malformed request body"))
		return
	}
	if !s.enrolments.allow(time.Now(), maxEnrolments, time.Minute) {
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests, errors.New("too many enrolments at once; try again in a minute"))
		return
	}

	login, member, err := s.Enroll.Member(r.Context(), ghToken)
	switch {
	case err != nil:
		s.Log.Error("enroll: checking org membership", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "could not confirm org membership with GitHub; try again shortly",
		})
		return
	case login == "":
		writeErr(w, http.StatusUnauthorized, errors.New(
			"GitHub rejected the token; run `gh auth login` and try again"))
		return
	case !member:
		s.Log.Warn("enroll: refused, not an org member", "login", login)
		writeErr(w, http.StatusForbidden, fmt.Errorf(
			"%s is not a member of the GitHub organisation this tracker serves", login))
		return
	}

	hostname := clip(in.Hostname, maxFieldLen)
	token, err := s.DB.IssueToken(r.Context(), login, hostname)
	if err != nil {
		s.writeInternal(w, "enroll", err)
		return
	}
	s.Log.Info("enrolled", "login", login, "hostname", hostname)
	writeJSON(w, http.StatusOK, schema.EnrollResponse{
		Token: token, Login: login, ServerVersion: s.Version,
	})
}

// limiter counts events in fixed windows. Its zero value is ready to use.
type limiter struct {
	mu    sync.Mutex
	start time.Time
	n     int
}

// allow reports whether one more event fits in the current window of length
// per, counting it if so.
func (l *limiter) allow(now time.Time, limit int, per time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.start) >= per {
		l.start, l.n = now, 0
	}
	if l.n >= limit {
		return false
	}
	l.n++
	return true
}
