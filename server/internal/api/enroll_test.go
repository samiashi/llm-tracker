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
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

const ghToken = "gho_colleague"

// fakeGitHub knows one token, ghToken, and says the same of its owner every
// time.
type fakeGitHub struct {
	member bool
	err    error
	calls  int
}

func (f *fakeGitHub) Member(_ context.Context, token string) (string, bool, error) {
	f.calls++
	switch {
	case f.err != nil:
		return "", false, f.err
	case token != ghToken:
		return "", false, nil
	}
	return "alice", f.member, nil
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
	s.Enroll = &fakeGitHub{member: true}
	s.Version = "v1.4.0"

	rec := enroll(s, "application/json", ghToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll: %d %s", rec.Code, rec.Body.String())
	}
	var got schema.EnrollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Token, schema.EnrolledTokenPrefix) || got.Login != "alice" ||
		got.ServerVersion != "v1.4.0" {
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

// Nobody the dashboard would turn away gets a token, and a request refused on
// its face never reaches GitHub.
func TestEnrolmentRefusesWhoeverTheOrgGateWould(t *testing.T) {
	for _, tc := range []struct {
		name        string
		verifier    *fakeGitHub
		contentType string
		bearer      string
		want        int
		asksGitHub  bool
	}{
		{"not a member", &fakeGitHub{}, "application/json", ghToken, http.StatusForbidden, true},
		{"token GitHub rejects", &fakeGitHub{member: true}, "application/json", "gho_expired", http.StatusUnauthorized, true},
		{"GitHub unreachable", &fakeGitHub{err: errors.New("timeout")}, "application/json", ghToken, http.StatusBadGateway, true},
		{"no token", &fakeGitHub{member: true}, "application/json", "", http.StatusUnauthorized, false},
		{"form post", &fakeGitHub{member: true}, "application/x-www-form-urlencoded", ghToken, http.StatusUnsupportedMediaType, false},
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
	for _, v := range []*fakeGitHub{{member: true}, {}, {err: errors.New("github returned 502")}} {
		s := newServer(t)
		s.Log = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
		s.Enroll = v
		enroll(s, "application/json", ghToken)
	}
	if logged.Len() == 0 {
		t.Fatal("nothing was logged, so this proves nothing")
	}
	if strings.Contains(logged.String(), ghToken) {
		t.Fatalf("the GitHub token reached the log:\n%s", logged.String())
	}
}

func TestEnrolmentIsRateLimitedBeforeGitHubIsAsked(t *testing.T) {
	s := newServer(t)
	v := &fakeGitHub{}
	s.Enroll = v
	for range maxEnrolments {
		if rec := enroll(s, "application/json", ghToken); rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d within the limit", rec.Code)
		}
	}
	rec := enroll(s, "application/json", ghToken)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d past the limit, want 429 with Retry-After", rec.Code)
	}
	if v.calls != maxEnrolments {
		t.Fatalf("GitHub asked %d times, want %d", v.calls, maxEnrolments)
	}
}

func TestLimiterAdmitsTheLimitThenResetsWithTheNextWindow(t *testing.T) {
	var w limiter
	t0 := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	for i := range 2 {
		if !w.allow(t0.Add(time.Duration(i)*time.Second), 2, time.Minute) {
			t.Fatalf("event %d refused inside the limit", i)
		}
	}
	if w.allow(t0.Add(59*time.Second), 2, time.Minute) {
		t.Fatal("a third event fit a window of two")
	}
	if !w.allow(t0.Add(time.Minute), 2, time.Minute) {
		t.Fatal("the next window did not start empty")
	}
}
