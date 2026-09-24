package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/samiashi/llm-tracker/schema"
)

// Verifier names the GitHub user a token belongs to, with an empty login when
// GitHub rejects the token (see auth.Local).
type Verifier interface {
	Login(ctx context.Context, token string) (string, error)
}

// maxEnrollBytes bounds a request that carries one hostname.
const maxEnrollBytes = 4 << 10

// handleEnroll trades a GitHub token for an ingest token of the machine's own,
// issued to the login GitHub names. The GitHub token is spent on that one
// question and neither kept nor logged; the one issued is kept only as a
// hash, and `-revoke` withdraws it.
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
	login, err := s.Enroll.Login(r.Context(), ghToken)
	switch {
	case err != nil:
		s.Log.Error("enroll: asking GitHub whose token this is", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "could not ask GitHub whose token this is; try again shortly",
		})
		return
	case login == "":
		writeErr(w, http.StatusUnauthorized, errors.New(
			"GitHub rejected the token; run `gh auth login` and try again"))
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
