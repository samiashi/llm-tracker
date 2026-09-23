package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/samiashi/llm-tracker/agent/internal/launchd"
	"github.com/samiashi/llm-tracker/schema"
)

// cmdUpgrade replaces this binary with the latest release, then refreshes the
// copy launchd starts and restarts the collector on its own data directory.
//
// It shells out to `gh`, which already holds the credential for the private
// repository, honours SSO and refreshes its own token; the alternative is a
// PAT on every laptop. The download lands beside the running binary and is
// renamed over it, so nothing ever starts a truncated one.
func cmdUpgrade(log *slog.Logger, dataDir string, force bool) error {
	// Bounded: `gh` can hang on a network stall, and someone is waiting.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("upgrading needs the GitHub CLI for a private repository: " +
			"install it with `brew install gh`, run `gh auth login`, then retry")
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}

	asset := fmt.Sprintf("llm-tracker-agent-%s-%s", runtime.GOOS, runtime.GOARCH)
	dir := filepath.Dir(self)
	staged := filepath.Join(dir, "."+filepath.Base(self)+".new")
	// Best-effort: on the success path the rename has already consumed it.
	defer func() { _ = os.Remove(staged) }()

	log.Info("downloading latest release", "asset", asset, "into", dir)
	// The constant upgradeRepo pins the path, not the host: gh takes GH_HOST
	// from the environment, and anything that set it would choose the server.
	ghEnv := append(os.Environ(), "GH_HOST=github.com")

	download := func(pattern, dest string) error {
		c := exec.CommandContext(ctx, "gh", "release", "download",
			"--repo", upgradeRepo, "--pattern", pattern, "--output", dest, "--clobber")
		c.Env = ghEnv
		c.Stderr = os.Stderr
		return c.Run()
	}

	if err := download(asset, staged); err != nil {
		return fmt.Errorf("gh release download: %w", err)
	}

	// Verified before it is executed at all: running it is how a compromised
	// release would act as the developer, even on the path that rejects it.
	sums := staged + ".sha256"
	defer os.Remove(sums) //nolint:errcheck // best-effort cleanup of a temp file
	if err := download(asset+".sha256", sums); err != nil {
		return fmt.Errorf("no published checksum for %s: %w "+
			"(refusing to install an unverified binary)", asset, err)
	}
	if err := verifySHA256(staged, sums); err != nil {
		return fmt.Errorf("refusing to install %s: %w", asset, err)
	}

	//nolint:gosec // G302: a binary we are about to exec must be executable.
	if err := os.Chmod(staged, 0o755); err != nil {
		return err
	}

	// It must also run here before launchd is given it: a wrong-architecture
	// asset leaves a collector that launchd can never start.
	out, err := exec.CommandContext(ctx, staged, "version").Output()
	if err != nil {
		return fmt.Errorf("the downloaded binary does not run here: %w", err)
	}
	newVersion := strings.TrimSpace(string(out))
	replace, err := upgradeVerdict(version, newVersion, force)
	if err != nil {
		return err
	}
	if !replace {
		fmt.Println("already on the latest release,", version)
		return nil
	}

	if err := os.Rename(staged, self); err != nil {
		return fmt.Errorf("replacing %s: %w", self, err)
	}
	fmt.Printf("upgraded %s from %s to %s\n", self, version, newVersion)

	// The daemon runs its own copy in its data directory, so replacing the
	// binary in hand does nothing for it until that copy is refreshed.
	if !launchd.Loaded() {
		fmt.Println("the collector is not running; to run it on this release:", agentCmd(dataDir, "install"))
		return nil
	}
	installed, err := installBinary(self, dataDir)
	if err != nil {
		return fmt.Errorf("refreshing the installed binary: %w", err)
	}
	if _, err := launchd.Install(installed, dataDir); err != nil {
		return fmt.Errorf("restarting the collector: %w", err)
	}
	fmt.Println("collector restarted on", newVersion, "with its data in", dataDir)
	return nil
}

// upgradeVerdict says whether to replace the running build with the latest
// release. It never moves a release backwards without force: GitHub's latest
// is the release published last, so a backport tagged after a newer release
// would otherwise downgrade everyone who ran upgrade.
func upgradeVerdict(running, latest string, force bool) (replace bool, err error) {
	if latest == running {
		return false, nil
	}
	if force {
		return true, nil
	}
	have, ok := schema.ReleaseVersion(running)
	if !ok {
		// dev or <sha>-dirty: nothing to compare, and any release is the way
		// back to one.
		return true, nil
	}
	want, ok := schema.ReleaseVersion(latest)
	if !ok {
		return false, fmt.Errorf("the latest release reports version %q, which is not vX.Y.Z; "+
			"pass -force to install it anyway", latest)
	}
	switch c := slices.Compare(want[:], have[:]); {
	case c < 0:
		return false, fmt.Errorf("the latest release, %s, is older than this build, %s; "+
			"pass -force to install it anyway", latest, running)
	case c == 0:
		return false, nil
	}
	return true, nil
}

// verifySHA256 checks a downloaded file against a shasum-format digest file.
//
// The digest file is `<hex>  <name>`, as `shasum -a 256` writes it. Only the
// hex is used: the name in it is the release's, not the staged temp path.
func verifySHA256(path, sumFile string) error {
	raw, err := os.ReadFile(sumFile) //nolint:gosec // path is derived, not user input
	if err != nil {
		return fmt.Errorf("reading checksum: %w", err)
	}
	want, _, _ := strings.Cut(strings.TrimSpace(string(raw)), " ")
	if len(want) != sha256.Size*2 {
		return fmt.Errorf("checksum file is not a sha256 digest")
	}

	f, err := os.Open(path) //nolint:gosec // path is derived, not user input
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !hmac.Equal([]byte(got), []byte(want)) {
		return fmt.Errorf("checksum mismatch: downloaded %s, published %s", got, want)
	}
	return nil
}
