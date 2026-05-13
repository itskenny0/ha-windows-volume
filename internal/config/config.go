// Package config persists user state (HA URL, tokens, entity overrides, prefs)
// to %APPDATA%\HAVolume\config.json.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Config is the on-disk shape. Keep field names stable; add new ones with
// omitempty so older configs still load.
type Config struct {
	HomeAssistantURL string `json:"home_assistant_url,omitempty"`

	// RefreshToken is the HA long-lived refresh token issued by the OAuth
	// flow. We exchange it for short-lived access tokens at runtime.
	RefreshToken string `json:"refresh_token,omitempty"`

	// ClientID is the loopback URL we presented during the OAuth flow.
	// HA validates that refresh requests use the same client_id.
	ClientID string `json:"client_id,omitempty"`

	// EntityVolume / EntityMuted are the HA entity_ids we read+write.
	// Defaults are derived from hostname on first run.
	EntityVolume string `json:"entity_volume,omitempty"`
	EntityMuted  string `json:"entity_muted,omitempty"`

	// Step is the slider step in HA (1..50). Cosmetic only.
	Step int `json:"step,omitempty"`

	// RunAtStartup mirrors the registry Run-key state.
	RunAtStartup bool `json:"run_at_startup,omitempty"`
}

// Defaults fills in zero-valued fields.
func (c *Config) Defaults() {
	if c.Step <= 0 {
		c.Step = 1
	}
}

// Path returns the absolute config-file path; the parent dir is created.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Dir returns the per-user config directory. On Windows that's
// %APPDATA%\HAVolume; on other platforms (we cross-compile to Windows only,
// but tests run on Linux) it falls back to ~/.config/ha-volume.
func Dir() (string, error) {
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		return filepath.Join(appdata, "HAVolume"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "ha-volume"), nil
}

var saveMu sync.Mutex

// Load reads config from disk. A missing file returns an empty Config and
// no error so first-run code can detect onboarding via `c.HomeAssistantURL == ""`.
func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c := &Config{}
			c.Defaults()
			return c, nil
		}
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	c.Defaults()
	return &c, nil
}

// Save writes config atomically (temp file + rename).
func Save(c *Config) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	p, err := Path()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
