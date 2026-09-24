package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samiashi/llm-tracker/schema"
)

const ghToken = "gho_colleague"

// fakeGitHub knows one token, ghToken, and names its owner alice.
type fakeGitHub struct {
	err   error
	calls int
}

func (f *fakeGitHub) Login(_ context.Context, token string) (string, error) {
	f.calls++
	switch {
	case f.err != nil:
		return "", f.err
	case token != ghToken:
		return "", nil
	}
	return "alice", nil
}

func enroll(s *Server, contentType, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/enroll", strings.NewReader(`{"hostname":"laptop"}`))
	req.Header.Set("Content-Type", contentType)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	s.Routes(http.NotFoundHandler()).ServeHTTP(rec, req)
	return rec
}

func ingestAs(s *Server, token string) int {
	req := ingestReq(`{"v":1,"machine_id":"m","events":[]}`)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Routes(http.NotFoundHandler()).ServeHTTP(rec, req)
	return rec.Code
}

func TestAnEnrolledTokenUploadsUntilItsOwnerIsRevoked(t *testing.T) {
	s := newServer(t)
	s.Enroll = &fakeGitHub{}
	s.Version = "v1.4.0"

	rec := enroll(s, "application/json", ghToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll: %d %s", rec.Code, rec.Body.String())
	}
	var got schema.EnrollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Token, schema.EnrolledTokenPrefix) || got.Login != "alice" {
		t.Fatalf("enroll answered %+v", got)
	}

	if code := ingestAs(s, got.Token); code != http.StatusOK {
		t.Fatalf("ingest with the issued token: %d", code)
	}
	if _, err := s.DB.RevokeTokens(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	if code := ingestAs(s, got.Token); code != http.StatusUnauthorized {
		t.Fatalf("ingest with a revoked token: %d, want 401", code)
	}
	// Revoking alice leaves everyone else's machines uploading.
	if code := ingestAs(s, testToken); code != http.StatusOK {
		t.Fatalf("ingest with another login's token: %d", code)
	}
}

func TestIngestRefusesATokenEnrolmentNeverIssued(t *testing.T) {
	s := newServer(t)
	if code := ingestAs(s, schema.EnrolledTokenPrefix+"made-up"); code != http.StatusUnauthorized {
		t.Fatalf("ingest: %d, want 401", code)
	}
}

// Only a token GitHub names gets one of ours, and a request refused on its
// face never reaches GitHub.
func TestEnrolmentRefusesATokenGitHubDoesNotName(t *testing.T) {
	for _, tc := range []struct {
		name        string
		verifier    *fakeGitHub
		contentType string
		bearer      string
		want        int
		asksGitHub  bool
	}{
		{"token GitHub rejects", &fakeGitHub{}, "application/json", "gho_expired", http.StatusUnauthorized, true},
		{"GitHub unreachable", &fakeGitHub{err: errors.New("timeout")}, "application/json", ghToken, http.StatusBadGateway, true},
		{"no token", &fakeGitHub{}, "application/json", "", http.StatusUnauthorized, false},
		{"form post", &fakeGitHub{}, "application/x-www-form-urlencoded", ghToken, http.StatusUnsupportedMediaType, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t)
			s.Enroll = tc.verifier
			rec := enroll(s, tc.contentType, tc.bearer)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), schema.EnrolledTokenPrefix) {
				t.Fatalf("a refused enrolment carried a token: %s", rec.Body.String())
			}
			if (tc.verifier.calls > 0) != tc.asksGitHub {
				t.Fatalf("GitHub asked %d times, want asked = %v", tc.verifier.calls, tc.asksGitHub)
			}
		})
	}
}

// The GitHub token opens everything its owner can reach, so it must not
// outlive the request, even in a log.
func TestEnrolmentNeverLogsTheGitHubToken(t *testing.T) {
	var logged bytes.Buffer
	for _, bearer := range []string{ghToken, "gho_expired"} {
		for _, v := range []*fakeGitHub{{}, {err: errors.New("github returned 502")}} {
			s := newServer(t)
			s.Log = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
			s.Enroll = v
			enroll(s, "application/json", bearer)
		}
	}
	if logged.Len() == 0 {
		t.Fatal("nothing was logged, so this proves nothing")
	}
	if strings.Contains(logged.String(), ghToken) || strings.Contains(logged.String(), "gho_expired") {
		t.Fatalf("the GitHub token reached the log:\n%s", logged.String())
	}
}
