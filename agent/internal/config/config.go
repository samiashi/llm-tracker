// Package config holds the agent's on-disk settings.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config is what enroll saves.
type Config struct {
	// ServerURL is the team server. Empty means not yet enrolled: the agent
	// still builds its local archive.
	ServerURL string `json:"server_url"`
	Token     string `json:"token,omitempty"`
}

func Path(dataDir string) string { return filepath.Join(dataDir, "config.json") }

func Load(dataDir string) (Config, error) {
	var c Config
	b, err := os.ReadFile(Path(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	err = json.Unmarshal(b, &c)
	return c, err
}

func Save(dataDir string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// The token is a credential, so the file is owner-only.
	return os.WriteFile(Path(dataDir), append(b, '\n'), 0o600)
}
