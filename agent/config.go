package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Config struct {
	ServerURL     string `json:"server_url"`
	Email         string `json:"email"`
	DeviceID      string `json:"device_id"`
	DeviceToken   string `json:"device_token"`
	DeviceName    string `json:"device_name"`
	DeviceOS      string `json:"device_os"`
	LocalAPIPort  int    `json:"local_api_port"`
	LocalAPIToken string `json:"local_api_token"`
	SendDir       string `json:"send_dir"`
	ReceivedDir   string `json:"received_dir"`
	SendKeep      bool   `json:"send_keep"` // keep files in place; resend on change
	// DefaultTTLS is how long items wait on the server for offline devices.
	// Clamped to 1 minute .. 30 days. 0 (old configs) means 30 minutes.
	DefaultTTLS int `json:"default_ttl_s"`
}

// TTL returns the effective retention in seconds.
func (c *Config) TTL() int {
	if c.DefaultTTLS <= 0 {
		return 1800
	}
	if c.DefaultTTLS < 60 {
		return 60
	}
	if c.DefaultTTLS > 2592000 {
		return 2592000
	}
	return c.DefaultTTLS
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return createDefaultConfig(path)
		}
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	// Migrate pre-keep configs: never move user files away unless they
	// explicitly chose --move. Absence of the key means "old default".
	if !strings.Contains(string(b), `"send_keep"`) {
		cfg.SendKeep = true
		saveConfig(&cfg, path)
	}
	return &cfg, nil
}

func saveConfig(cfg *Config, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0600)
}

func createDefaultConfig(path string) (*Config, error) {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "Unknown Device"
	}

	tokenBytes := make([]byte, 16)
	rand.Read(tokenBytes)
	localAPIToken := hex.EncodeToString(tokenBytes)

	home, _ := os.UserHomeDir()

	// New installs share via Documents; existing configs keep their folders.
	docBase := filepath.Join(home, "Documents", "CrossShare")
	cfg := &Config{
		ServerURL:     "http://localhost:8080",
		DeviceID:      "",
		DeviceToken:   "",
		DeviceName:    hostname,
		DeviceOS:      runtime.GOOS,
		LocalAPIPort:  9876,
		LocalAPIToken: localAPIToken,
		SendDir:       filepath.Join(docBase, "send"),
		ReceivedDir:   filepath.Join(docBase, "received"),
		SendKeep:      true, // never move user files away by default
	}

	err := saveConfig(cfg, path)
	return cfg, err
}
