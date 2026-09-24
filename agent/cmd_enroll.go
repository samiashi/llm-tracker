package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/samiashi/llm-tracker/agent/internal/config"
	"github.com/samiashi/llm-tracker/agent/internal/tracker"
	"github.com/samiashi/llm-tracker/schema"
)

// cmdEnroll joins this machine to the team's tracker with nothing typed. The
// GitHub CLI's token goes to the server, which checks org membership -- the
// test that gates the dashboard -- and issues an ingest token of this
// machine's own.
//
// The server spends the GitHub token on that check and keeps nothing of it;
// the token it issues can only upload, and its -revoke withdraws it.
func cmdEnroll(dataDir, server string, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	base, err := serverURL(server)
	if err != nil {
		return err
	}
	// Refused before gh is asked for anything.
	if err := tracker.RequireTLS(base); err != nil {
		return err
	}

	// A re-run installer finds this machine enrolled already, and leaves a
	// token that still works alone rather than minting another.
	cfg, err := config.Load(dataDir)
	if err != nil {
		return err
	}
	if cfg.ServerURL == base && tracker.New(base, cfg.Token, version, log).Check(ctx) == nil {
		fmt.Println("already enrolled with", base)
		return nil
	}

	ghToken, err := githubToken(ctx)
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	res, err := tracker.Enroll(ctx, base, ghToken, schema.EnrollRequest{Hostname: hostname})
	if err != nil {
		return fmt.Errorf("enrolling with %s: %w", base, err)
	}

	// Checked before it is saved: the daemon re-reads the file every pass, so
	// a token saved unchecked would stop a working agent uploading.
	if err := tracker.New(base, res.Token, version, log).Check(ctx); err != nil {
		return fmt.Errorf("%s issued a token but refused an upload with it: %w; nothing was saved", base, err)
	}
	if err := config.Save(dataDir, config.Config{ServerURL: base, Token: res.Token}); err != nil {
		return err
	}
	fmt.Println("enrolled as", res.Login, "with", base)
	return nil
}

// serverURL validates a server address and returns it in the form sync
// appends /v1/ingest to.
func serverURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", fmt.Errorf("server URL %q is not an http:// or https:// address with a host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("server URL %q carries a query or fragment; give the tracker's own address", raw)
	}
	// A trailing slash would make every upload a POST to //v1/ingest.
	return strings.TrimRight(raw, "/"), nil
}

// githubToken returns the token the GitHub CLI holds for github.com.
func githubToken(ctx context.Context) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", errors.New("enrolling needs the GitHub CLI, signed in: " +
			"brew install gh && gh auth login")
	}
	c := ghCommand(ctx, "auth", "token", "--hostname", "github.com")
	var stderr strings.Builder
	c.Stderr = &stderr
	out, err := c.Output()
	token := strings.TrimSpace(string(out))
	if err != nil || token == "" {
		// gh's own words say which: not signed in, or a gh too old to have
		// `auth token` (it arrived in 2.17).
		return "", fmt.Errorf("could not read gh's token for github.com (%s): "+
			"run `gh auth login`, or upgrade gh, then retry", strings.TrimSpace(stderr.String()))
	}
	return token, nil
}
