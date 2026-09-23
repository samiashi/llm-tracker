package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/samiashi/llm-tracker/schema"
	"github.com/samiashi/llm-tracker/server/internal/auth"
)

// Verifier says who a GitHub token belongs to and whether they may enrol, with
// an empty login when GitHub rejects the token: auth.Authenticator admits the
// org's members, auth.Local, on a loopback server, whoever GitHub names.
type Verifier interface {
	Member(ctx context.Context, token string) (login string, member bool, err error)
}

// maxEnrollBytes bounds a request that carries one hostname.
const maxEnrollBytes = 4 << 10

// limiter counts enrolment attempts, each of which spends the caller's token
// on GitHub, per client address and in all (see auth.Limiter).
type limiter = auth.Limiter

// handleEnroll trades a GitHub token for an ingest token of the machine's own,
// if the token's owner is in the org -- the test the dashboard applies. The
// GitHub token is spent on that check and neither kept nor logged; the one
// issued is kept only as a hash, and `-revoke` withdraws it.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
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
	if !s.enrolments.Allow(r) {
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
		Token: token, Login: login,
	})
}
