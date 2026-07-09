package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
)

// Config holds user-settable knobs persisted at
// $XDG_CONFIG_HOME/collectif/config.json (or ~/.config/collectif/config.json).
// Additive: leave room to grow — other issues will add fields here.
type Config struct {
	// #35 hourly dollar cap across all sessions. 0 = no cap.
	CostCapHourUSD float64 `json:"cost_cap_hour_usd"`
	// #36 outbound notification webhook — POSTed on the same status
	// transitions the browser gets notified about. Empty = disabled.
	NotifyWebhookURL string `json:"notify_webhook_url"`
}

var currentConfig atomic.Pointer[Config]

func configFilePath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "collectif", "config.json"), nil
}

// LoadConfig reads config.json from disk. Missing file → zero-value.
// Malformed → log and return zero-value (we never want a bad config
// file to prevent the server from starting).
func LoadConfig() Config {
	var zero Config
	p, err := configFilePath()
	if err != nil {
		return zero
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("config: read %s: %v", p, err)
		}
		return zero
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		log.Printf("config: parse %s: %v (using defaults)", p, err)
		return zero
	}
	return c
}

// SaveConfig writes config.json with 0o600 perms.
func SaveConfig(c Config) error {
	p, err := configFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// GetConfig returns the current in-memory config snapshot. Never nil
// once main() has called initConfig.
func GetConfig() *Config {
	c := currentConfig.Load()
	if c == nil {
		// Fallback to zero-value if not yet initialized (e.g. in tests).
		z := Config{}
		return &z
	}
	return c
}

// initConfig loads once at startup and publishes into the atomic pointer.
func initConfig() {
	c := LoadConfig()
	currentConfig.Store(&c)
}

// handleConfig — POST /api/config accepts a JSON body and persists it.
// GET returns the current in-memory snapshot.
func handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, GetConfig())
	case http.MethodPost:
		var c Config
		if !decodeBody(w, r, &c) {
			return
		}
		if err := SaveConfig(c); err != nil {
			http.Error(w, "save: "+err.Error(), http.StatusInternalServerError)
			return
		}
		currentConfig.Store(&c)
		writeJSON(w, http.StatusOK, &c)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
