package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// Enroll asks the tracker at baseURL for an ingest token of this machine's
// own, proving who is asking with githubToken.
//
// That token opens everything its owner can reach on GitHub, so it goes to
// baseURL and nowhere else: never in the clear (RequireTLS), and never on
// through a redirect, which would hand it to whatever the redirect names.
func Enroll(ctx context.Context, baseURL, githubToken string, in schema.EnrollRequest) (schema.EnrollResponse, error) {
	var out schema.EnrollResponse
	if err := RequireTLS(baseURL); err != nil {
		return out, err
	}
	body, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/enroll", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+githubToken)

	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if err := refusal(resp, "the GitHub token goes to the tracker's own address and no further"); err != nil {
		return out, err
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil ||
		!strings.HasPrefix(out.Token, schema.EnrolledTokenPrefix) || out.Login == "" {
		return schema.EnrollResponse{}, fmt.Errorf("%s did not answer with an enrolment "+
			"(is the server URL right?)", baseURL)
	}
	return out, nil
}

// RequireTLS refuses an address a credential would cross the network to in
// the clear. Plain http is allowed only to loopback: a server on this machine,
// where there is no network to cross.
func RequireTLS(baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return err
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if u.Scheme == "https" || (u.Scheme == "http" && (host == "localhost" || ip != nil && ip.IsLoopback())) {
		return nil
	}
	return fmt.Errorf("refusing to send a GitHub token to %s: enrolment needs an https:// address", baseURL)
}
